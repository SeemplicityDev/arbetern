package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

type Client struct {
	api *gh.Client
}

func NewClient(token string) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(context.Background(), ts)
	return &Client{api: gh.NewClient(httpClient)}
}

func (c *Client) GetAuthenticatedUser(ctx context.Context) (string, error) {
	user, _, err := c.api.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("failed to get authenticated user: %w", err)
	}
	return user.GetLogin(), nil
}

// GetGrantedScopes queries the GitHub API and returns the OAuth scopes
// the configured token actually has (read from the X-OAuth-Scopes header).
func (c *Client) GetGrantedScopes(ctx context.Context) ([]string, error) {
	_, resp, err := c.api.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to query GitHub API: %w", err)
	}
	raw := resp.Header.Get("X-OAuth-Scopes")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	scopes := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			scopes = append(scopes, s)
		}
	}
	return scopes, nil
}

func (c *Client) ResolveOwner(ctx context.Context) (string, error) {
	user, _, err := c.api.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("failed to resolve owner: %w", err)
	}

	orgs, _, err := c.api.Organizations.List(ctx, "", nil)
	if err == nil && len(orgs) > 0 {
		return orgs[0].GetLogin(), nil
	}

	return user.GetLogin(), nil
}

func (c *Client) GetFileContent(ctx context.Context, owner, repo, path, branch string) (string, string, error) {
	opts := &gh.RepositoryContentGetOptions{Ref: branch}
	file, _, _, err := c.api.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return "", "", fmt.Errorf("failed to get file %s: %w", path, err)
	}

	content, err := base64.StdEncoding.DecodeString(*file.Content)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode file content: %w", err)
	}

	return string(content), file.GetSHA(), nil
}

func (c *Client) GetDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	r, _, err := c.api.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("failed to get repository %s/%s: %w", owner, repo, err)
	}
	return r.GetDefaultBranch(), nil
}

func (c *Client) CreateBranch(ctx context.Context, owner, repo, baseBranch, newBranch string) error {
	ref, _, err := c.api.Git.GetRef(ctx, owner, repo, "refs/heads/"+baseBranch)
	if err != nil {
		return fmt.Errorf("failed to get ref for %s: %w", baseBranch, err)
	}

	newRef := &gh.Reference{
		Ref:    gh.String("refs/heads/" + newBranch),
		Object: ref.Object,
	}

	_, _, err = c.api.Git.CreateRef(ctx, owner, repo, newRef)
	if err != nil {
		return fmt.Errorf("failed to create branch %s: %w", newBranch, err)
	}
	return nil
}

func (c *Client) UpdateFile(ctx context.Context, owner, repo, path, branch, message string, content []byte, sha string) error {
	opts := &gh.RepositoryContentFileOptions{
		Message: gh.String(message),
		Content: content,
		Branch:  gh.String(branch),
		SHA:     gh.String(sha),
	}

	_, _, err := c.api.Repositories.UpdateFile(ctx, owner, repo, path, opts)
	if err != nil {
		return fmt.Errorf("failed to update file %s: %w", path, err)
	}
	return nil
}

// CreateFile creates a new file in a GitHub repository. Unlike UpdateFile, no
// SHA is required because the file does not exist yet.
func (c *Client) CreateFile(ctx context.Context, owner, repo, path, branch, message string, content []byte) error {
	opts := &gh.RepositoryContentFileOptions{
		Message: gh.String(message),
		Content: content,
		Branch:  gh.String(branch),
	}

	_, _, err := c.api.Repositories.CreateFile(ctx, owner, repo, path, opts)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", path, err)
	}
	return nil
}

func (c *Client) CreatePullRequest(ctx context.Context, owner, repo, baseBranch, headBranch, title, body string) (string, error) {
	pr := &gh.NewPullRequest{
		Title: gh.String(title),
		Body:  gh.String(body),
		Head:  gh.String(headBranch),
		Base:  gh.String(baseBranch),
	}

	created, _, err := c.api.PullRequests.Create(ctx, owner, repo, pr)
	if err != nil {
		return "", fmt.Errorf("failed to create pull request: %w", err)
	}

	// Best-effort: request GitHub Copilot as a reviewer. This silently
	// no-ops if Copilot code review is not enabled for the repo (e.g. the
	// plan doesn't include it, the org has it disabled, or the PR is from
	// a fork) — we never fail the PR creation because of it.
	c.requestCopilotReviewer(ctx, owner, repo, created.GetNumber())

	return created.GetHTMLURL(), nil
}

// requestCopilotReviewer best-effort-requests the GitHub Copilot bot as a
// reviewer on an existing pull request. Errors are logged and swallowed: we
// never fail PR creation because Copilot couldn't be added.
//
// GitHub Copilot code review is a Bot, not a User, so it cannot be requested
// through the normal `reviewers` list with its bot login
// (`copilot-pull-request-reviewer[bot]`) — the REST endpoint returns 422
// "Could not resolve to a User with the username ...". GitHub special-cases
// the literal string "Copilot" in the REST request-reviewers endpoint for
// repos where Copilot code review is enabled, so we try that first.
//
// If that also fails (older GHES, Copilot not available on the plan, not
// enabled at the org/repo level, or the PAT lacks `pull_request: write` /
// `repo` scope), we fall back to the GraphQL `requestReviews` mutation
// which resolves the Copilot actor by bot login and requests review that
// way. Any remaining error is logged with the raw response body.
func (c *Client) requestCopilotReviewer(ctx context.Context, owner, repo string, number int) {
	if number == 0 {
		return
	}
	// Attempt 1: REST with the magic "Copilot" login.
	req := gh.ReviewersRequest{Reviewers: []string{"Copilot"}}
	_, resp, err := c.api.PullRequests.RequestReviewers(ctx, owner, repo, number, req)
	if err == nil {
		log.Printf("[github] requested copilot review on %s/%s#%d (REST)", owner, repo, number)
		return
	}
	restStatus, restBody := describeHTTPError(resp, err)
	log.Printf("[github] copilot REST request failed for %s/%s#%d: %s body=%s", owner, repo, number, restStatus, restBody)

	// Attempt 2: GraphQL fallback.
	if err := c.requestCopilotReviewerGraphQL(ctx, owner, repo, number); err != nil {
		log.Printf("[github] copilot GraphQL request failed for %s/%s#%d: %v", owner, repo, number, err)
		return
	}
	log.Printf("[github] requested copilot review on %s/%s#%d (GraphQL)", owner, repo, number)
}

// describeHTTPError extracts a short status + body from a go-github response,
// handling nil-resp cases.
func describeHTTPError(resp *gh.Response, err error) (status, body string) {
	if resp == nil || resp.Response == nil {
		return "no response", err.Error()
	}
	status = resp.Status
	if resp.Body != nil {
		b, _ := io.ReadAll(resp.Body)
		body = strings.TrimSpace(string(b))
		if len(body) > 500 {
			body = body[:500] + "…"
		}
	}
	if body == "" {
		body = err.Error()
	}
	return status, body
}

// requestCopilotReviewerGraphQL resolves the Copilot bot actor for the repo
// and requests its review via the GraphQL `requestReviews` mutation. Returns
// nil on success.
func (c *Client) requestCopilotReviewerGraphQL(ctx context.Context, owner, repo string, number int) error {
	// Step 1: resolve PR node ID + the Copilot bot's suggested-reviewer id.
	// Copilot surfaces in pullRequest.suggestedReviewers when available for
	// that repo; if it's not present, Copilot is simply not enabled here.
	query := `query($owner:String!,$repo:String!,$num:Int!){
	  repository(owner:$owner,name:$repo){
	    pullRequest(number:$num){
	      id
	      suggestedReviewers { reviewer { __typename login ... on Bot { id } ... on User { id } } }
	    }
	  }
	}`
	queryVars := map[string]any{"owner": owner, "repo": repo, "num": number}
	queryResp := struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ID                 string `json:"id"`
					SuggestedReviewers []struct {
						Reviewer struct {
							TypeName string `json:"__typename"`
							Login    string `json:"login"`
							ID       string `json:"id"`
						} `json:"reviewer"`
					} `json:"suggestedReviewers"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}{}
	if err := c.graphql(ctx, query, queryVars, &queryResp); err != nil {
		return fmt.Errorf("resolve PR/copilot node ids: %w", err)
	}
	if len(queryResp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", queryResp.Errors[0].Message)
	}
	prID := queryResp.Data.Repository.PullRequest.ID
	if prID == "" {
		return fmt.Errorf("pull request node id not found")
	}
	var copilotID string
	for _, s := range queryResp.Data.Repository.PullRequest.SuggestedReviewers {
		if strings.EqualFold(s.Reviewer.Login, "copilot-pull-request-reviewer") ||
			strings.EqualFold(s.Reviewer.Login, "Copilot") {
			copilotID = s.Reviewer.ID
			break
		}
	}
	if copilotID == "" {
		return fmt.Errorf("copilot bot not in suggestedReviewers — code review likely not enabled for this repo")
	}

	// Step 2: request Copilot as reviewer.
	mutation := `mutation($prId:ID!,$uids:[ID!]!){
	  requestReviews(input:{pullRequestId:$prId,userIds:$uids,union:true}){
	    clientMutationId
	  }
	}`
	mutVars := map[string]any{"prId": prID, "uids": []string{copilotID}}
	mutResp := struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}{}
	if err := c.graphql(ctx, mutation, mutVars, &mutResp); err != nil {
		return fmt.Errorf("requestReviews mutation: %w", err)
	}
	if len(mutResp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", mutResp.Errors[0].Message)
	}
	return nil
}

// graphql POSTs a query + variables to GitHub's GraphQL endpoint using the
// same authenticated HTTP client the REST API uses, and decodes the response
// into out.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	payload := map[string]any{"query": query, "variables": vars}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.api.Client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500] + "…"
		}
		return fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, snippet)
	}
	return json.Unmarshal(body, out)
}

func GenerateBranchName(agentName string) string {
	return fmt.Sprintf("%s/patch-%d", agentName, time.Now().Unix())
}

func (c *Client) SearchFiles(ctx context.Context, owner, repo, branch, pattern string) ([]string, error) {
	ref, _, err := c.api.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("failed to get ref for %s: %w", branch, err)
	}
	tree, _, err := c.api.Git.GetTree(ctx, owner, repo, ref.Object.GetSHA(), true)
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	lowerPattern := strings.ToLower(pattern)
	var matches []string
	for _, entry := range tree.Entries {
		path := entry.GetPath()
		if strings.Contains(strings.ToLower(path), lowerPattern) {
			matches = append(matches, path)
		}
	}

	// If the tree was truncated (very large repo), fall back to GitHub code search
	// to find files by path, which doesn't have the same size limitation.
	if tree.GetTruncated() && len(matches) == 0 {
		q := fmt.Sprintf("filename:%s repo:%s/%s", pattern, owner, repo)
		results, _, searchErr := c.api.Search.Code(ctx, q, &gh.SearchOptions{
			ListOptions: gh.ListOptions{PerPage: 100},
		})
		if searchErr == nil {
			for _, r := range results.CodeResults {
				matches = append(matches, r.GetPath())
			}
		}
	}

	return matches, nil
}

func (c *Client) GetDirectoryContents(ctx context.Context, owner, repo, path, branch string) ([]string, error) {
	opts := &gh.RepositoryContentGetOptions{Ref: branch}
	_, dir, _, err := c.api.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get directory %s: %w", path, err)
	}
	if dir == nil {
		return nil, fmt.Errorf("path %s is not a directory", path)
	}
	var entries []string
	for _, entry := range dir {
		name := entry.GetPath()
		if entry.GetType() == "dir" {
			name += "/"
		}
		entries = append(entries, name)
	}
	return entries, nil
}

func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]string, error) {
	var allRepos []string
	opts := &gh.RepositoryListByOrgOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for {
		repos, resp, err := c.api.Repositories.ListByOrg(ctx, org, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list repositories for org %s: %w", org, err)
		}
		for _, r := range repos {
			// Skip archived repos — they never receive commits, so
			// including them in activity digests just wastes API calls
			// and rate-limit budget. Same for disabled repos (blocked
			// by GitHub), which the caller can never read from.
			if r.GetArchived() || r.GetDisabled() {
				continue
			}
			allRepos = append(allRepos, r.GetFullName())
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allRepos, nil
}

func (c *Client) ListUserRepos(ctx context.Context) ([]string, error) {
	var allRepos []string
	opts := &gh.RepositoryListByAuthenticatedUserOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for {
		repos, resp, err := c.api.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list repositories: %w", err)
		}
		for _, r := range repos {
			allRepos = append(allRepos, r.GetFullName())
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allRepos, nil
}

var workflowRunURLPattern = regexp.MustCompile(`https://github\.com/([^/]+)/([^/]+)/actions/runs/(\d+)`)

// prURLPattern matches GitHub PR URLs like https://github.com/owner/repo/pull/123
var prURLPattern = regexp.MustCompile(`https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// ExtractPRURLs returns all GitHub PR URLs found in the given text.
func ExtractPRURLs(text string) []string {
	return prURLPattern.FindAllString(text, -1)
}

// ParsePRURL extracts owner, repo, and PR number from a GitHub PR URL.
func ParsePRURL(rawURL string) (owner, repo string, number int, err error) {
	matches := prURLPattern.FindStringSubmatch(rawURL)
	if len(matches) != 4 {
		return "", "", 0, fmt.Errorf("not a valid GitHub PR URL: %s", rawURL)
	}
	n, err := strconv.Atoi(matches[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid PR number in URL: %w", err)
	}
	return matches[1], matches[2], n, nil
}

// PRSummary holds essential information about a pull request.
type PRSummary struct {
	Number    int
	Title     string
	State     string
	Author    string
	URL       string
	Body      string
	Diff      string
	FileNames []string
}

// GetPullRequest fetches a PR's details and diff.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*PRSummary, error) {
	pr, _, err := c.api.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR #%d: %w", number, err)
	}

	summary := &PRSummary{
		Number: number,
		Title:  pr.GetTitle(),
		State:  pr.GetState(),
		Author: pr.GetUser().GetLogin(),
		URL:    pr.GetHTMLURL(),
		Body:   pr.GetBody(),
	}

	// Get changed files with pagination.
	var diff strings.Builder
	opts := &gh.ListOptions{PerPage: 100}
	for {
		files, resp, err := c.api.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			break
		}
		for _, f := range files {
			summary.FileNames = append(summary.FileNames, f.GetFilename())
			fmt.Fprintf(&diff, "--- %s (%s, +%d -%d)\n", f.GetFilename(), f.GetStatus(), f.GetAdditions(), f.GetDeletions())
			if patch := f.GetPatch(); patch != "" {
				diff.WriteString(patch)
				diff.WriteString("\n\n")
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	summary.Diff = diff.String()

	return summary, nil
}

// FormatPRSummary turns a PRSummary into a readable string.
func FormatPRSummary(s *PRSummary) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "PR #%d: %s\n", s.Number, s.Title)
	fmt.Fprintf(&sb, "Author: %s | State: %s\n", s.Author, s.State)
	fmt.Fprintf(&sb, "URL: %s\n", s.URL)
	if s.Body != "" {
		body := s.Body
		if len(body) > 1000 {
			body = body[:1000] + "..."
		}
		fmt.Fprintf(&sb, "\nDescription:\n%s\n", body)
	}
	if len(s.FileNames) > 0 {
		fmt.Fprintf(&sb, "\nChanged files (%d):\n", len(s.FileNames))
		for _, f := range s.FileNames {
			fmt.Fprintf(&sb, "  • %s\n", f)
		}
	}
	if s.Diff != "" {
		diff := s.Diff
		if len(diff) > 12000 {
			diff = diff[:12000] + "\n... (diff truncated)"
		}
		fmt.Fprintf(&sb, "\nDiff:\n%s\n", diff)
	}
	return sb.String()
}

// CommitSummary holds essential information about a single commit.
type CommitSummary struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"` // first line only
	Author  string    `json:"author"`  // GitHub login if available, else committer name
	Date    time.Time `json:"date"`    // author date (commit timestamp)
	URL     string    `json:"url"`
}

// ListCommits returns commits for a repo, optionally restricted to a branch
// (sha) and an author/time window. since and until may be zero to omit.
// limit caps the number of commits returned (GitHub default PerPage=30;
// paginates up to 'limit', max 300).
func (c *Client) ListCommits(ctx context.Context, owner, repo, branch, author string, since, until time.Time, limit int) ([]CommitSummary, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 300 {
		limit = 300
	}
	perPage := limit
	if perPage > 100 {
		perPage = 100
	}
	opts := &gh.CommitsListOptions{
		SHA:    branch,
		Author: author,
		ListOptions: gh.ListOptions{
			PerPage: perPage,
		},
	}
	if !since.IsZero() {
		opts.Since = since
	}
	if !until.IsZero() {
		opts.Until = until
	}

	var out []CommitSummary
	for {
		page, resp, err := c.api.Repositories.ListCommits(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list commits for %s/%s: %w", owner, repo, err)
		}
		for _, rc := range page {
			msg := rc.GetCommit().GetMessage()
			if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
				msg = msg[:idx]
			}
			author := rc.GetAuthor().GetLogin()
			if author == "" {
				author = rc.GetCommit().GetAuthor().GetName()
			}
			out = append(out, CommitSummary{
				SHA:     rc.GetSHA(),
				Message: msg,
				Author:  author,
				Date:    rc.GetCommit().GetAuthor().GetDate().Time,
				URL:     rc.GetHTMLURL(),
			})
			if len(out) >= limit {
				return out, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// ListPullRequests returns recent PRs for a repo.
func (c *Client) ListPullRequests(ctx context.Context, owner, repo, state string, limit int) ([]PRSummary, error) {
	if state == "" {
		state = "all"
	}
	if limit <= 0 || limit > 30 {
		limit = 10
	}

	prs, _, err := c.api.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       state,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: limit},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list PRs: %w", err)
	}

	var summaries []PRSummary
	for _, pr := range prs {
		summaries = append(summaries, PRSummary{
			Number: pr.GetNumber(),
			Title:  pr.GetTitle(),
			State:  pr.GetState(),
			Author: pr.GetUser().GetLogin(),
			URL:    pr.GetHTMLURL(),
		})
	}
	return summaries, nil
}

// SearchCode searches for code content in a repository using the GitHub code search API.
// Paginates through all results (up to GitHub's 1000-result limit) and requests text-match fragments.
func (c *Client) SearchCode(ctx context.Context, owner, repo, query string) ([]CodeSearchResult, error) {
	q := fmt.Sprintf("%s repo:%s/%s", query, owner, repo)

	var allMatches []CodeSearchResult
	opts := &gh.SearchOptions{
		TextMatch:   true,
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	for {
		results, resp, err := c.api.Search.Code(ctx, q, opts)
		if err != nil {
			// If we already have some results and hit a secondary rate limit, return what we have.
			if len(allMatches) > 0 {
				break
			}
			return nil, fmt.Errorf("failed to search code: %w", err)
		}

		for _, r := range results.CodeResults {
			match := CodeSearchResult{
				File: r.GetPath(),
				Repo: r.GetRepository().GetFullName(),
				URL:  r.GetHTMLURL(),
			}
			for _, frag := range r.TextMatches {
				match.Fragments = append(match.Fragments, frag.GetFragment())
			}
			allMatches = append(allMatches, match)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allMatches, nil
}

// SearchCodeOrg searches for code content across all repositories in a GitHub
// organization. This is much more efficient than calling SearchCode per-repo
// when the user wants to find usages across the entire org.
// Paginates through all results (up to GitHub's 1000-result limit) and requests text-match fragments.
func (c *Client) SearchCodeOrg(ctx context.Context, org, query string) ([]CodeSearchResult, error) {
	q := fmt.Sprintf("%s org:%s", query, org)

	var allMatches []CodeSearchResult
	opts := &gh.SearchOptions{
		TextMatch:   true,
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	for {
		results, resp, err := c.api.Search.Code(ctx, q, opts)
		if err != nil {
			if len(allMatches) > 0 {
				break
			}
			return nil, fmt.Errorf("failed to search code in org: %w", err)
		}

		for _, r := range results.CodeResults {
			match := CodeSearchResult{
				File: r.GetPath(),
				Repo: r.GetRepository().GetFullName(),
				URL:  r.GetHTMLURL(),
			}
			for _, frag := range r.TextMatches {
				match.Fragments = append(match.Fragments, frag.GetFragment())
			}
			allMatches = append(allMatches, match)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allMatches, nil
}

// CodeSearchResult represents a single code search hit.
type CodeSearchResult struct {
	File      string
	Repo      string
	URL       string
	Fragments []string
}

func ParseWorkflowRunURL(rawURL string) (owner, repo string, runID int64, err error) {
	matches := workflowRunURLPattern.FindStringSubmatch(rawURL)
	if len(matches) != 4 {
		return "", "", 0, fmt.Errorf("not a valid GitHub Actions workflow run URL: %s", rawURL)
	}
	runID, err = strconv.ParseInt(matches[3], 10, 64)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid run ID in URL: %w", err)
	}
	return matches[1], matches[2], runID, nil
}

func ExtractWorkflowRunURLs(text string) []string {
	return workflowRunURLPattern.FindAllString(text, -1)
}

type WorkflowRunSummary struct {
	RunID       int64
	Name        string
	Status      string
	Conclusion  string
	URL         string
	Jobs        []WorkflowJobSummary
	Annotations []WorkflowAnnotation
}

type WorkflowJobSummary struct {
	Name       string
	Status     string
	Conclusion string
	Steps      []WorkflowStepSummary
	LogContent string // Populated for failed jobs only.
}

type WorkflowStepSummary struct {
	Name       string
	Status     string
	Conclusion string
}

type WorkflowAnnotation struct {
	JobName string
	Level   string
	Message string
	Title   string
}

func (c *Client) GetWorkflowRunSummary(ctx context.Context, owner, repo string, runID int64) (*WorkflowRunSummary, error) {
	run, _, err := c.api.Actions.GetWorkflowRunByID(ctx, owner, repo, runID)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow run %d: %w", runID, err)
	}

	summary := &WorkflowRunSummary{
		RunID:      runID,
		Name:       run.GetName(),
		Status:     run.GetStatus(),
		Conclusion: run.GetConclusion(),
		URL:        run.GetHTMLURL(),
	}

	jobs, _, err := c.api.Actions.ListWorkflowJobs(ctx, owner, repo, runID, nil)
	if err != nil {
		return summary, fmt.Errorf("failed to list jobs for run %d: %w", runID, err)
	}

	for _, job := range jobs.Jobs {
		js := WorkflowJobSummary{
			Name:       job.GetName(),
			Status:     job.GetStatus(),
			Conclusion: job.GetConclusion(),
		}
		for _, step := range job.Steps {
			js.Steps = append(js.Steps, WorkflowStepSummary{
				Name:       step.GetName(),
				Status:     step.GetStatus(),
				Conclusion: step.GetConclusion(),
			})
		}

		// Fetch actual log output for failed jobs.
		if job.GetConclusion() == "failure" {
			logContent, logErr := c.getJobLogs(ctx, owner, repo, job.GetID())
			if logErr == nil {
				js.LogContent = logContent
			}
		}

		summary.Jobs = append(summary.Jobs, js)

		checkRunID := parseCheckRunID(job.GetCheckRunURL())
		if checkRunID == 0 {
			continue
		}
		annotations, _, err := c.api.Checks.ListCheckRunAnnotations(ctx, owner, repo, checkRunID, nil)
		if err != nil {
			continue
		}
		for _, ann := range annotations {
			summary.Annotations = append(summary.Annotations, WorkflowAnnotation{
				JobName: job.GetName(),
				Level:   ann.GetAnnotationLevel(),
				Message: ann.GetMessage(),
				Title:   ann.GetTitle(),
			})
		}
	}

	return summary, nil
}

func parseCheckRunID(checkRunURL string) int64 {
	if checkRunURL == "" {
		return 0
	}
	parts := strings.Split(checkRunURL, "/")
	if len(parts) == 0 {
		return 0
	}
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

const maxJobLogSize = 16000

// getJobLogs downloads the plain-text log for a specific job run.
func (c *Client) getJobLogs(ctx context.Context, owner, repo string, jobID int64) (string, error) {
	logURL, _, err := c.api.Actions.GetWorkflowJobLogs(ctx, owner, repo, jobID, 2)
	if err != nil {
		return "", fmt.Errorf("failed to get log URL for job %d: %w", jobID, err)
	}

	resp, err := http.Get(logURL.String())
	if err != nil {
		return "", fmt.Errorf("failed to download job logs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJobLogSize+1))
	if err != nil {
		return "", fmt.Errorf("failed to read job logs: %w", err)
	}

	content := string(body)
	if len(content) > maxJobLogSize {
		// Keep the tail — the error is usually at the end.
		content = "... (log truncated, showing last portion) ...\n" + content[len(content)-maxJobLogSize:]
	}
	return content, nil
}

func FormatWorkflowRunSummary(s *WorkflowRunSummary) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Workflow Run: %s (ID: %d)\n", s.Name, s.RunID)
	fmt.Fprintf(&sb, "Status: %s | Conclusion: %s\n", s.Status, s.Conclusion)
	fmt.Fprintf(&sb, "URL: %s\n\n", s.URL)

	for _, job := range s.Jobs {
		icon := "+"
		if job.Conclusion == "failure" {
			icon = "X"
		}
		fmt.Fprintf(&sb, "[%s] Job: %s (%s)\n", icon, job.Name, job.Conclusion)
		for _, step := range job.Steps {
			stepIcon := " "
			switch step.Conclusion {
			case "failure":
				stepIcon = "X"
			case "success":
				stepIcon = "+"
			}
			fmt.Fprintf(&sb, "  [%s] %s (%s)\n", stepIcon, step.Name, step.Conclusion)
		}
		sb.WriteString("\n")
	}

	if len(s.Annotations) > 0 {
		sb.WriteString("Annotations:\n")
		for _, ann := range s.Annotations {
			level := strings.ToUpper(ann.Level)
			fmt.Fprintf(&sb, "  [%s] %s\n", level, ann.JobName)
			if ann.Title != "" {
				fmt.Fprintf(&sb, "    Title: %s\n", ann.Title)
			}
			fmt.Fprintf(&sb, "    Message: %s\n", ann.Message)
		}
	}

	for _, job := range s.Jobs {
		if job.LogContent != "" {
			fmt.Fprintf(&sb, "\n--- Logs for failed job '%s' ---\n%s\n", job.Name, job.LogContent)
		}
	}

	return sb.String()
}

// RerunFailedJobs re-runs only the failed jobs (and their dependents) in a workflow run.
func (c *Client) RerunFailedJobs(ctx context.Context, owner, repo string, runID int64) error {
	_, err := c.api.Actions.RerunFailedJobsByID(ctx, owner, repo, runID)
	if err != nil {
		return fmt.Errorf("failed to rerun failed jobs for run %d: %w", runID, err)
	}
	return nil
}

// RerunWorkflow re-runs an entire workflow run (all jobs).
func (c *Client) RerunWorkflow(ctx context.Context, owner, repo string, runID int64) error {
	_, err := c.api.Actions.RerunWorkflowByID(ctx, owner, repo, runID)
	if err != nil {
		return fmt.Errorf("failed to rerun workflow run %d: %w", runID, err)
	}
	return nil
}
