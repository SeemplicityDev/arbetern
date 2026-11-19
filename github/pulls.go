package github

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v91/github"

	"github.com/justmike1/arbetern/internal/origin"
	"github.com/justmike1/arbetern/internal/safego"
)

// AutomatedPR is an open pull request arbetern authored, as listed by the
// console's Pull requests page.
type AutomatedPR struct {
	Repo      string        `json:"repo"`
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	URL       string        `json:"url"`
	Draft     bool          `json:"draft"`
	Agent     string        `json:"agent,omitempty"`
	Source    origin.Source `json:"source,omitempty"`
	Origin    *Origin       `json:"origin,omitempty"`
	Comments  int           `json:"comments"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

const maxAutomatedPRPages = 3

// ListOpenAutomatedPullRequests returns the open pull requests authored by the
// token's user whose body carries an arbetern marker: the ones ready for
// review first, then drafts, each group newest-updated first.
func (c *Client) ListOpenAutomatedPullRequests(ctx context.Context) ([]AutomatedPR, error) {
	login, err := c.GetAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("type:pr is:open author:%s", login)
	opts := &gh.SearchOptions{
		Sort:        "updated",
		Order:       "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	out := []AutomatedPR{}
	for page := 0; page < maxAutomatedPRPages; page++ {
		results, resp, err := c.api.Search.Issues(ctx, q, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to search open pull requests: %w", err)
		}
		for _, issue := range results.Issues {
			body := issue.GetBody()
			if !IsAutomatedPRBody(body) {
				continue
			}
			owner, repo, number, err := ParsePRURL(issue.GetHTMLURL())
			if err != nil {
				continue
			}
			attribution := ParseAttribution(body)
			out = append(out, AutomatedPR{
				Repo:      owner + "/" + repo,
				Number:    number,
				Title:     issue.GetTitle(),
				URL:       issue.GetHTMLURL(),
				Draft:     issue.GetDraft(),
				Agent:     attribution.Agent,
				Source:    attribution.Source,
				Origin:    attribution.Origin(),
				Comments:  issue.GetComments(),
				CreatedAt: issue.GetCreatedAt().Time,
				UpdatedAt: issue.GetUpdatedAt().Time,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Draft && out[j].Draft })
	return out, nil
}

// OpenPullRequest is an open pull request with the review requests still pending on it.
type OpenPullRequest struct {
	Repo               string
	Number             int
	Title              string
	Body               string
	Author             string
	URL                string
	BaseRef            string
	Draft              bool
	Labels             []string
	RequestedReviewers []string
	RequestedTeams     []string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// OpenPRFilter keeps the pull requests that pass every non-empty field, where any value of a field passes it; Reviewers and Teams together are one field.
type OpenPRFilter struct {
	Reviewers []string
	Teams     []string
	Authors   []string
	Labels    []string
	Draft     *bool
}

// Match reports whether pr passes the filter.
func (f OpenPRFilter) Match(pr OpenPullRequest) bool {
	if len(f.Reviewers)+len(f.Teams) > 0 && !anyEqualFold(pr.RequestedReviewers, f.Reviewers) && !anyEqualFold(pr.RequestedTeams, f.Teams) {
		return false
	}
	if len(f.Authors) > 0 && !anyEqualFold([]string{pr.Author}, f.Authors) {
		return false
	}
	if len(f.Labels) > 0 && !anyEqualFold(pr.Labels, f.Labels) {
		return false
	}
	return f.Draft == nil || *f.Draft == pr.Draft
}

func anyEqualFold(have, want []string) bool {
	return slices.ContainsFunc(have, func(h string) bool {
		return slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(h, w) })
	})
}

// OpenPRsBulkResult is one entry in the response of ListOpenPullRequestsBulk; Truncated means only the oldest open pull requests were read.
type OpenPRsBulkResult struct {
	Repo      string
	PRs       []OpenPullRequest
	Truncated bool
	Error     string
}

const maxOpenPRPages = 10

// ListOpenPullRequestsBulk lists the open pull requests of many repos in parallel, oldest first. Same fan-out and error semantics as GetFilesBulk.
func (c *Client) ListOpenPullRequestsBulk(ctx context.Context, owner string, repos []string) ([]OpenPRsBulkResult, error) {
	results := make([]OpenPRsBulkResult, len(repos))
	sem := make(chan struct{}, bulkConcurrency)
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer safego.Recover("github: bulk open pull requests")
			results[i] = OpenPRsBulkResult{Repo: repo}
			if ctx.Err() != nil {
				results[i].Error = ctx.Err().Error()
				return
			}
			prs, truncated, err := c.listOpenPullRequests(ctx, owner, repo)
			if err != nil {
				results[i].Error = err.Error()
				return
			}
			results[i].PRs, results[i].Truncated = prs, truncated
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func (c *Client) listOpenPullRequests(ctx context.Context, owner, repo string) ([]OpenPullRequest, bool, error) {
	opts := &gh.PullRequestListOptions{
		State:       PRStateOpen,
		Sort:        "created",
		Direction:   "asc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []OpenPullRequest
	for page := 0; page < maxOpenPRPages; page++ {
		prs, resp, err := c.api.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, false, fmt.Errorf("failed to list open pull requests: %w", err)
		}
		for _, pr := range prs {
			out = append(out, openPullRequest(repo, pr))
		}
		if resp == nil || resp.NextPage == 0 {
			return out, false, nil
		}
		opts.Page = resp.NextPage
	}
	return out, true, nil
}

func openPullRequest(repo string, pr *gh.PullRequest) OpenPullRequest {
	out := OpenPullRequest{
		Repo:      repo,
		Number:    pr.GetNumber(),
		Title:     pr.GetTitle(),
		Body:      pr.GetBody(),
		Author:    pr.GetUser().GetLogin(),
		URL:       pr.GetHTMLURL(),
		BaseRef:   pr.GetBase().GetRef(),
		Draft:     pr.GetDraft(),
		CreatedAt: pr.GetCreatedAt().Time,
		UpdatedAt: pr.GetUpdatedAt().Time,
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	for _, u := range pr.RequestedReviewers {
		out.RequestedReviewers = append(out.RequestedReviewers, u.GetLogin())
	}
	for _, t := range pr.RequestedTeams {
		out.RequestedTeams = append(out.RequestedTeams, t.GetSlug())
	}
	return out
}
