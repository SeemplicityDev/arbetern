package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	gh "github.com/google/go-github/v91/github"
)

const (
	maxRulePages      = 5
	maxThreadPages    = 10
	maxListedPRFiles  = 3000
	rulesPageSize     = 100
	reviewThreadsPage = 100
)

// MergeRules is what the rulesets on a branch ask of a pull request before it merges.
type MergeRules struct {
	Approvals        int
	CodeOwners       bool
	LastPushApproval bool
	ThreadResolution bool
	ReviewerFiles    []string // file patterns a rule asks a specific reviewer to approve
	MergeMethods     []string // nil when no rule restricts them
	Checks           []RequiredCheck
	Strict           bool // the head must contain the branch's tip
	LinearHistory    bool
	SignedCommits    bool
	Other            []string // rule types that can hold a merge and are described by no field above
}

// RequiredCheck is a status check a rule requires, from one app when AppID is set.
type RequiredCheck struct {
	Context string
	AppID   int64
}

var mergeNeutralRules = []string{
	"creation", "deletion", "non_fast_forward", "branch_name_pattern", "tag_name_pattern", "copilot_code_review",
	"file_path_restriction", "file_extension_restriction", "max_file_path_length", "max_file_size",
}

type branchRuleJSON struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

type pullRequestRuleJSON struct {
	AllowedMergeMethods            []string `json:"allowed_merge_methods"`
	RequireCodeOwnerReview         bool     `json:"require_code_owner_review"`
	RequireLastPushApproval        bool     `json:"require_last_push_approval"`
	RequiredApprovingReviewCount   int      `json:"required_approving_review_count"`
	RequiredReviewThreadResolution bool     `json:"required_review_thread_resolution"`
	RequiredReviewers              []struct {
		MinimumApprovals *int     `json:"minimum_approvals"`
		FilePatterns     []string `json:"file_patterns"`
	} `json:"required_reviewers"`
}

type statusChecksRuleJSON struct {
	RequiredStatusChecks []struct {
		Context       string `json:"context"`
		IntegrationID int64  `json:"integration_id"`
	} `json:"required_status_checks"`
	StrictRequiredStatusChecksPolicy bool `json:"strict_required_status_checks_policy"`
}

// BranchMergeRules reads every active ruleset rule on branch, from the repository and its organization.
func (c *Client) BranchMergeRules(ctx context.Context, owner, repo, branch string) (MergeRules, error) {
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	path := fmt.Sprintf("repos/%s/%s/rules/branches/%s", url.PathEscape(owner), url.PathEscape(repo), strings.Join(segs, "/"))
	var rules []branchRuleJSON
	for page := 1; ; page++ {
		if page > maxRulePages {
			return MergeRules{}, fmt.Errorf("%s/%s has more than %d rules on %s", owner, repo, maxRulePages*rulesPageSize, branch)
		}
		req, err := c.api.NewRequest(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=%d&page=%d", path, rulesPageSize, page), nil)
		if err != nil {
			return MergeRules{}, err
		}
		var batch []branchRuleJSON
		resp, err := c.api.Do(req, &batch)
		if err != nil {
			return MergeRules{}, fmt.Errorf("failed to read the rules on %s/%s %s: %w", owner, repo, branch, err)
		}
		rules = append(rules, batch...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return parseMergeRules(rules)
}

func parseMergeRules(rules []branchRuleJSON) (MergeRules, error) {
	var out MergeRules
	for _, r := range rules {
		switch r.Type {
		case "pull_request":
			var p pullRequestRuleJSON
			if err := json.Unmarshal(r.Parameters, &p); err != nil {
				return MergeRules{}, fmt.Errorf("reading a pull_request rule: %w", err)
			}
			out.Approvals = max(out.Approvals, p.RequiredApprovingReviewCount)
			out.CodeOwners = out.CodeOwners || p.RequireCodeOwnerReview
			out.LastPushApproval = out.LastPushApproval || p.RequireLastPushApproval
			out.ThreadResolution = out.ThreadResolution || p.RequiredReviewThreadResolution
			for _, rr := range p.RequiredReviewers {
				if rr.MinimumApprovals == nil || *rr.MinimumApprovals > 0 {
					out.ReviewerFiles = append(out.ReviewerFiles, rr.FilePatterns...)
				}
			}
			if len(p.AllowedMergeMethods) > 0 {
				if out.MergeMethods == nil {
					out.MergeMethods = slices.Clone(p.AllowedMergeMethods)
				} else {
					out.MergeMethods = slices.DeleteFunc(out.MergeMethods, func(m string) bool { return !slices.Contains(p.AllowedMergeMethods, m) })
				}
			}
		case "required_status_checks":
			var p statusChecksRuleJSON
			if err := json.Unmarshal(r.Parameters, &p); err != nil {
				return MergeRules{}, fmt.Errorf("reading a required_status_checks rule: %w", err)
			}
			for _, c := range p.RequiredStatusChecks {
				out.Checks = append(out.Checks, RequiredCheck{Context: c.Context, AppID: c.IntegrationID})
			}
			out.Strict = out.Strict || p.StrictRequiredStatusChecksPolicy
		case "required_linear_history":
			out.LinearHistory = true
		case "required_signatures":
			out.SignedCommits = true
		default:
			if !slices.Contains(mergeNeutralRules, r.Type) && !slices.Contains(out.Other, r.Type) {
				out.Other = append(out.Other, r.Type)
			}
		}
	}
	return out, nil
}

// UnresolvedReviewThreads counts the review threads on a pull request that nobody has resolved.
func (c *Client) UnresolvedReviewThreads(ctx context.Context, nodeID string) (int, error) {
	if nodeID == "" {
		return 0, errors.New("the pull request has no node id")
	}
	query := fmt.Sprintf(`query($id:ID!,$after:String){node(id:$id){... on PullRequest{reviewThreads(first:%d,after:$after){nodes{isResolved} pageInfo{hasNextPage endCursor}}}}}`, reviewThreadsPage)
	var after any
	open := 0
	for range maxThreadPages {
		var resp struct {
			Data struct {
				Node struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"node"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := c.graphql(ctx, query, map[string]any{"id": nodeID, "after": after}, &resp); err != nil {
			return 0, fmt.Errorf("reviewThreads: %w", err)
		}
		if len(resp.Errors) > 0 {
			return 0, &refusal{"reviewThreads", resp.Errors[0].Message}
		}
		threads := resp.Data.Node.ReviewThreads
		for _, n := range threads.Nodes {
			if !n.IsResolved {
				open++
			}
		}
		if !threads.PageInfo.HasNextPage {
			return open, nil
		}
		after = threads.PageInfo.EndCursor
	}
	return 0, fmt.Errorf("the pull request has more than %d review threads", maxThreadPages*reviewThreadsPage)
}

// ChangedPaths lists the paths a pull request touches, a renamed file under both its names.
func (c *Client) ChangedPaths(ctx context.Context, owner, repo string, number int) ([]string, error) {
	var out []string
	files := 0
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.api.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list the files of PR #%d: %w", number, err)
		}
		for _, f := range page {
			files++
			out = append(out, f.GetFilename())
			if prev := f.GetPreviousFilename(); prev != "" {
				out = append(out, prev)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if files >= maxListedPRFiles {
		return nil, fmt.Errorf("PR #%d changes more files than GitHub lists", number)
	}
	return out, nil
}
