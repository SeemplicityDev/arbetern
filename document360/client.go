// Package document360 wraps the read-only subset of the Document360 v3 API
// (https://apidocs.document360.com) an agent needs to answer questions from a
// knowledge base: workspaces, the category tree, article listings, keyword
// search and article content.
//
// Auth is a scoped API key sent as X-API-Key. Every call is a GET, so retries
// are always safe. The v3 API is project-scoped; the project is taken from
// config or discovered when the key can see exactly one.
package document360

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/text"
	"github.com/justmike1/arbetern/internal/ttlcache"
)

const (
	// DefaultRegion is the API region used when none is configured.
	DefaultRegion = "eu"

	maxResponseBody = 8 << 20
	httpTimeout     = 30 * time.Second
	userAgent       = "arbetern/document360-connector"

	// maxRetries bounds the retry chain for one request; the initial attempt
	// is not counted.
	maxRetries = 4
	// maxRateLimitPause caps how long a request waits out an exhausted
	// per-minute window before giving up.
	maxRateLimitPause = 65 * time.Second

	// workspacesTTL is how long the workspace list is served from memory.
	workspacesTTL = 10 * time.Minute
	// contentTTL is how long article bodies and workspace article listings are
	// served from memory. A knowledge base changes far more slowly than an
	// agent re-reads it, so this collapses the repeat fetches a multi-step
	// answer (and every workflow tick) would otherwise make.
	contentTTL = 5 * time.Minute
	// maxCachedArticles bounds the article-body cache.
	maxCachedArticles = 256
	// maxCachedListings bounds the workspace article-listing cache.
	maxCachedListings = 8
	// connectRetryInterval paces the startup probe until the project resolves.
	connectRetryInterval = 30 * time.Second

	// maxConcurrent bounds in-flight requests for one batched tool call, so a
	// batch cannot burn the key's per-minute read allowance in one burst.
	maxConcurrent = 4
	// maxBatchArticles caps how many articles one call may read.
	maxBatchArticles = 5
	// maxBatchQueries caps how many phrasings one search call may run.
	maxBatchQueries = 3
	// maxInlineContent caps how many hits a search may inline bodies for.
	maxInlineContent = 5

	// maxPageSize is the API's page_size ceiling.
	maxPageSize = 100
	// maxListPages caps how many pages a category-filtered listing walks.
	maxListPages = 10
	// maxCategoryPages caps how many pages the category tree is read from.
	maxCategoryPages = 10
)

// regionHosts maps a configured region to its API host. Keys are sent to
// whichever host is chosen, so this allowlist is what stops a mistyped host
// from becoming a credential-exfiltration path.
var regionHosts = map[string]string{
	"eu": "https://apihub.document360.io",
	"us": "https://apihub.us.document360.io",
	"ca": "https://apihub.ca.document360.io",
}

// Regions lists the accepted region codes, sorted.
func Regions() []string {
	out := make([]string, 0, len(regionHosts))
	for r := range regionHosts {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Client talks to one Document360 project. It is safe for concurrent use.
type Client struct {
	baseURL           string
	region            string
	apiKey            string
	configuredProject string
	httpClient        *http.Client

	mu         sync.Mutex
	project    *Project
	pauseUntil time.Time // set when the API reports an exhausted window

	workspaces *ttlcache.Cache[[]Workspace]
	articles   *ttlcache.Keyed[string, *Article]
	listings   *ttlcache.Keyed[string, []ArticleSummary]
}

// NewClient builds a client for the given key, optional project ID and region
// (one of Regions; empty means DefaultRegion). The project is resolved in the
// background with retries, so startup never blocks; Ready reports success.
func NewClient(apiKey, projectID, region string) (*Client, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = DefaultRegion
	}
	base, ok := regionHosts[region]
	if !ok {
		return nil, fmt.Errorf("unknown document360 region %q (want one of %s)", region, strings.Join(Regions(), ", "))
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("document360 api key is empty")
	}
	c := &Client{
		baseURL:           base,
		region:            region,
		apiKey:            apiKey,
		configuredProject: strings.TrimSpace(projectID),
		httpClient:        &http.Client{Timeout: httpTimeout},
	}
	c.workspaces = ttlcache.New(workspacesTTL, c.fetchWorkspaces)
	c.articles = ttlcache.NewKeyed(contentTTL, maxCachedArticles, c.fetchArticle)
	c.listings = ttlcache.NewKeyed(contentTTL, maxCachedListings, c.fetchListing)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.resolveProject(ctx); err != nil {
		log.Printf("[document360] initial project probe failed, will retry every %s: %v", connectRetryInterval, ErrorDetail(err))
		safego.Go("document360: connect retry", c.retryConnect)
	} else {
		log.Printf("[document360] connected (region %s, project %s)", c.region, c.ProjectLabel())
	}
	return c, nil
}

// Ready reports whether the project has been resolved and the key accepted.
func (c *Client) Ready() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.project != nil
}

// Region returns the configured region code.
func (c *Client) Region() string { return c.region }

// ProjectLabel returns the resolved project's name and ID, or the configured
// ID while unresolved, for status panels and logs.
func (c *Client) ProjectLabel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.project == nil {
		if c.configuredProject != "" {
			return c.configuredProject + " (unverified)"
		}
		return "(unresolved)"
	}
	if c.project.Name == "" {
		return c.project.ID
	}
	return fmt.Sprintf("%s (%s)", c.project.Name, c.project.ID)
}

func (c *Client) retryConnect() {
	ticker := time.NewTicker(connectRetryInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := c.resolveProject(ctx)
		cancel()
		if err != nil {
			log.Printf("[document360] project probe retry failed: %v", ErrorDetail(err))
			continue
		}
		log.Printf("[document360] connected after retry (region %s, project %s)", c.region, c.ProjectLabel())
		return
	}
}

// resolveProject validates the configured project or discovers the one the
// key can see. The result is cached for the client's lifetime.
func (c *Client) resolveProject(ctx context.Context) (*Project, error) {
	c.mu.Lock()
	if c.project != nil {
		p := c.project
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()

	var resolved *Project
	if c.configuredProject != "" {
		var p Project
		if err := c.get(ctx, "/v3/projects/"+url.PathEscape(c.configuredProject), nil, &p, nil); err != nil {
			return nil, fmt.Errorf("verifying configured project: %w", err)
		}
		if p.ID == "" {
			p.ID = c.configuredProject
		}
		resolved = &p
	} else {
		var projects []Project
		q := url.Values{"page_size": {strconv.Itoa(maxPageSize)}}
		if err := c.get(ctx, "/v3/projects", q, &projects, nil); err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		switch len(projects) {
		case 0:
			return nil, errors.New("the API key can see no project — check its content scope, or set document360-project-id")
		case 1:
			resolved = &projects[0]
		default:
			names := make([]string, 0, len(projects))
			for _, p := range projects {
				names = append(names, fmt.Sprintf("%s (%s)", p.Name, p.ID))
			}
			return nil, fmt.Errorf("the API key can see %d projects; set document360-project-id to one of: %s", len(projects), strings.Join(names, ", "))
		}
	}

	c.mu.Lock()
	c.project = resolved
	c.mu.Unlock()
	return resolved, nil
}

func (c *Client) projectPath(ctx context.Context) (string, error) {
	p, err := c.resolveProject(ctx)
	if err != nil {
		return "", err
	}
	return "/v3/projects/" + url.PathEscape(p.ID), nil
}

// ── Workspaces ─────────────────────────────────────────────────────────────

func (c *Client) fetchWorkspaces(ctx context.Context) ([]Workspace, error) {
	base, err := c.projectPath(ctx)
	if err != nil {
		return nil, err
	}
	var all []Workspace
	err = c.walkPages(ctx, base+"/workspaces", url.Values{}, maxCategoryPages, func(data json.RawMessage) (int, error) {
		var page []Workspace
		if err := json.Unmarshal(data, &page); err != nil {
			return 0, err
		}
		all = append(all, page...)
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].IsDefault != all[j].IsDefault {
			return all[i].IsDefault
		}
		return all[i].Order < all[j].Order
	})
	return all, nil
}

// ListWorkspaces returns the project's workspaces, default first. The list
// is cached briefly because it changes far more slowly than queries run.
func (c *Client) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	return c.workspaces.Get(ctx)
}

// resolveWorkspace maps an ID, slug or name onto a known workspace; empty
// selects the default. Matching against the listed set means an unknown value
// is refused here rather than becoming a path segment.
func (c *Client) resolveWorkspace(ctx context.Context, ref string) (Workspace, error) {
	list, err := c.ListWorkspaces(ctx)
	if err != nil {
		return Workspace{}, err
	}
	if len(list) == 0 {
		return Workspace{}, errors.New("the project has no workspaces visible to this API key")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return list[0], nil
	}
	for _, w := range list {
		if strings.EqualFold(w.ID, ref) {
			return w, nil
		}
	}
	for _, w := range list {
		if strings.EqualFold(w.Slug, ref) || strings.EqualFold(w.Name, ref) {
			return w, nil
		}
	}
	names := make([]string, 0, len(list))
	for _, w := range list {
		names = append(names, fmt.Sprintf("%s (%s)", w.Name, w.ID))
	}
	return Workspace{}, fmt.Errorf("unknown workspace %q; call document360_list_workspaces — known: %s", ref, strings.Join(names, ", "))
}

// ── Categories ─────────────────────────────────────────────────────────────

// ListCategories returns the workspace's full category tree.
func (c *Client) ListCategories(ctx context.Context, workspaceRef, langCode string) (Workspace, []Category, error) {
	ws, err := c.resolveWorkspace(ctx, workspaceRef)
	if err != nil {
		return Workspace{}, nil, err
	}
	base, err := c.projectPath(ctx)
	if err != nil {
		return ws, nil, err
	}
	q := url.Values{}
	if l := normalizeLang(langCode); l != "" {
		q.Set("lang_code", l)
	}
	var all []Category
	err = c.walkPages(ctx, base+"/workspaces/"+url.PathEscape(ws.ID)+"/categories", q, maxCategoryPages, func(data json.RawMessage) (int, error) {
		var page []Category
		if err := json.Unmarshal(data, &page); err != nil {
			return 0, err
		}
		all = append(all, page...)
		return len(page), nil
	})
	return ws, all, err
}

// ── Articles ───────────────────────────────────────────────────────────────

func listingKey(workspaceID, lang string) string { return workspaceID + "|" + lang }
func articleKey(articleID, lang string) string   { return articleID + "|" + lang }

// fetchListing walks a workspace's article pages once. The API has no
// server-side category filter, so a per-category question is answered from
// this list instead of re-walking every page for each category asked about.
func (c *Client) fetchListing(ctx context.Context, key string) ([]ArticleSummary, error) {
	workspaceID, lang, _ := strings.Cut(key, "|")
	base, err := c.projectPath(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"page_size": {strconv.Itoa(maxPageSize)}}
	if lang != "" {
		q.Set("lang_code", lang)
	}
	var all []ArticleSummary
	err = c.walkPages(ctx, base+"/workspaces/"+url.PathEscape(workspaceID)+"/articles", q, maxListPages, func(data json.RawMessage) (int, error) {
		var rows []ArticleSummary
		if err := json.Unmarshal(data, &rows); err != nil {
			return 0, err
		}
		all = append(all, rows...)
		return len(rows), nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// ListArticles returns article summaries. Without a category it serves one
// page straight from the API. With one it filters the cached workspace
// listing, so asking about several categories costs one walk, not one per
// category.
func (c *Client) ListArticles(ctx context.Context, workspaceRef, categoryID, langCode string, page, pageSize int) (*ArticleList, error) {
	ws, err := c.resolveWorkspace(ctx, workspaceRef)
	if err != nil {
		return nil, err
	}
	base, err := c.projectPath(ctx)
	if err != nil {
		return nil, err
	}
	categoryID = strings.TrimSpace(categoryID)
	lang := normalizeLang(langCode)
	out := &ArticleList{Workspace: ws, CategoryID: categoryID}

	if categoryID == "" {
		if page < 1 {
			page = 1
		}
		q := url.Values{
			"page":                {strconv.Itoa(page)},
			"page_size":           {strconv.Itoa(clamp(pageSize, 1, maxPageSize, 25))},
			"include_total_count": {"true"},
		}
		if lang != "" {
			q.Set("lang_code", lang)
		}
		var rows []ArticleSummary
		pg, err := c.getPage(ctx, base+"/workspaces/"+url.PathEscape(ws.ID)+"/articles", q, &rows)
		if err != nil {
			return nil, err
		}
		out.Articles, out.Pagination, out.Scanned = rows, pg, len(rows)
		return out, nil
	}

	key := listingKey(ws.ID, lang)
	_, out.Cached = c.listings.Cached(key)
	all, err := c.listings.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if strings.EqualFold(a.CategoryID, categoryID) {
			out.Articles = append(out.Articles, a)
		}
	}
	out.Scanned = len(all)
	return out, nil
}

// fetchArticle reads one article, preferring its published version. A 404
// there is retried once without the published constraint, because an article
// the search index still lists may have no resolvable published version in
// the requested language.
func (c *Client) fetchArticle(ctx context.Context, key string) (*Article, error) {
	articleID, lang, _ := strings.Cut(key, "|")
	base, err := c.projectPath(ctx)
	if err != nil {
		return nil, err
	}
	path := base + "/articles/" + url.PathEscape(articleID)

	read := func(published bool) (*Article, error) {
		q := url.Values{
			"content_mode": {"display"},
			"published":    {strconv.FormatBool(published)},
		}
		if lang != "" {
			q.Set("lang_code", lang)
		}
		var a Article
		if err := c.get(ctx, path, q, &a, nil); err != nil {
			return nil, err
		}
		return &a, nil
	}

	a, err := read(true)
	if err == nil {
		return a, nil
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		if a, retryErr := read(false); retryErr == nil {
			return a, nil
		}
	}
	return nil, err
}

// GetArticle fetches one article, rendered for display so snippets and
// variables are resolved. Results are cached briefly.
func (c *Client) GetArticle(ctx context.Context, articleID, langCode string) (*Article, error) {
	articleID = strings.TrimSpace(articleID)
	if articleID == "" {
		return nil, errors.New("article_id is required")
	}
	return c.articles.Get(ctx, articleKey(articleID, normalizeLang(langCode)))
}

// ArticleResult pairs one requested article ID with its outcome, so a batch
// read reports per-article failures instead of failing as a whole.
type ArticleResult struct {
	ID      string
	Article *Article
	Err     error
}

// GetArticles reads several articles concurrently, in the order requested.
// Duplicate IDs are collapsed and the batch is capped at maxBatchArticles.
func (c *Client) GetArticles(ctx context.Context, articleIDs []string, langCode string) ([]ArticleResult, error) {
	ids := dedupeStrings(articleIDs, maxBatchArticles)
	if len(ids) == 0 {
		return nil, errors.New("article_id is required")
	}
	out := make([]ArticleResult, len(ids))
	runConcurrent(ctx, len(ids), maxConcurrent, func(ctx context.Context, i int) {
		a, err := c.GetArticle(ctx, ids[i], langCode)
		out[i] = ArticleResult{ID: ids[i], Article: a, Err: err}
	})
	return out, nil
}

// PlainContent returns the article body as readable text: Markdown as-is,
// otherwise the rendered HTML reduced to text.
func (a *Article) PlainContent() string {
	if a == nil {
		return ""
	}
	if strings.EqualFold(a.ContentType, "markdown") && strings.TrimSpace(a.Content) != "" {
		return strings.TrimSpace(a.Content)
	}
	if strings.TrimSpace(a.HTMLContent) != "" {
		return text.StripHTML(a.HTMLContent)
	}
	return text.StripHTML(a.Content)
}

// ── Search ─────────────────────────────────────────────────────────────────

// SearchOptions describes one search call, which may carry several phrasings
// of the same question and may inline the top hits' article bodies.
type SearchOptions struct {
	WorkspaceRef   string
	Queries        []string
	LangCode       string
	Page           int
	PageSize       int
	IncludeContent bool
	ContentLimit   int
}

// Search runs keyword searches over the published, visible articles of a
// workspace. Several queries run concurrently and their hits are merged with
// duplicates collapsed, so alternative phrasings cost one call rather than
// one round each. With IncludeContent the top hits' bodies are read and
// returned inline, which answers most questions without a follow-up call.
func (c *Client) Search(ctx context.Context, opts SearchOptions) (*SearchResult, error) {
	queries := dedupeStrings(opts.Queries, maxBatchQueries)
	if len(queries) == 0 {
		return nil, errors.New("query is required")
	}
	ws, err := c.resolveWorkspace(ctx, opts.WorkspaceRef)
	if err != nil {
		return nil, err
	}
	base, err := c.projectPath(ctx)
	if err != nil {
		return nil, err
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}
	pageSize := clamp(opts.PageSize, 1, maxPageSize, 10)
	lang := normalizeLang(opts.LangCode)
	path := base + "/workspaces/" + url.PathEscape(ws.ID) + "/search"

	type queryResult struct {
		hits []SearchHit
		pg   Pagination
		err  error
	}
	results := make([]queryResult, len(queries))
	runConcurrent(ctx, len(queries), maxConcurrent, func(ctx context.Context, i int) {
		q := url.Values{
			"query":     {queries[i]},
			"page":      {strconv.Itoa(page)},
			"page_size": {strconv.Itoa(pageSize)},
		}
		if lang != "" {
			q.Set("lang_code", lang)
		}
		var hits []SearchHit
		pg, err := c.getPage(ctx, path, q, &hits)
		results[i] = queryResult{hits: hits, pg: pg, err: err}
	})

	out := &SearchResult{Queries: queries, Workspace: ws, LangCode: lang}
	seen := make(map[string]int, pageSize*len(queries))
	var firstErr error
	for i, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			out.Failed = append(out.Failed, queries[i])
			continue
		}
		if out.Pagination.PageSize == 0 {
			out.Pagination = r.pg
		} else if r.pg.HasMore {
			out.Pagination.HasMore = true
		}
		for _, h := range r.hits {
			if idx, dup := seen[h.ArticleID]; dup {
				out.Hits[idx].MatchedQueries = appendUnique(out.Hits[idx].MatchedQueries, queries[i])
				out.Duplicates++
				continue
			}
			h.MatchedQueries = []string{queries[i]}
			seen[h.ArticleID] = len(out.Hits)
			out.Hits = append(out.Hits, h)
		}
	}
	// Every phrasing failing is a failed call; some failing still answers.
	if len(out.Failed) == len(queries) {
		return nil, firstErr
	}
	if !opts.IncludeContent || len(out.Hits) == 0 {
		return out, nil
	}

	limit := clamp(opts.ContentLimit, 1, maxInlineContent, 3)
	if limit > len(out.Hits) {
		limit = len(out.Hits)
	}
	runConcurrent(ctx, limit, maxConcurrent, func(ctx context.Context, i int) {
		a, err := c.GetArticle(ctx, out.Hits[i].ArticleID, lang)
		if err != nil {
			out.Hits[i].BodyNote = "content unavailable: " + err.Error()
			return
		}
		out.Hits[i].Body = a.PlainContent()
		out.Hits[i].URL = a.URL
	})
	out.ContentRead = limit
	return out, nil
}

// ── Concurrency ────────────────────────────────────────────────────────────

// runConcurrent calls fn for every index below n, at most limit at a time,
// and returns once all have finished. fn records its own result; a cancelled
// context is reported by the per-item error each fn observes.
func runConcurrent(ctx context.Context, n, limit int, fn func(ctx context.Context, i int)) {
	if n <= 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	if limit > n {
		limit = n
	}
	idx := make(chan int, n)
	for i := 0; i < n; i++ {
		idx <- i
	}
	close(idx)
	var wg sync.WaitGroup
	wg.Add(limit)
	for w := 0; w < limit; w++ {
		go func() {
			defer wg.Done()
			for i := range idx {
				fn(ctx, i)
			}
		}()
	}
	wg.Wait()
}

// dedupeStrings trims, drops blanks and duplicates (case-insensitively) and
// caps the result, preserving the caller's order.
func dedupeStrings(in []string, max int) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		k := strings.ToLower(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
		if max > 0 && len(out) == max {
			break
		}
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// ── Request plumbing ───────────────────────────────────────────────────────

// walkPages reads consecutive pages until the API reports no more, the page
// comes back empty, or maxPages is reached. onPage returns the row count so an
// empty page can end a walk whose has_more flag is unreliable.
func (c *Client) walkPages(ctx context.Context, path string, q url.Values, maxPages int, onPage func(json.RawMessage) (int, error), last ...*Pagination) error {
	q = cloneValues(q)
	if q.Get("page_size") == "" {
		q.Set("page_size", strconv.Itoa(maxPageSize))
	}
	if q.Get("page") == "" {
		q.Set("page", "1")
	}
	for i := 0; i < maxPages; i++ {
		var raw json.RawMessage
		var pg Pagination
		if err := c.get(ctx, path, q, &raw, &pg); err != nil {
			return err
		}
		if len(last) > 0 && last[0] != nil {
			*last[0] = pg
		}
		n, err := onPage(raw)
		if err != nil {
			return fmt.Errorf("decoding document360 response: %w", err)
		}
		if n == 0 || !pg.HasMore {
			return nil
		}
		if pg.NextCursor != "" {
			q.Set("cursor", pg.NextCursor)
			q.Del("page")
		} else {
			cur, _ := strconv.Atoi(q.Get("page"))
			if cur < 1 {
				cur = 1
			}
			q.Set("page", strconv.Itoa(cur+1))
		}
	}
	return nil
}

func (c *Client) getPage(ctx context.Context, path string, q url.Values, out any) (Pagination, error) {
	var pg Pagination
	err := c.get(ctx, path, q, out, &pg)
	return pg, err
}

// get performs one authenticated GET, retrying 429/5xx and transport errors
// with backoff, and decodes the envelope's data into out. The query string is
// never logged: it carries search terms.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any, pg *Pagination) error {
	if c == nil {
		return errors.New("document360 client is not configured")
	}
	fullURL := c.baseURL + path
	if len(q) > 0 {
		fullURL += "?" + q.Encode()
	}

	var (
		lastErr  error
		nextWait time.Duration
	)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := httpx.SleepCtx(ctx, nextWait); err != nil {
			return err
		}
		if err := c.waitForWindow(ctx); err != nil {
			return err
		}
		nextWait = httpx.Backoff(attempt+1, 0)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return fmt.Errorf("building document360 request: %w", err)
		}
		req.Header.Set("X-API-Key", c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("document360 request failed: %w", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		c.noteRateLimit(resp.Header, resp.StatusCode)
		if readErr != nil {
			lastErr = fmt.Errorf("reading document360 response: %w", readErr)
			continue
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return decodeEnvelope(body, out, pg)
		case httpx.RetryableStatus(resp.StatusCode) && attempt < maxRetries:
			lastErr = newAPIError(resp.StatusCode, body)
			nextWait = httpx.Backoff(attempt+1, httpx.RetryAfter(resp.Header))
			log.Printf("[document360] retrying in %s (attempt %d/%d) on GET %s — %s",
				nextWait.Round(time.Millisecond), attempt+1, maxRetries, path, ErrorDetail(lastErr))
			continue
		default:
			return newAPIError(resp.StatusCode, body)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("document360 request exhausted %d attempts", maxRetries+1)
	}
	return lastErr
}

// noteRateLimit records an exhausted read window so the next request waits
// for the reset instead of spending its retries on 429s.
func (c *Client) noteRateLimit(h http.Header, status int) {
	remaining, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	exhausted := status == http.StatusTooManyRequests || (err == nil && remaining <= 0)
	if !exhausted {
		return
	}
	wait := httpx.RetryAfter(h)
	if secs, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Reset"))); err == nil && secs > 0 {
		if d := time.Duration(secs) * time.Second; d > wait {
			wait = d
		}
	}
	if wait <= 0 || wait > maxRateLimitPause {
		return
	}
	c.mu.Lock()
	if until := time.Now().Add(wait); until.After(c.pauseUntil) {
		c.pauseUntil = until
	}
	c.mu.Unlock()
}

func (c *Client) waitForWindow(ctx context.Context) error {
	c.mu.Lock()
	wait := time.Until(c.pauseUntil)
	c.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	if wait > maxRateLimitPause {
		wait = maxRateLimitPause
	}
	return httpx.SleepCtx(ctx, wait)
}

func decodeEnvelope(body []byte, out any, pg *Pagination) error {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decoding document360 response: %w", err)
	}
	if !env.Success && len(env.Errors) > 0 {
		e := &APIError{Status: http.StatusOK, RequestID: env.RequestID}
		e.Code, e.Message = env.Errors[0].code(), env.Errors[0].message()
		return e
	}
	if pg != nil && env.Pagination != nil {
		*pg = *env.Pagination
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decoding document360 response: %w", err)
	}
	return nil
}

func (e apiErrorItem) code() string {
	if e.Code != "" {
		return e.Code
	}
	return e.ErrorCode
}

func (e apiErrorItem) message() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Description
}

// ── Errors ─────────────────────────────────────────────────────────────────

// APIError is a non-2xx (or success=false) response. Error() is the
// model-facing text and names no internals; Detail() is the log form with the
// API's own message and request ID.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func newAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status}
	var p problem
	if json.Unmarshal(body, &p) == nil {
		e.Code = p.Code
		e.RequestID = p.TraceID
		switch {
		case p.Message != "":
			e.Message = p.Message
		case p.Detail != "":
			e.Message = p.Detail
		case p.Title != "":
			e.Message = p.Title
		case len(p.Errors) > 0:
			e.Message = p.Errors[0].message()
			if e.Code == "" {
				e.Code = p.Errors[0].code()
			}
		}
	}
	if e.Message == "" {
		e.Message = text.Truncate(strings.TrimSpace(string(body)), 300)
	}
	return e
}

// Detail returns the full diagnostic form, for logs.
func (e *APIError) Detail() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "document360 API error: HTTP %d", e.Status)
	if e.Code != "" {
		fmt.Fprintf(&sb, " %s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&sb, ": %s", e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&sb, " (request %s)", e.RequestID)
	}
	return sb.String()
}

// Error returns the sanitized, model-facing form.
func (e *APIError) Error() string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return "Document360 rejected the API key (missing, expired or revoked). This is a configuration matter — retrying will not help; report it as a configuration gap."
	case e.Status == http.StatusForbidden:
		return "the Document360 API key is not allowed to read this resource: its role or content scope excludes it, or the feature is not in the plan. Retrying will not help."
	case e.Status == http.StatusNotFound:
		// The search index outlives deleted and re-scoped articles, so a hit
		// whose ID 404s here is normal. Say plainly that the ID is spent:
		// without that, a model re-requests it and then re-searches, which is
		// several wasted rounds for an article that does not exist.
		return "Document360 cannot return this article: the ID is stale, the article is unpublished in this language, or it is outside the API key's content scope. Do NOT request this ID again — use a different hit from the results you already have."
	case e.Status == http.StatusTooManyRequests:
		return "Document360 rate-limited the request and it did not succeed after several retries. Wait a minute, then retry with fewer or smaller calls."
	case e.Status >= 500:
		return "Document360 returned a temporary server error that did not clear after several retries. Retry shortly."
	case e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity:
		if e.Message != "" {
			return "Document360 rejected the request parameters: " + e.Message
		}
		return "Document360 rejected the request parameters (check lang_code, page and page_size)."
	default:
		return fmt.Sprintf("Document360 returned HTTP %d. Do not retry blindly; report it if it persists.", e.Status)
	}
}

// ErrorDetail returns the log-surface text for err: an *APIError's full
// detail, or the error's own text otherwise.
func ErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Detail()
	}
	return err.Error()
}

// ── Helpers ────────────────────────────────────────────────────────────────

// normalizeLang lowercases a language code, keeping any region suffix upper
// (en, pt-BR). Anything that is not a plausible code is dropped so a bad value
// never reaches the API.
func normalizeLang(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	parts := strings.SplitN(s, "-", 2)
	lang := strings.ToLower(parts[0])
	if len(lang) < 2 || len(lang) > 3 {
		return ""
	}
	for _, r := range lang {
		if r < 'a' || r > 'z' {
			return ""
		}
	}
	if len(parts) == 1 {
		return lang
	}
	region := strings.ToUpper(parts[1])
	if len(region) != 2 {
		return lang
	}
	return lang + "-" + region
}

func clamp(v, lo, hi, def int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}
