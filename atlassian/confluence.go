package atlassian

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
func (c *Client) ListConfluenceSpaces() ([]ConfluenceSpace, error) {
	base := c.confluenceBaseURL()
	endpoint := fmt.Sprintf("%s/api/v2/spaces?limit=50&sort=name", base)

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
		return nil, fmt.Errorf("list spaces request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list spaces returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Results []ConfluenceSpace `json:"results"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse spaces: %w", err)
	}
	return result.Results, nil
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

	respBody, err := io.ReadAll(resp.Body)
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

	respBody, err := io.ReadAll(resp.Body)
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
