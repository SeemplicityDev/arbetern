package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	gh "github.com/google/go-github/v91/github"
)

// ErrMergeableNow means GitHub would not turn auto-merge on because the pull request can be merged now.
var ErrMergeableNow = errors.New("the pull request can be merged now")

var graphQLMergeMethods = map[string]string{"squash": "SQUASH", "rebase": "REBASE", "merge": "MERGE"}

const maxCompareFiles = 300

type refusal struct{ op, message string }

func (e *refusal) Error() string { return e.op + ": " + e.message }

// APIMessage is the message GitHub answered err with, or err itself when it sent none.
func APIMessage(err error) string {
	var rf *refusal
	if errors.As(err, &rf) {
		return rf.message
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Message != "" {
		return ghErr.Message
	}
	return err.Error()
}

// EnablePullRequestAutoMerge turns on auto-merge with method (squash, rebase or merge) while the head is still expectedHead.
func (c *Client) EnablePullRequestAutoMerge(ctx context.Context, nodeID, method, expectedHead string) error {
	m, ok := graphQLMergeMethods[method]
	if !ok {
		return fmt.Errorf("unknown merge method %q", method)
	}
	mutation := `mutation($id:ID!,$method:PullRequestMergeMethod!,$head:GitObjectID!){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:$method,expectedHeadOid:$head}){pullRequest{number}}}`
	err := c.mutate(ctx, "enablePullRequestAutoMerge", mutation, map[string]any{"id": nodeID, "method": m, "head": expectedHead})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "clean status") {
		return fmt.Errorf("%w: %v", ErrMergeableNow, err)
	}
	return err
}

// DisablePullRequestAutoMerge turns auto-merge off.
func (c *Client) DisablePullRequestAutoMerge(ctx context.Context, nodeID string) error {
	mutation := `mutation($id:ID!){disablePullRequestAutoMerge(input:{pullRequestId:$id}){pullRequest{number}}}`
	return c.mutate(ctx, "disablePullRequestAutoMerge", mutation, map[string]any{"id": nodeID})
}

// AutoMergeDisabledBy returns the user who last turned auto-merge off by hand, or "" when an app or GitHub itself did, or nobody has.
func (c *Client) AutoMergeDisabledBy(ctx context.Context, nodeID string) (string, error) {
	if nodeID == "" {
		return "", errors.New("the pull request has no node id")
	}
	var resp struct {
		Data struct {
			Node struct {
				TimelineItems struct {
					Nodes []struct {
						ReasonCode string `json:"reasonCode"`
						Disabler   *struct {
							Type  string `json:"__typename"`
							Login string `json:"login"`
						} `json:"disabler"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	query := `query($id:ID!){node(id:$id){... on PullRequest{timelineItems(last:1,itemTypes:[AUTO_MERGE_DISABLED_EVENT]){nodes{... on AutoMergeDisabledEvent{reasonCode disabler{__typename login}}}}}}}`
	if err := c.graphql(ctx, query, map[string]any{"id": nodeID}, &resp); err != nil {
		return "", fmt.Errorf("autoMergeDisabledEvent: %w", err)
	}
	if len(resp.Errors) > 0 {
		return "", &refusal{"autoMergeDisabledEvent", resp.Errors[0].Message}
	}
	nodes := resp.Data.Node.TimelineItems.Nodes
	if len(nodes) == 0 || nodes[0].Disabler == nil || nodes[0].Disabler.Type != "User" || !strings.EqualFold(nodes[0].ReasonCode, "manually_disabled") {
		return "", nil
	}
	return nodes[0].Disabler.Login, nil
}

// MergePullRequest merges a pull request with method while its head is still sha.
func (c *Client) MergePullRequest(ctx context.Context, owner, repo string, number int, sha, method string) error {
	if _, _, err := c.api.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{SHA: sha, MergeMethod: method}); err != nil {
		return fmt.Errorf("failed to merge PR #%d: %w", number, err)
	}
	return nil
}

// UpdatePullRequestBranch merges the base branch into a pull request whose head is still expectedHead.
func (c *Client) UpdatePullRequestBranch(ctx context.Context, owner, repo string, number int, expectedHead string) error {
	_, _, err := c.api.PullRequests.UpdateBranch(ctx, owner, repo, number, &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: &expectedHead})
	var accepted *gh.AcceptedError
	if err != nil && !errors.As(err, &accepted) {
		return fmt.Errorf("failed to update the branch of PR #%d: %w", number, err)
	}
	return nil
}

// ReviewDecision returns APPROVED, CHANGES_REQUESTED or REVIEW_REQUIRED, or "" when the branch rules require no review.
func (c *Client) ReviewDecision(ctx context.Context, nodeID string) (string, error) {
	if nodeID == "" {
		return "", errors.New("the pull request has no node id")
	}
	var resp struct {
		Data struct {
			Node struct {
				ReviewDecision string `json:"reviewDecision"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, `query($id:ID!){node(id:$id){... on PullRequest{reviewDecision}}}`, map[string]any{"id": nodeID}, &resp); err != nil {
		return "", fmt.Errorf("reviewDecision: %w", err)
	}
	if len(resp.Errors) > 0 {
		return "", &refusal{"reviewDecision", resp.Errors[0].Message}
	}
	return resp.Data.Node.ReviewDecision, nil
}

// CanPush reports whether user has write access to the repository.
func (c *Client) CanPush(ctx context.Context, owner, repo, user string) (bool, error) {
	lvl, _, err := c.api.Repositories.GetPermissionLevel(ctx, owner, repo, user)
	if err != nil {
		return false, fmt.Errorf("failed to read the permission of %s: %w", user, err)
	}
	p := lvl.GetPermission()
	return p == "admin" || p == "write", nil
}

// ChangeFingerprint hashes each run of lines head changes over its merge base with base, with the unchanged line right above and below it; merging base in cleanly cannot touch those.
func (c *Client) ChangeFingerprint(ctx context.Context, owner, repo, base, head string) (string, error) {
	cmp, _, err := c.api.Repositories.CompareCommits(ctx, owner, repo, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return "", fmt.Errorf("failed to compare %s...%s: %w", base, head, err)
	}
	files := slices.Clone(cmp.Files)
	slices.SortFunc(files, func(a, b *gh.CommitFile) int { return strings.Compare(a.GetFilename(), b.GetFilename()) })
	var b strings.Builder
	if len(files) >= maxCompareFiles {
		// GitHub lists only the first files of a larger comparison, so what the rest change is unknown.
		writeField(&b, head)
	}
	for _, f := range files {
		writeField(&b, f.GetFilename())
		writeField(&b, f.GetPreviousFilename())
		writeField(&b, f.GetStatus())
		if f.GetPatch() == "" {
			fmt.Fprintf(&b, "+%d-%d;", f.GetAdditions(), f.GetDeletions())
			writeField(&b, f.GetSHA())
			continue
		}
		writeChangeRuns(&b, f.GetPatch())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func writeField(b *strings.Builder, s string) { fmt.Fprintf(b, "%d:%s", len(s), s) }

func writeChangeRuns(b *strings.Builder, patch string) {
	var above string
	var run []string
	flush := func(below string) {
		if len(run) == 0 {
			return
		}
		writeField(b, above)
		fmt.Fprintf(b, "%d;", len(run))
		for _, line := range run {
			writeField(b, line)
		}
		writeField(b, below)
		run = nil
	}
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			flush("")
			above = ""
		case strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "\\") && len(run) > 0:
			run = append(run, line)
		default:
			flush(line)
			above = line
		}
	}
	flush("")
}

// BaseMerge reports whether sha is a merge GitHub itself made of a commit already on base into sha's first parent, which it returns.
func (c *Client) BaseMerge(ctx context.Context, owner, repo, base, sha string) (string, bool, error) {
	cm, _, err := c.api.Git.GetCommit(ctx, owner, repo, sha)
	if err != nil {
		return "", false, fmt.Errorf("failed to read commit %s: %w", sha, err)
	}
	if len(cm.Parents) != 2 || !strings.EqualFold(cm.GetCommitter().GetEmail(), "noreply@github.com") || !cm.GetVerification().GetVerified() {
		return "", false, nil
	}
	ahead, err := c.CommitsAhead(ctx, owner, repo, base, cm.Parents[1].GetSHA())
	if err != nil {
		return "", false, err
	}
	return cm.Parents[0].GetSHA(), ahead == 0, nil
}

func (c *Client) mutate(ctx context.Context, op, mutation string, vars map[string]any) error {
	if id, _ := vars["id"].(string); id == "" {
		return errors.New("the pull request has no node id")
	}
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, mutation, vars, &resp); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if len(resp.Errors) > 0 {
		return &refusal{op, resp.Errors[0].Message}
	}
	return nil
}
