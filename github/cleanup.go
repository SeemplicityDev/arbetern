package github

import (
	"context"
	"fmt"
	"slices"
	"strings"

	gh "github.com/google/go-github/v91/github"
)

const maxBranchCommits = 500

// OpenedBySelf reports whether the client's own account opened a pull request with this author and an arbetern-marked body.
func (c *Client) OpenedBySelf(ctx context.Context, author, body string) (bool, error) {
	self, err := c.GetAuthenticatedUser(ctx)
	if err != nil {
		return false, err
	}
	return openedBy(self, author, body), nil
}

func openedBy(self, author, body string) bool {
	return strings.EqualFold(author, self) && IsAutomatedPRBody(body)
}

// ClosePullRequest closes a pull request without merging it.
func (c *Client) ClosePullRequest(ctx context.Context, owner, repo string, number int) error {
	if _, _, err := c.api.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: new(PRStateClosed)}); err != nil {
		return fmt.Errorf("failed to close PR #%d: %w", number, err)
	}
	return nil
}

// DeleteOwnBranch deletes a branch only the client's own account worked on, or says why it was kept; an empty base means its last pull request's base.
func (c *Client) DeleteOwnBranch(ctx context.Context, owner, repo, branch, base string) (kept string, err error) {
	sha, err := c.BranchHead(ctx, owner, repo, branch)
	if IsNotFound(err) {
		return "it no longer exists", nil
	}
	if err != nil {
		return "", err
	}
	def, err := c.GetDefaultBranch(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if branch == def {
		return "it is the default branch", nil
	}
	self, err := c.GetAuthenticatedUser(ctx)
	if err != nil {
		return "", err
	}

	prs, err := c.pullRequestsByHead(ctx, owner, repo, branch)
	if err != nil {
		return "", err
	}
	ownPR := false
	for _, pr := range prs {
		if pr.GetState() == PRStateOpen {
			return fmt.Sprintf("pull request #%d is still open from it", pr.GetNumber()), nil
		}
		ownPR = ownPR || openedBy(self, pr.GetUser().GetLogin(), pr.GetBody())
		if base == "" {
			base = pr.GetBase().GetRef()
		}
	}
	if base == "" {
		base = def
	}
	into, _, err := c.api.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{State: PRStateOpen, Base: branch, ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil {
		return "", fmt.Errorf("failed to list pull requests into %s: %w", branch, err)
	}
	if len(into) > 0 {
		return fmt.Sprintf("open pull request #%d merges into it", into[0].GetNumber()), nil
	}

	own, others, tooMany, err := c.branchAuthors(ctx, owner, repo, base, sha, self)
	switch {
	case err != nil:
		return "", err
	case tooMany:
		return fmt.Sprintf("it has more than %d commits beyond %s, too many to check", maxBranchCommits, base), nil
	case len(others) > 0:
		return fmt.Sprintf("it has commits beyond %s by %s", base, strings.Join(others, ", ")), nil
	case own == 0 && !ownPR:
		return fmt.Sprintf("%s opened no pull request from it and wrote none of its commits", self), nil
	}

	now, err := c.BranchHead(ctx, owner, repo, branch)
	if err != nil {
		return "", err
	}
	if now != sha {
		return "it received new commits while it was being checked", nil
	}
	if _, err := c.api.Git.DeleteRef(ctx, owner, repo, "heads/"+branch); err != nil {
		return "", fmt.Errorf("failed to delete branch %s: %w", branch, err)
	}
	return "", nil
}

func (c *Client) pullRequestsByHead(ctx context.Context, owner, repo, branch string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{State: PRStateAll, Head: owner + ":" + branch, ListOptions: gh.ListOptions{PerPage: 30}}
	prs, _, err := c.api.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list pull requests from %s: %w", branch, err)
	}
	return slices.DeleteFunc(prs, func(pr *gh.PullRequest) bool { return pr.GetHead().GetRef() != branch }), nil
}

func (c *Client) branchAuthors(ctx context.Context, owner, repo, base, head, self string) (own int, others []string, tooMany bool, err error) {
	opts := &gh.ListOptions{PerPage: 100}
	for {
		cmp, resp, err := c.api.Repositories.CompareCommits(ctx, owner, repo, base, head, opts)
		if err != nil {
			return 0, nil, false, fmt.Errorf("failed to compare %s...%s: %w", base, head, err)
		}
		if cmp.GetAheadBy() > maxBranchCommits {
			return 0, nil, true, nil
		}
		for _, rc := range cmp.Commits {
			if strings.EqualFold(rc.GetAuthor().GetLogin(), self) {
				own++
				continue
			}
			// A merge GitHub made of base into the branch ("Update branch") carries no work of its own.
			if len(rc.Parents) == 2 {
				_, baseMerge, err := c.BaseMerge(ctx, owner, repo, base, rc.GetSHA())
				if err != nil {
					return 0, nil, false, err
				}
				if baseMerge {
					continue
				}
			}
			who := rc.GetAuthor().GetLogin()
			if who == "" {
				who = rc.GetCommit().GetAuthor().GetName()
			}
			if !slices.Contains(others, who) {
				others = append(others, who)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return own, others, false, nil
		}
		opts.Page = resp.NextPage
	}
}
