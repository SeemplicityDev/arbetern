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

// Values GitHub reports for a pull request's state, a review's state and a check's status.
const (
	PRStateOpen   = "open"
	PRStateClosed = "closed"
	PRStateAll    = "all"

	ReviewApproved         = "APPROVED"
	ReviewRequired         = "REVIEW_REQUIRED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewDismissed        = "DISMISSED"
	ReviewCommented        = "COMMENTED"

	CheckCompleted = "completed"
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
	UpdatedAt   time.Time `json:"updated_at,omitzero"` // comments: CI reports are often edited in place
	CommitID    string    `json:"commit_id,omitempty"` // reviews: the head the verdict is about
}

// Trusted reports whether the author owns the repository, is a member of its organization or a collaborator.
func (d DiscussionItem) Trusted() bool { return slices.Contains(trustedAssociations, d.Association) }

func isBotUser(u *gh.User) bool {
	login := strings.ToLower(u.GetLogin())
	return u.GetType() == "Bot" || strings.HasSuffix(login, "[bot]") || login == "copilot"
}

// PullRequestDiscussion is PullRequestComments with each body cut to 2,000 bytes.
func (c *Client) PullRequestDiscussion(ctx context.Context, owner, repo string, number int) ([]DiscussionItem, error) {
	items, err := c.PullRequestComments(ctx, owner, repo, number)
	for i := range items {
		items[i].Body = text.Truncate(items[i].Body, maxFeedbackBody)
	}
	return items, err
}

// PullRequestComments returns the reviews, inline review comments and conversation comments on a pull request with whole bodies, oldest first within each kind.
func (c *Client) PullRequestComments(ctx context.Context, owner, repo string, number int) ([]DiscussionItem, error) {
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
			Body:        strings.TrimSpace(body),
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
			items[len(items)-1].CommitID = r.GetCommitID()
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
			items[len(items)-1].UpdatedAt = cm.GetUpdatedAt().Time
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
			items[len(items)-1].UpdatedAt = cm.GetUpdatedAt().Time
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		issueOpts.Page = resp.NextPage
	}
	return items, nil
}

const defaultCommentChars = 3000

// FormatPRComments renders a discussion newest first in up to 64 KiB, only author's items when author is set, each body cut to maxChars (default 3,000).
func FormatPRComments(ref string, items []DiscussionItem, author string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = defaultCommentChars
	}
	items = slices.DeleteFunc(slices.Clone(items), func(it DiscussionItem) bool {
		return it.Body == "" && (it.Kind != DiscussionReview || it.State == ReviewCommented)
	})
	if len(items) == 0 {
		return fmt.Sprintf("No comments or reviews on %s.", ref)
	}
	total := len(items)
	if author != "" {
		commenters := make([]string, 0, total)
		for _, it := range items {
			commenters = append(commenters, it.Author)
		}
		items = slices.DeleteFunc(items, func(it DiscussionItem) bool {
			return !strings.EqualFold(it.Author, author) && !strings.EqualFold(it.Author, author+"[bot]")
		})
		if len(items) == 0 {
			slices.Sort(commenters)
			return fmt.Sprintf("No comments or reviews by %s on %s. Commenters: %s.", author, ref, strings.Join(slices.Compact(commenters), ", "))
		}
	}
	slices.SortStableFunc(items, func(a, b DiscussionItem) int { return b.CreatedAt.Compare(a.CreatedAt) })

	var sb strings.Builder
	if author != "" {
		fmt.Fprintf(&sb, "Comments and reviews by %s on %s, newest first (%d of %d):\n", author, ref, len(items), total)
	} else {
		fmt.Fprintf(&sb, "Comments and reviews on %s, newest first (%d):\n", ref, total)
	}
	for i, it := range items {
		entry := formatDiscussionItem(it, maxChars)
		if i > 0 && sb.Len()+len(entry) > text.DefaultMaxBytes {
			fmt.Fprintf(&sb, "\n… %d older not shown (output limit); use a smaller max_chars or filter by author.\n", len(items)-i)
			break
		}
		sb.WriteString(entry)
	}
	return sb.String()
}

func formatDiscussionItem(it DiscussionItem, maxChars int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n— %s · %s · %s", it.Author, discussionKind(it), it.CreatedAt.UTC().Format(time.RFC3339))
	if it.UpdatedAt.After(it.CreatedAt) {
		sb.WriteString(", updated " + it.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if it.URL != "" {
		sb.WriteString(" · " + it.URL)
	}
	sb.WriteString("\n")
	if len(it.Body) > maxChars {
		fmt.Fprintf(&sb, "%s… [cut at %d of %d chars]\n", text.TruncatePlain(it.Body, maxChars), maxChars, len(it.Body))
	} else if it.Body != "" {
		sb.WriteString(it.Body + "\n")
	}
	return sb.String()
}

func discussionKind(it DiscussionItem) string {
	switch it.Kind {
	case DiscussionReview:
		return "review " + it.State
	case DiscussionInline:
		if it.Line > 0 {
			return fmt.Sprintf("review comment on %s:%d", it.Path, it.Line)
		}
		return "review comment on " + it.Path
	}
	return "comment"
}

// Check is one check run or commit status on a commit.
type Check struct {
	ID         int64  `json:"id,omitempty"` // check run id; 0 for a commit status
	Name       string `json:"name"`
	Status     string `json:"status"` // queued | in_progress | completed
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	App        string `json:"app,omitempty"`
	AppID      int64  `json:"app_id,omitempty"`
	Title      string `json:"title,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

var failedConclusions = []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale"}

// Failed reports whether the check finished without passing.
func (c Check) Failed() bool {
	return c.Status == CheckCompleted && slices.Contains(failedConclusions, c.Conclusion)
}

const maxListedChecks = 15

// FormatChecks lists the failed and pending checks of sha with their URLs.
func FormatChecks(sha string, checks []Check) string {
	short := sha[:min(len(sha), 7)]
	if len(checks) == 0 {
		return fmt.Sprintf("CI checks on %s: none reported.", short)
	}
	var open []string
	failed, pending := 0, 0
	for _, c := range checks {
		state := "PENDING"
		switch {
		case c.Failed():
			state = "FAILED"
			failed++
		case c.Status != CheckCompleted:
			pending++
		default:
			continue
		}
		line := fmt.Sprintf("  • %s %s (%s)", state, c.Name, cmp.Or(c.Conclusion, c.Status))
		if c.URL != "" {
			line += " " + c.URL
		}
		if note, _, _ := strings.Cut(strings.TrimSpace(cmp.Or(c.Title, c.Summary)), "\n"); note != "" {
			line += " — " + text.Truncate(note, 200)
		}
		open = append(open, line)
	}
	passed := len(checks) - failed - pending
	if len(open) == 0 {
		return fmt.Sprintf("CI checks on %s: all %d passed.", short, passed)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CI checks on %s (%d failed, %d pending, %d passed):", short, failed, pending, passed)
	for _, l := range open[:min(len(open), maxListedChecks)] {
		sb.WriteString("\n" + l)
	}
	if extra := len(open) - maxListedChecks; extra > 0 {
		fmt.Fprintf(&sb, "\n  • … %d more", extra)
	}
	return sb.String()
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
				AppID:      r.GetApp().GetID(),
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
		ch := Check{Name: s.GetContext(), Status: CheckCompleted, URL: s.GetTargetURL(), Summary: s.GetDescription()}
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
	mutation := `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}`
	return c.mutate(ctx, "markPullRequestReadyForReview", mutation, map[string]any{"id": nodeID})
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

// EditPullRequestBody replaces a pull request's description.
func (c *Client) EditPullRequestBody(ctx context.Context, owner, repo string, number int, body string) error {
	if _, _, err := c.api.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{Body: &body}); err != nil {
		return fmt.Errorf("failed to edit the description of PR #%d: %w", number, err)
	}
	return nil
}

// EditPullRequestTitle replaces a pull request's title.
func (c *Client) EditPullRequestTitle(ctx context.Context, owner, repo string, number int, title string) error {
	if _, _, err := c.api.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{Title: &title}); err != nil {
		return fmt.Errorf("failed to edit the title of PR #%d: %w", number, err)
	}
	return nil
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
