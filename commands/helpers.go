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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/slack"
	"gopkg.in/yaml.v3"
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
		return args, fmt.Sprintf("Error parsing arguments: %v", err)
	}
	return args, ""
}

// requireReady checks that an OAuthClient is non-nil and ready.
// Returns a user-facing error string if the client is not available.
func requireReady(name string, c OAuthClient) string {
	if c == nil || !c.Ready() {
		return fmt.Sprintf("Error: %s integration is not connected. It may still be initializing — please try again shortly.", name)
	}
	return ""
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
		return "", "", fmt.Sprintf("Error resolving owner: %v", err)
	}
	if branch != "" && agentPatchBranchRE.MatchString(branch) {
		log.Printf("[helpers] ignoring auto-generated agent branch %q as base for %s/%s; falling back to default branch",
			branch, owner, repo)
		branch = ""
	}
	if branch == "" {
		branch, err = ghClient.GetDefaultBranch(ctx, owner, repo)
		if err != nil {
			return "", "", fmt.Sprintf("Error getting default branch: %v", err)
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

// httpGetClient is a shared client for the http_get tool. Short timeout to
// avoid stalling a workflow tick on a slow upstream.
var httpGetClient = &http.Client{
	Timeout: 25 * time.Second,
	// Disallow redirects to private/loopback hosts (mitigates SSRF if a
	// crafted prompt tricks the model into pivoting through a redirect).
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return validatePublicURL(req.URL.String())
	},
}

// validatePublicURL rejects schemes other than http/https and obvious
// internal targets (localhost, link-local, private IPs by hostname). This is
// best-effort; net.LookupIP-based checks happen implicitly via the OS but we
// don't want to add network calls before the request itself.
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

// doHTTPGet implements the http_get tool. Returns a model-facing string with
// status, content-type, and (truncated) body. Errors are returned inline so
// the LLM can react and try a different URL.
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

// doHelmIndexLatest fetches a Helm repository's index.yaml, locates the
// requested chart, and returns the highest stable semver — without ever
// pasting the (often multi-MB) index back into the LLM. We stream the body
// up to a hard cap (16 MiB) and parse only the entries we need.
func doHelmIndexLatest(ctx context.Context, repoURL, chart, userID, channelID string) string {
	repoURL = strings.TrimSpace(repoURL)
	chart = strings.TrimSpace(chart)
	if repoURL == "" || chart == "" {
		return "Error: repo_url and chart are required."
	}
	idxURL := strings.TrimRight(repoURL, "/") + "/index.yaml"
	if err := validatePublicURL(idxURL); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, idxURL, nil)
	if err != nil {
		return fmt.Sprintf("Error building request: %v", err)
	}
	req.Header.Set("User-Agent", "arbetern-helm-index/1.0")
	req.Header.Set("Accept", "application/yaml, text/yaml, application/x-yaml, */*")

	resp, err := httpGetClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Error fetching %s: %v", idxURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Sprintf("Error: HTTP %d from %s — %s",
			resp.StatusCode, idxURL, strings.TrimSpace(string(preview)))
	}

	const maxIndexBytes = 16 * 1024 * 1024
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if rerr != nil {
		return fmt.Sprintf("Error reading %s: %v", idxURL, rerr)
	}
	if len(body) > maxIndexBytes {
		return fmt.Sprintf("Error: index.yaml at %s exceeded %d bytes", idxURL, maxIndexBytes)
	}

	var idx struct {
		Entries map[string][]struct {
			Version    string   `yaml:"version"`
			AppVersion string   `yaml:"appVersion"`
			URLs       []string `yaml:"urls"`
			Created    string   `yaml:"created"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return fmt.Sprintf("Error parsing index.yaml at %s: %v", idxURL, err)
	}
	entries, ok := idx.Entries[chart]
	if !ok || len(entries) == 0 {
		// Case-insensitive fallback.
		lc := strings.ToLower(chart)
		for k, v := range idx.Entries {
			if strings.ToLower(k) == lc {
				entries = v
				chart = k
				ok = true
				break
			}
		}
	}
	if !ok || len(entries) == 0 {
		// Suggest a few available chart names so the model can self-correct.
		names := make([]string, 0, len(idx.Entries))
		for k := range idx.Entries {
			names = append(names, k)
		}
		sort.Strings(names)
		preview := names
		if len(preview) > 25 {
			preview = append(preview[:25:25], "...")
		}
		return fmt.Sprintf("Error: chart %q not found in %s. Available entries (sample): %s",
			chart, idxURL, strings.Join(preview, ", "))
	}

	versions := make([]string, 0, len(entries))
	appByVer := make(map[string]string, len(entries))
	urlByVer := make(map[string]string, len(entries))
	for _, e := range entries {
		v := strings.TrimSpace(e.Version)
		if v == "" || isHelmPrerelease(v) {
			continue
		}
		versions = append(versions, v)
		if _, seen := appByVer[v]; !seen {
			appByVer[v] = e.AppVersion
			if len(e.URLs) > 0 {
				urlByVer[v] = e.URLs[0]
			}
		}
	}
	if len(versions) == 0 {
		return fmt.Sprintf("Error: no stable versions for %s in %s", chart, idxURL)
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareHelmSemver(versions[i], versions[j]) > 0
	})
	latest := versions[0]
	recent := versions
	truncated := false
	if len(recent) > 10 {
		recent = recent[:10]
		truncated = true
	}

	log.Printf("[user=%s channel=%s] helm_index_latest_version %s @ %s -> %s",
		userID, channelID, chart, repoURL, latest)

	out := fmt.Sprintf("Chart: %s\nRepo: %s\nLatest stable version: %s",
		chart, repoURL, latest)
	if av := appByVer[latest]; av != "" {
		out += fmt.Sprintf("\nApp version: %s", av)
	}
	if u := urlByVer[latest]; u != "" {
		out += fmt.Sprintf("\nChart URL: %s", u)
	}
	suffix := ""
	if truncated {
		suffix = " (showing 10 most recent)"
	}
	out += fmt.Sprintf("\nRecent stable versions%s: %s", suffix, strings.Join(recent, ", "))
	return out
}

// isHelmPrerelease returns true for any version that semver considers a
// pre-release (a "-" suffix in the version core). Build metadata after "+"
// is ignored.
func isHelmPrerelease(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	return strings.ContainsRune(v, '-')
}

// compareHelmSemver does a permissive component-wise semver compare.
// Returns >0 if a is newer, <0 if older, 0 if equal.
func compareHelmSemver(a, b string) int {
	an := normalizeHelmVer(a)
	bn := normalizeHelmVer(b)
	la := strings.FieldsFunc(an, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	lb := strings.FieldsFunc(bn, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	n := len(la)
	if len(lb) > n {
		n = len(lb)
	}
	for i := 0; i < n; i++ {
		var ai, bi string
		if i < len(la) {
			ai = la[i]
		}
		if i < len(lb) {
			bi = lb[i]
		}
		if ai == bi {
			continue
		}
		ax, aerr := strconv.Atoi(ai)
		bx, berr := strconv.Atoi(bi)
		if aerr == nil && berr == nil {
			if ax < bx {
				return -1
			}
			return 1
		}
		if ai == "" {
			return -1
		}
		if bi == "" {
			return 1
		}
		if ai < bi {
			return -1
		}
		return 1
	}
	return 0
}

func normalizeHelmVer(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	return v
}
