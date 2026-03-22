package atlassian

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Confluence REST API v2
// ---------------------------------------------------------------------------
// Confluence shares the same Atlassian OAuth token and cloud ID as Jira.
// For Basic Auth the site URL is the same; for OAuth the base URL differs
// (api.atlassian.com/ex/confluence/<cloudID>).

// confluenceBaseURL returns the REST API base URL for Confluence.
func (c *Client) confluenceBaseURL() string {
	switch c.mode {
	case authOAuth:
		c.tokenMu.RLock()
		cid := c.cloudID
		c.tokenMu.RUnlock()
		return fmt.Sprintf("%s/%s", atlassianConfluenceOAuthBase, cid)
	default: // authBasic
		return strings.TrimRight(c.siteURL, "/") + "/wiki"
	}
}

// ConfluenceSpace represents a Confluence space.
type ConfluenceSpace struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ListConfluenceSpaces returns all Confluence spaces the authenticated user can see.
// Paginates using cursor-based _links.next from the Confluence v2 API.
func (c *Client) ListConfluenceSpaces() ([]ConfluenceSpace, error) {
	base := c.confluenceBaseURL()
	nextURL := fmt.Sprintf("%s/api/v2/spaces?limit=%d&sort=name", base, confluenceSpacesPageSize)

	var allSpaces []ConfluenceSpace
	for nextURL != "" {
		req, err := http.NewRequest("GET", nextURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if err := c.authRequest(req); err != nil {
			return nil, err
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list spaces request failed: %w", err)
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list spaces returned HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		var result struct {
			Results []ConfluenceSpace `json:"results"`
			Links   struct {
				Next string `json:"next"`
			} `json:"_links"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("parse spaces: %w", err)
		}

		allSpaces = append(allSpaces, result.Results...)

		if result.Links.Next == "" || len(result.Results) == 0 {
			break
		}
		// _links.next is a relative path; prepend the base URL.
		nextURL = base + result.Links.Next
	}

	return allSpaces, nil
}

// ConfluenceSearchResult represents a page returned by the Confluence search (CQL).
type ConfluenceSearchResult struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Type   string `json:"type"`
	Status string `json:"status"`
	WebURL string `json:"-"`
}

// SearchConfluencePages searches Confluence using CQL and returns matching pages.
func (c *Client) SearchConfluencePages(cql string, limit int) ([]ConfluenceSearchResult, error) {
	if limit <= 0 || limit > 25 {
		limit = 25
	}
	base := c.confluenceBaseURL()
	qs := url.Values{
		"cql":   {cql},
		"limit": {fmt.Sprintf("%d", limit)},
	}
	endpoint := fmt.Sprintf("%s/rest/api/content/search?%s", base, qs.Encode())

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search confluence request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence search returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var raw struct {
		Results []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Type   string `json:"type"`
			Status string `json:"status"`
			Links  struct {
				WebUI string `json:"webui"`
				Base  string `json:"base"`
			} `json:"_links"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}

	results := make([]ConfluenceSearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		webURL := r.Links.WebUI
		base := r.Links.Base
		if base == "" {
			base = strings.TrimRight(c.siteURL, "/") + "/wiki"
		}
		if webURL != "" {
			webURL = base + webURL
		}
		results = append(results, ConfluenceSearchResult{
			ID:     r.ID,
			Title:  r.Title,
			Type:   r.Type,
			Status: r.Status,
			WebURL: webURL,
		})
	}
	return results, nil
}

// ConfluencePage holds the content of a single Confluence page.
type ConfluencePage struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	SpaceID string `json:"spaceId"`
	Status  string `json:"status"`
	Body    string `json:"-"`
	WebURL  string `json:"-"`
	Version int    `json:"-"`
}

// GetConfluencePage retrieves a Confluence page by its ID, returning the body
// in storage format (Confluence-flavoured XHTML).
func (c *Client) GetConfluencePage(pageID string) (*ConfluencePage, error) {
	base := c.confluenceBaseURL()
	endpoint := fmt.Sprintf("%s/api/v2/pages/%s?body-format=storage", base, url.PathEscape(pageID))

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get page request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get page returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var raw struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		SpaceID string `json:"spaceId"`
		Status  string `json:"status"`
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
		Body struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
		Links struct {
			WebUI string `json:"webui"`
			Base  string `json:"base"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("parse page: %w", err)
	}

	webURL := raw.Links.WebUI
	base = raw.Links.Base
	if base == "" {
		base = strings.TrimRight(c.siteURL, "/")
	}
	if webURL != "" {
		webURL = base + webURL
	}

	return &ConfluencePage{
		ID:      raw.ID,
		Title:   raw.Title,
		SpaceID: raw.SpaceID,
		Status:  raw.Status,
		Body:    raw.Body.Storage.Value,
		WebURL:  webURL,
		Version: raw.Version.Number,
	}, nil
}

// GetConfluencePageByTitle finds a page in a space by its exact title.
// Returns nil (not an error) if no page matches.
func (c *Client) GetConfluencePageByTitle(spaceKey, title string) (*ConfluencePage, error) {
	cql := fmt.Sprintf(`space = "%s" AND title = "%s" AND type = page`, spaceKey, title)
	results, err := c.SearchConfluencePages(cql, 1)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return c.GetConfluencePage(results[0].ID)
}

// CreateConfluencePageInput holds the parameters for creating a new Confluence page.
type CreateConfluencePageInput struct {
	SpaceKey string // Confluence space key (e.g. "DO", "ENG")
	Title    string // Page title
	Body     string // Page body in Confluence storage format (XHTML)
	ParentID string // Optional parent page ID for nesting under a specific page
}

// CreateConfluencePageResult holds the output of a successful page creation.
type CreateConfluencePageResult struct {
	ID     string
	Title  string
	WebURL string
}

// CreateConfluencePage creates a new Confluence page via the v2 REST API.
func (c *Client) CreateConfluencePage(input CreateConfluencePageInput) (*CreateConfluencePageResult, error) {
	// Resolve space key → space ID (Confluence v2 API requires spaceId).
	spaceID, err := c.resolveConfluenceSpaceID(input.SpaceKey)
	if err != nil {
		return nil, fmt.Errorf("resolve space %q: %w", input.SpaceKey, err)
	}

	base := c.confluenceBaseURL()

	payload := map[string]interface{}{
		"spaceId": spaceID,
		"status":  "current",
		"title":   input.Title,
		"body": map[string]interface{}{
			"representation": "storage",
			"value":          input.Body,
		},
	}
	if input.ParentID != "" {
		payload["parentId"] = input.ParentID
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v2/pages", base)
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := c.authRequest(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create page request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("create page returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var raw struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Links struct {
			WebUI string `json:"webui"`
			Base  string `json:"base"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("parse create page response: %w", err)
	}

	webURL := raw.Links.WebUI
	linkBase := raw.Links.Base
	if linkBase == "" {
		linkBase = strings.TrimRight(c.siteURL, "/")
	}
	if webURL != "" {
		webURL = linkBase + webURL
	}

	return &CreateConfluencePageResult{
		ID:     raw.ID,
		Title:  raw.Title,
		WebURL: webURL,
	}, nil
}

// resolveConfluenceSpaceID maps a space key (e.g. "DO") to its numeric space ID.
func (c *Client) resolveConfluenceSpaceID(spaceKey string) (string, error) {
	spaces, err := c.ListConfluenceSpaces()
	if err != nil {
		return "", err
	}
	for _, s := range spaces {
		if strings.EqualFold(s.Key, spaceKey) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("confluence space with key %q not found", spaceKey)
}

// ---------------------------------------------------------------------------
// Confluence URL / Tiny-Link Resolver
// ---------------------------------------------------------------------------

// ResolveConfluencePageID extracts a numeric Confluence page ID from various
// input formats:
//   - Numeric ID: "123456" → "123456"
//   - Tiny link URL: https://site.atlassian.net/wiki/x/AQAF3Q → decoded page ID
//   - Full page URL: .../spaces/SPACE/pages/123456/... → "123456"
func ResolveConfluencePageID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("empty page ID or URL")
	}

	// Already a numeric ID.
	if _, err := strconv.ParseInt(input, 10, 64); err == nil {
		return input, nil
	}

	// Try to parse as URL.
	u, err := url.Parse(input)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("not a valid page ID or Confluence URL: %s", input)
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")

	// Tiny link: /wiki/x/<code>  or  /x/<code>
	for i, seg := range segments {
		if seg == "x" && i+1 < len(segments) {
			code := segments[i+1]
			id, err := decodeTinyLink(code)
			if err != nil {
				return "", fmt.Errorf("failed to decode Confluence tiny link %q: %w", code, err)
			}
			return strconv.FormatInt(id, 10), nil
		}
	}

	// Full URL: .../pages/<id>/...
	for i, seg := range segments {
		if seg == "pages" && i+1 < len(segments) {
			candidate := segments[i+1]
			if _, err := strconv.ParseInt(candidate, 10, 64); err == nil {
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("could not extract Confluence page ID from URL: %s", input)
}

// decodeTinyLink decodes a Confluence tiny-link code (base64-encoded page ID).
// Confluence encodes the content ID as little-endian bytes, base64-encodes them,
// and strips trailing '=' padding.
func decodeTinyLink(code string) (int64, error) {
	// Re-add base64 padding.
	padded := code
	if m := len(padded) % 4; m != 0 {
		padded += strings.Repeat("=", 4-m)
	}

	data, err := base64.StdEncoding.DecodeString(padded)
	if err != nil {
		// Fall back to URL-safe alphabet.
		data, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return 0, fmt.Errorf("base64 decode failed: %w", err)
		}
	}

	if len(data) == 0 || len(data) > 8 {
		return 0, fmt.Errorf("unexpected decoded length %d bytes", len(data))
	}

	// Little-endian bytes → int64.
	var id int64
	for i := len(data) - 1; i >= 0; i-- {
		id = (id << 8) | int64(data[i])
	}
	if id <= 0 {
		return 0, fmt.Errorf("decoded page ID is not positive: %d", id)
	}
	return id, nil
}
