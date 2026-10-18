package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v91/github"
)

// ErrMergeableNow means GitHub would not turn auto-merge on because the pull request can be merged now.
var ErrMergeableNow = errors.New("the pull request can be merged now")

var graphQLMergeMethods = map[string]string{"squash": "SQUASH", "rebase": "REBASE", "merge": "MERGE"}

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

// CommitParents returns a commit's parents, first parent first, and whether GitHub itself committed and signed it.
func (c *Client) CommitParents(ctx context.Context, owner, repo, sha string) (parents []string, byGitHub bool, err error) {
	cm, _, err := c.api.Git.GetCommit(ctx, owner, repo, sha)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read commit %s: %w", sha, err)
	}
	for _, p := range cm.Parents {
		parents = append(parents, p.GetSHA())
	}
	return parents, strings.EqualFold(cm.GetCommitter().GetEmail(), "noreply@github.com") && cm.GetVerification().GetVerified(), nil
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
