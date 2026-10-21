package grafana

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 100
)

// SearchOptions narrows a dashboard search.
type SearchOptions struct {
	Query      string
	Tags       []string
	FolderUIDs []string
	Limit      int
	Page       int
}

// SearchDashboards finds dashboards by title substring, tags and folders.
func (c *Client) SearchDashboards(ctx context.Context, opts SearchOptions) ([]SearchHit, error) {
	q := url.Values{"type": {"dash-db"}}
	if s := strings.TrimSpace(opts.Query); s != "" {
		q.Set("query", s)
	}
	for _, t := range opts.Tags {
		if t = strings.TrimSpace(t); t != "" {
			q.Add("tag", t)
		}
	}
	for _, f := range opts.FolderUIDs {
		if f = strings.TrimSpace(f); f != "" {
			q.Add("folderUIDs", f)
		}
	}
	q.Set("limit", strconv.Itoa(clamp(opts.Limit, 1, maxSearchLimit, defaultSearchLimit)))
	if opts.Page > 1 {
		q.Set("page", strconv.Itoa(opts.Page))
	}
	return c.search(ctx, q)
}

// SearchFolders finds folders by title substring.
func (c *Client) SearchFolders(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	q := url.Values{"type": {"dash-folder"}}
	if s := strings.TrimSpace(query); s != "" {
		q.Set("query", s)
	}
	q.Set("limit", strconv.Itoa(clamp(limit, 1, maxSearchLimit, defaultSearchLimit)))
	return c.search(ctx, q)
}

func (c *Client) search(ctx context.Context, q url.Values) ([]SearchHit, error) {
	var hits []SearchHit
	if err := c.get(ctx, "/api/search", q, &hits); err != nil {
		return nil, err
	}
	for i := range hits {
		hits[i].URL = c.link(hits[i].URL)
	}
	return hits, nil
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
