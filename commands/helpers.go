package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/slack"
)

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

// resolveRepoBranch resolves the GitHub owner and fills in the default branch
// when branch is empty.
func resolveRepoBranch(ctx context.Context, ghClient *github.Client, repo, branch string) (owner, resolvedBranch string, errMsg string) {
	owner, err := ghClient.ResolveOwner(ctx)
	if err != nil {
		return "", "", fmt.Sprintf("Error resolving owner: %v", err)
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
