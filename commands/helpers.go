package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/slack"
)

// stripWhitespace removes every whitespace character from s so two strings
// that differ only in whitespace (trailing newline, blank lines, indentation)
// compare equal. Used to detect no-op / whitespace-only code edits.
func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// buildPRBody returns the PR body for a write-tool call. When the caller
// supplied pr_body (trimmed non-empty) we use it verbatim and append a
// one-line Slack attribution footer so reviewers still know which user /
// workflow originated the change. Otherwise we fall back to the generic
// template the tool historically used.
func buildPRBody(userID, supplied, fallback string) string {
	supplied = strings.TrimSpace(supplied)
	if supplied == "" {
		return fallback
	}
	return supplied + "\n\n---\n_Automated via Slack by <@" + userID + ">_"
}

// parseToolArgs unmarshals a JSON string into the target struct.
// Returns a user-facing error string if parsing fails.
func parseToolArgs[T any](argsJSON string) (T, string) {
	var args T
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return args, preconditionErrf("Error parsing arguments: %v", err)
	}
	return args, ""
}

// requireReady checks that an OAuthClient is non-nil and ready.
// Returns a user-facing error string if the client is not available.
func requireReady(name string, c OAuthClient) string {
	if c == nil || !c.Ready() {
		return preconditionErrf("Error: %s integration is not connected. It may still be initializing — please try again shortly.", name)
	}
	return ""
}

// preconditionErrPrefix tags tool-result strings whose error happened
// BEFORE any mutating side effect was attempted (missing/invalid args,
// content guards, regex compile failures, "old_content not found", etc.).
//
// These rejections are recoverable: the LLM sees the error, fixes its
// arguments, and retries on the next round. They are NOT counted as
// mutating-tool failures by the workflow tick loop, because no PR was
// opened, no Slack message was sent, no dashboard was written — nothing
// actually happened externally. The prefix uses non-printable bytes so
// it's invisible if it ever leaks through to the LLM unstripped.
const preconditionErrPrefix = "\x00\x00ARBETERN_PRECONDITION\x00\x00"

// preconditionErrf wraps a formatted error string with the precondition
// sentinel so the workflow loop can recognise it as "no side effect was
// attempted". The sentinel is stripped before the message is handed to
// the LLM.
func preconditionErrf(format string, a ...any) string {
	return preconditionErrPrefix + fmt.Sprintf(format, a...)
}

// isPreconditionErr reports whether a tool result is a precondition
// rejection (no side effect attempted).
func isPreconditionErr(s string) bool {
	return strings.HasPrefix(s, preconditionErrPrefix)
}

// stripPreconditionPrefix removes the internal sentinel so the tool
// result is safe to pass back to the LLM.
func stripPreconditionPrefix(s string) string {
	return strings.TrimPrefix(s, preconditionErrPrefix)
}

// agentPatchBranchRE matches branches produced by github.GenerateBranchName
// ("<agent>/patch-<unix-timestamp>"). We silently reject these as a PR base
// because they are transient auto-generated head branches from a prior tick —
// the LLM sometimes copies one from list_pull_requests into the `branch`
// argument of modify_file / create_file, which would open the new PR against
// the old workflow branch instead of the repo's default branch (main/master).
var agentPatchBranchRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*/patch-[0-9]+$`)

// resolveRepoBranch resolves the GitHub owner and fills in the default branch
// when branch is empty. It also silently replaces obviously-wrong bases
// (auto-generated <agent>/patch-<ts> head branches) with the repo default so a
// confused LLM cannot open a PR that targets a stale workflow branch.
func resolveRepoBranch(ctx context.Context, ghClient *github.Client, repo, branch string) (owner, resolvedBranch string, errMsg string) {
	owner, err := ghClient.ResolveOwner(ctx)
	if err != nil {
		return "", "", preconditionErrf("Error resolving owner: %v", err)
	}
	if branch != "" && agentPatchBranchRE.MatchString(branch) {
		log.Printf("[helpers] ignoring auto-generated agent branch %q as base for %s/%s; falling back to default branch",
			branch, owner, repo)
		branch = ""
	}
	if branch == "" {
		branch, err = ghClient.GetDefaultBranch(ctx, owner, repo)
		if err != nil {
			return "", "", preconditionErrf("Error getting default branch: %v", err)
		}
	}
	return owner, branch, ""
}

// replyOrThread posts a message either as a thread reply (if auditTS is set)
// or via the Slack response URL. Used by both DebugHandler and GeneralHandler.
func replyOrThread(slackClient SlackClient, channelID, responseURL, auditTS, text string) {
	if auditTS != "" {
		if err := slackClient.PostThreadReply(channelID, auditTS, text); err != nil {
			log.Printf("[channel=%s] failed to post thread reply: %v", channelID, err)
		}
		return
	}
	if err := slack.RespondToURL(responseURL, text, false); err != nil {
		log.Printf("[channel=%s] failed to respond: %v", channelID, err)
	}
}

// fetchWorkflowLogsBulk fetches GitHub Actions workflow run summaries for all
// workflow URLs found in the given text. Shared by DebugHandler and GeneralHandler.
func fetchWorkflowLogsBulk(ctx context.Context, ghClient *github.Client, text, userID, channelID string) string {
	urls := github.ExtractWorkflowRunURLs(text)
	if len(urls) == 0 {
		return ""
	}

	seen := make(map[string]bool)
	var result string
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true

		owner, repo, runID, err := github.ParseWorkflowRunURL(u)
		if err != nil {
			continue
		}

		log.Printf("[user=%s channel=%s] fetching workflow run %s/%s/%d", userID, channelID, owner, repo, runID)
		summary, err := ghClient.GetWorkflowRunSummary(ctx, owner, repo, runID)
		if err != nil {
			log.Printf("[user=%s channel=%s] failed to fetch workflow run summary: %v", userID, channelID, err)
			continue
		}

		result += github.FormatWorkflowRunSummary(summary)
	}
	return result
}

// httpGetClient is shared by http_get.
var httpGetClient = &http.Client{
	Timeout: 25 * time.Second,
	// Re-validate on redirect to mitigate SSRF via 30x.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return validatePublicURL(req.URL.String())
	},
}

// validatePublicURL rejects non-http(s) schemes and obvious internal
// targets. No DNS resolution — hostname-level checks only.
func validatePublicURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("only http(s) URLs are allowed (got %q)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("url is missing a host")
	}
	switch host {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return fmt.Errorf("refusing to fetch from %s", host)
	}
	if strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("refusing to fetch from internal host %s", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("refusing to fetch from non-public IP %s", host)
		}
	}
	return nil
}

// doHTTPGet implements the http_get tool. Errors are returned inline so
// the LLM can react and retry.
func doHTTPGet(ctx context.Context, rawURL, accept string, maxBytes int, userID, channelID string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "Error: url is required."
	}
	if err := validatePublicURL(rawURL); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if maxBytes <= 0 {
		maxBytes = 200_000
	}
	if maxBytes > 1_048_576 {
		maxBytes = 1_048_576
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Sprintf("Error building request: %v", err)
	}
	req.Header.Set("User-Agent", "arbetern-http-get/1.0")
	if strings.TrimSpace(accept) == "" {
		accept = "application/json, application/yaml, text/yaml, text/plain, */*"
	}
	req.Header.Set("Accept", accept)

	resp, err := httpGetClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Error fetching %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if rerr != nil {
		return fmt.Sprintf("Error reading response from %s: %v", rawURL, rerr)
	}
	truncated := false
	if len(body) > maxBytes {
		body = body[:maxBytes]
		truncated = true
	}
	log.Printf("[user=%s channel=%s] http_get %s -> HTTP %d (%d bytes%s)",
		userID, channelID, rawURL, resp.StatusCode, len(body), map[bool]string{true: ", truncated", false: ""}[truncated])

	header := fmt.Sprintf("GET %s\nStatus: %d %s\nContent-Type: %s\nBody bytes: %d%s\n---\n",
		rawURL,
		resp.StatusCode, http.StatusText(resp.StatusCode),
		resp.Header.Get("Content-Type"),
		len(body),
		map[bool]string{true: " (truncated — re-request with a more specific URL or higher max_bytes if needed)", false: ""}[truncated],
	)
	return header + string(body)
}
