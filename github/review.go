package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v91/github"

	"github.com/justmike1/arbetern/internal/text"
)

// Kinds of DiscussionItem.
const (
	DiscussionReview  = "review"
	DiscussionInline  = "inline"
	DiscussionComment = "comment"
)

const (
	maxCheckPages  = 3
	maxAnnotations = 20
)

// DiscussionItem is one review, inline review comment or conversation comment on a pull request.
type DiscussionItem struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	Author      string    `json:"author"`
	Bot         bool      `json:"bot"`
	Association string    `json:"association"`
	State       string    `json:"state,omitempty"`
	Path        string    `json:"path,omitempty"`
	Line        int       `json:"line,omitempty"`
	InReplyTo   int64     `json:"in_reply_to,omitempty"`
	Body        string    `json:"body"`
	URL         string    `json:"url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Trusted reports whether the author owns the repository, is a member of its organization or a collaborator.
func (d DiscussionItem) Trusted() bool { return slices.Contains(trustedAssociations, d.Association) }

func isBotUser(u *gh.User) bool {
	login := strings.ToLower(u.GetLogin())
	return u.GetType() == "Bot" || strings.HasSuffix(login, "[bot]") || login == "copilot"
}

// PullRequestDiscussion returns the reviews, inline review comments and conversation comments on a pull request, oldest first within each kind.
func (c *Client) PullRequestDiscussion(ctx context.Context, owner, repo string, number int) ([]DiscussionItem, error) {
	var items []DiscussionItem
	add := func(id int64, kind string, user *gh.User, association, state, path string, line int, inReplyTo int64, body, url string, at gh.Timestamp) {
		items = append(items, DiscussionItem{
			ID:          id,
			Kind:        kind,
			Author:      user.GetLogin(),
			Bot:         isBotUser(user),
			Association: association,
			State:       state,
			Path:        path,
			Line:        line,
			InReplyTo:   inReplyTo,
			Body:        text.Truncate(strings.TrimSpace(body), maxFeedbackBody),
			URL:         url,
			CreatedAt:   at.Time,
		})
	}

	reviewOpts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < maxFeedbackPages; page++ {
		reviews, resp, err := c.api.PullRequests.ListReviews(ctx, owner, repo, number, reviewOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list reviews for PR #%d: %w", number, err)
		}
		for _, r := range reviews {
			add(r.GetID(), DiscussionReview, r.GetUser(), r.GetAuthorAssociation(), r.GetState(), "", 0, 0, r.GetBody(), r.GetHTMLURL(), r.GetSubmittedAt())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}

	inlineOpts := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < maxFeedbackPages; page++ {
		comments, resp, err := c.api.PullRequests.ListComments(ctx, owner, repo, number, inlineOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list review comments for PR #%d: %w", number, err)
		}
		for _, cm := range comments {
			line := cm.GetLine()
			if line == 0 {
				line = cm.GetOriginalLine()
			}
			add(cm.GetID(), DiscussionInline, cm.GetUser(), cm.GetAuthorAssociation(), "", cm.GetPath(), line, cm.GetInReplyTo(), cm.GetBody(), cm.GetHTMLURL(), cm.GetCreatedAt())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		inlineOpts.Page = resp.NextPage
	}

	issueOpts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < maxFeedbackPages; page++ {
		comments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, number, issueOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list comments for PR #%d: %w", number, err)
		}
		for _, cm := range comments {
			add(cm.GetID(), DiscussionComment, cm.GetUser(), cm.GetAuthorAssociation(), "", "", 0, 0, cm.GetBody(), cm.GetHTMLURL(), cm.GetCreatedAt())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		issueOpts.Page = resp.NextPage
	}
	return items, nil
}

// Check is one check run or commit status on a commit.
type Check struct {
	ID         int64  `json:"id,omitempty"` // check run id; 0 for a commit status
	Name       string `json:"name"`
	Status     string `json:"status"` // queued | in_progress | completed
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	App        string `json:"app,omitempty"`
	Title      string `json:"title,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// ChecksForRef returns the latest check runs and the commit statuses of ref.
func (c *Client) ChecksForRef(ctx context.Context, owner, repo, ref string) ([]Check, error) {
	var out []Check
	opts := &gh.ListCheckRunsOptions{Filter: new("latest"), ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < maxCheckPages; page++ {
		res, resp, err := c.api.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list check runs for %s: %w", ref, err)
		}
		for _, r := range res.CheckRuns {
			out = append(out, Check{
				ID:         r.GetID(),
				Name:       r.GetName(),
				Status:     r.GetStatus(),
				Conclusion: r.GetConclusion(),
				URL:        cmp.Or(r.GetDetailsURL(), r.GetHTMLURL()),
				App:        r.GetApp().GetSlug(),
				Title:      r.GetOutput().GetTitle(),
				Summary:    text.Truncate(r.GetOutput().GetSummary(), 2000),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	combined, _, err := c.api.Repositories.GetCombinedStatus(ctx, owner, repo, ref, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("failed to read the commit status of %s: %w", ref, err)
	}
	for _, s := range combined.Statuses {
		ch := Check{Name: s.GetContext(), Status: "completed", URL: s.GetTargetURL(), Summary: s.GetDescription()}
		switch s.GetState() {
		case "pending":
			ch.Status = "in_progress"
		case "success":
			ch.Conclusion = "success"
		default:
			ch.Conclusion = "failure"
		}
		out = append(out, ch)
	}
	return out, nil
}

// Annotation is one annotation of a check run.
type Annotation struct {
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Level   string `json:"level,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message"`
}

// CheckAnnotations returns up to 20 annotations of a check run.
func (c *Client) CheckAnnotations(ctx context.Context, owner, repo string, checkRunID int64) ([]Annotation, error) {
	anns, _, err := c.api.Checks.ListCheckRunAnnotations(ctx, owner, repo, checkRunID, &gh.ListOptions{PerPage: maxAnnotations})
	if err != nil {
		return nil, fmt.Errorf("failed to list annotations of check run %d: %w", checkRunID, err)
	}
	out := make([]Annotation, 0, len(anns))
	for _, a := range anns {
		out = append(out, Annotation{
			Path:    a.GetPath(),
			Line:    a.GetStartLine(),
			Level:   a.GetAnnotationLevel(),
			Title:   a.GetTitle(),
			Message: text.Truncate(a.GetMessage(), 1000),
		})
	}
	return out, nil
}

// JobLog returns the trimmed log of a GitHub Actions job; a job's id is its check run id.
func (c *Client) JobLog(ctx context.Context, owner, repo string, jobID int64) (string, error) {
	return c.getJobLogs(ctx, owner, repo, jobID)
}

// RequestCopilotReview asks Copilot to review a pull request, best effort.
func (c *Client) RequestCopilotReview(ctx context.Context, owner, repo string, number int) {
	c.requestCopilotReviewer(ctx, owner, repo, number)
}

// MarkReadyForReview takes the draft pull request with the given GraphQL node id out of draft.
func (c *Client) MarkReadyForReview(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return errors.New("the pull request has no node id")
	}
	mutation := `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}`
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"id": nodeID}, &resp); err != nil {
		return fmt.Errorf("markPullRequestReadyForReview: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("markPullRequestReadyForReview: %s", resp.Errors[0].Message)
	}
	return nil
}

// ReplyToReviewComment answers in the thread of an inline review comment; commentID must be the thread's first comment.
func (c *Client) ReplyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, body string) (string, error) {
	cm, _, err := c.api.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, number, body, commentID)
	if err != nil {
		return "", fmt.Errorf("failed to reply to review comment %d: %w", commentID, err)
	}
	return cm.GetHTMLURL(), nil
}

// CommentOnPullRequest adds a conversation comment to a pull request.
func (c *Client) CommentOnPullRequest(ctx context.Context, owner, repo string, number int, body string) (string, error) {
	cm, _, err := c.api.Issues.CreateComment(ctx, owner, repo, number, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return "", fmt.Errorf("failed to comment on PR #%d: %w", number, err)
	}
	return cm.GetHTMLURL(), nil
}

// BranchHead returns the commit a branch points at.
func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	ref, _, err := c.api.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("failed to read branch %s: %w", branch, err)
	}
	return ref.GetObject().GetSHA(), nil
}

// Divergence reports how many commits head has that base lacks, and the reverse.
func (c *Client) Divergence(ctx context.Context, owner, repo, base, head string) (ahead, behind int, err error) {
	cmp, _, err := c.api.Repositories.CompareCommits(ctx, owner, repo, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to compare %s...%s: %w", base, head, err)
	}
	return cmp.GetAheadBy(), cmp.GetBehindBy(), nil
}

// FastForwardBranch moves branch to sha; GitHub refuses a move that is not a fast-forward.
func (c *Client) FastForwardBranch(ctx context.Context, owner, repo, branch, sha string) error {
	_, _, err := c.api.Git.UpdateRef(ctx, owner, repo, "heads/"+branch, gh.UpdateRef{SHA: sha, Force: new(false)})
	if err != nil {
		return fmt.Errorf("failed to move %s to %s: %w", branch, sha, err)
	}
	return nil
}

// CreateBranchAt creates branch pointing at sha.
func (c *Client) CreateBranchAt(ctx context.Context, owner, repo, branch, sha string) error {
	if _, _, err := c.api.Git.CreateRef(ctx, owner, repo, gh.CreateRef{Ref: "refs/heads/" + branch, SHA: sha}); err != nil {
		return fmt.Errorf("failed to create branch %s: %w", branch, err)
	}
	return nil
}

// DeleteBranch removes a branch.
func (c *Client) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	if _, err := c.api.Git.DeleteRef(ctx, owner, repo, "heads/"+branch); err != nil {
		return fmt.Errorf("failed to delete branch %s: %w", branch, err)
	}
	return nil
}

// IsRefused reports whether GitHub answered 403 or 422, as it does for a missing permission, a branch rule or a move that is not a fast-forward.
func IsRefused(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	return ghErr.Response.StatusCode == http.StatusForbidden || ghErr.Response.StatusCode == http.StatusUnprocessableEntity
}
