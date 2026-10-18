package github

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	gh "github.com/google/go-github/v91/github"
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
	HeadSHA      string    `json:"head_sha"`
	BaseRef      string    `json:"base_ref"`
	Draft        bool      `json:"draft"`
	Mergeable    *bool     `json:"mergeable,omitempty"`       // nil until GitHub has computed it
	MergeState   string    `json:"mergeable_state,omitempty"` // clean | behind | blocked | dirty | unstable | has_hooks | unknown
	AutoMerge    bool      `json:"auto_merge,omitempty"`
	AutoMergeBy  string    `json:"auto_merge_by,omitempty"` // login that turned auto-merge on
	NodeID       string    `json:"node_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	MergedAt     time.Time `json:"merged_at,omitzero"`
	ClosedAt     time.Time `json:"closed_at,omitzero"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	Body         string    `json:"-"`
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
		HeadSHA:      pr.GetHead().GetSHA(),
		BaseRef:      pr.GetBase().GetRef(),
		Draft:        pr.GetDraft(),
		Mergeable:    pr.Mergeable,
		MergeState:   pr.GetMergeableState(),
		AutoMerge:    pr.AutoMerge != nil,
		AutoMergeBy:  pr.GetAutoMerge().GetEnabledBy().GetLogin(),
		NodeID:       pr.GetNodeID(),
		CreatedAt:    pr.GetCreatedAt().Time,
		MergedAt:     pr.GetMergedAt().Time,
		ClosedAt:     pr.GetClosedAt().Time,
		Additions:    pr.GetAdditions(),
		Deletions:    pr.GetDeletions(),
		ChangedFiles: pr.GetChangedFiles(),
		Body:         pr.GetBody(),
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
	items, err := c.PullRequestDiscussion(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	fb := &PRFeedback{Reviews: []PRComment{}, Comments: []PRComment{}}
	exclude = strings.TrimSpace(exclude)
	left := maxFeedbackItems
	for _, kind := range []string{DiscussionReview, DiscussionInline, DiscussionComment} {
		for _, it := range items {
			if left == 0 {
				return fb, nil
			}
			if it.Kind != kind || it.Body == "" || !it.Trusted() || (exclude != "" && strings.EqualFold(it.Author, exclude)) {
				continue
			}
			cm := PRComment{Author: it.Author, State: it.State, Path: it.Path, Body: it.Body, CreatedAt: it.CreatedAt}
			if kind == DiscussionReview {
				fb.Reviews = append(fb.Reviews, cm)
			} else {
				fb.Comments = append(fb.Comments, cm)
			}
			left--
		}
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
func ProjectPRMarker(project, task string) string {
	return renderPRMarker([][2]string{
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
