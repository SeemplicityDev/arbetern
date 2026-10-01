package github

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	gh "github.com/google/go-github/v91/github"

	"github.com/justmike1/arbetern/internal/text"
)

const (
	maxFeedbackPages = 3
	maxFeedbackItems = 50 // shared by reviews and comments, filled in that order
	maxFeedbackBody  = 2000
	projectPRSource  = "project"
)

var trustedAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// PRState is the lifecycle and size of one pull request.
type PRState struct {
	Number       int       `json:"number"`
	URL          string    `json:"url"`
	Title        string    `json:"title"`
	State        string    `json:"state"` // "open" | "closed"
	Merged       bool      `json:"merged"`
	Author       string    `json:"author"`
	HeadRef      string    `json:"head_ref"`
	BaseRef      string    `json:"base_ref"`
	CreatedAt    time.Time `json:"created_at"`
	MergedAt     time.Time `json:"merged_at,omitzero"`
	ClosedAt     time.Time `json:"closed_at,omitzero"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
}

// PullRequestState fetches a pull request's state, timestamps and size.
func (c *Client) PullRequestState(ctx context.Context, owner, repo string, number int) (*PRState, error) {
	pr, _, err := c.api.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR #%d: %w", number, err)
	}
	return &PRState{
		Number:       number,
		URL:          pr.GetHTMLURL(),
		Title:        pr.GetTitle(),
		State:        pr.GetState(),
		Merged:       pr.GetMerged(),
		Author:       pr.GetUser().GetLogin(),
		HeadRef:      pr.GetHead().GetRef(),
		BaseRef:      pr.GetBase().GetRef(),
		CreatedAt:    pr.GetCreatedAt().Time,
		MergedAt:     pr.GetMergedAt().Time,
		ClosedAt:     pr.GetClosedAt().Time,
		Additions:    pr.GetAdditions(),
		Deletions:    pr.GetDeletions(),
		ChangedFiles: pr.GetChangedFiles(),
	}, nil
}

// PRComment is one review or comment on a pull request.
type PRComment struct {
	Author    string    `json:"author"`
	State     string    `json:"state,omitempty"` // review state for reviews
	Path      string    `json:"path,omitempty"`  // file for inline comments
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// PRFeedback holds the reviews and comments left on a pull request.
type PRFeedback struct {
	Reviews  []PRComment `json:"reviews"`
	Comments []PRComment `json:"comments"` // inline review comments then conversation comments, chronological
}

// PullRequestFeedback returns reviews and comments on a pull request by repository owners, members and collaborators other than exclude.
func (c *Client) PullRequestFeedback(ctx context.Context, owner, repo string, number int, exclude string) (*PRFeedback, error) {
	fb := &PRFeedback{Reviews: []PRComment{}, Comments: []PRComment{}}
	exclude = strings.TrimSpace(exclude)
	left := maxFeedbackItems
	add := func(dst *[]PRComment, user *gh.User, association, state, path, body string, at gh.Timestamp) {
		body = strings.TrimSpace(body)
		if left == 0 || body == "" || !slices.Contains(trustedAssociations, association) || (exclude != "" && strings.EqualFold(user.GetLogin(), exclude)) {
			return
		}
		*dst = append(*dst, PRComment{
			Author:    user.GetLogin(),
			State:     state,
			Path:      path,
			Body:      text.Truncate(body, maxFeedbackBody),
			CreatedAt: at.Time,
		})
		left--
	}

	reviewOpts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < maxFeedbackPages && left > 0; page++ {
		reviews, resp, err := c.api.PullRequests.ListReviews(ctx, owner, repo, number, reviewOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list reviews for PR #%d: %w", number, err)
		}
		for _, r := range reviews {
			add(&fb.Reviews, r.GetUser(), r.GetAuthorAssociation(), r.GetState(), "", r.GetBody(), r.GetSubmittedAt())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}

	inlineOpts := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < maxFeedbackPages && left > 0; page++ {
		comments, resp, err := c.api.PullRequests.ListComments(ctx, owner, repo, number, inlineOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list review comments for PR #%d: %w", number, err)
		}
		for _, cm := range comments {
			add(&fb.Comments, cm.GetUser(), cm.GetAuthorAssociation(), "", cm.GetPath(), cm.GetBody(), cm.GetCreatedAt())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		inlineOpts.Page = resp.NextPage
	}

	issueOpts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < maxFeedbackPages && left > 0; page++ {
		comments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, number, issueOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to list comments for PR #%d: %w", number, err)
		}
		for _, cm := range comments {
			add(&fb.Comments, cm.GetUser(), cm.GetAuthorAssociation(), "", "", cm.GetBody(), cm.GetCreatedAt())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		issueOpts.Page = resp.NextPage
	}
	return fb, nil
}

// CommitsAhead reports how many commits head has that base does not.
func (c *Client) CommitsAhead(ctx context.Context, owner, repo, base, head string) (int, error) {
	// ahead_by covers the whole comparison, so one commit per page keeps the response small.
	cmp, _, err := c.api.Repositories.CompareCommits(ctx, owner, repo, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return 0, fmt.Errorf("failed to compare %s...%s: %w", base, head, err)
	}
	return cmp.GetAheadBy(), nil
}

// ProjectPRMarker renders the hidden body marker for a pull request opened by a project task.
func ProjectPRMarker(agent, project, task string) string {
	return renderPRMarker([][2]string{
		{"agent", markerValue(agent)},
		{"source", projectPRSource},
		{"project", markerValue(project)},
		{"task", markerValue(task)},
	})
}

// markerValue drops a value that would not survive prMarkerRe and parsePRMarker intact.
func markerValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.ContainsFunc(v, func(r rune) bool { return r == '>' || unicode.IsSpace(r) }) {
		return ""
	}
	return v
}
