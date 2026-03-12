package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSite = "datadoghq.com"

	// Response body size limit for io.LimitReader.
	maxResponseBody = 5 << 20 // 5 MB
)

// Client talks to the Datadog REST API (v1/v2).
type Client struct {
	apiKey     string
	appKey     string
	site       string
	httpClient *http.Client
}

// NewClient creates a Datadog API client. site may be empty (defaults to "datadoghq.com").
func NewClient(apiKey, appKey, site string) *Client {
	if site == "" {
		site = defaultSite
	}
	return &Client{
		apiKey: apiKey,
		appKey: appKey,
		site:   site,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// --------------------------------------------------------------------------
// Public methods
// --------------------------------------------------------------------------

// ListMonitors returns monitors matching an optional query string.
// query can be empty to list all, or use Datadog monitor search syntax (e.g. "tag:env:prod").
func (c *Client) ListMonitors(ctx context.Context, query string, limit int) ([]Monitor, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	params := url.Values{
		"page_size": {fmt.Sprintf("%d", limit)},
	}
	if query != "" {
		params.Set("query", query)
	}
	var monitors []Monitor
	if err := c.get(ctx, "/api/v1/monitor", params, &monitors); err != nil {
		return nil, err
	}
	return monitors, nil
}

// GetMonitor fetches a single monitor by its numeric ID.
func (c *Client) GetMonitor(ctx context.Context, monitorID string) (*Monitor, error) {
	var m Monitor
	if err := c.get(ctx, fmt.Sprintf("/api/v1/monitor/%s", monitorID), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SearchLogs searches Datadog logs using the Log Search API (v2).
func (c *Client) SearchLogs(ctx context.Context, query string, from, to string, limit int) (*LogSearchResponse, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if from == "" {
		// Default: last 1 hour.
		from = time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}

	body := map[string]interface{}{
		"filter": map[string]interface{}{
			"query": query,
			"from":  from,
			"to":    to,
		},
		"sort": "-timestamp",
		"page": map[string]interface{}{
			"limit": limit,
		},
	}

	var resp LogSearchResponse
	if err := c.post(ctx, "/api/v2/logs/events/search", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListHosts returns infrastructure hosts, optionally filtered.
func (c *Client) ListHosts(ctx context.Context, filter string, count int) (*HostListResponse, error) {
	if count <= 0 || count > 100 {
		count = 20
	}
	params := url.Values{
		"count": {fmt.Sprintf("%d", count)},
	}
	if filter != "" {
		params.Set("filter", filter)
	}
	var resp HostListResponse
	if err := c.get(ctx, "/api/v1/hosts", params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetDashboard fetches a dashboard by its ID.
func (c *Client) GetDashboard(ctx context.Context, dashboardID string) (*Dashboard, error) {
	var d Dashboard
	if err := c.get(ctx, fmt.Sprintf("/api/v1/dashboard/%s", dashboardID), nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDashboards lists all dashboards, optionally filtered by query.
func (c *Client) ListDashboards(ctx context.Context, query string, count int) ([]DashboardSummary, error) {
	if count <= 0 || count > 50 {
		count = 20
	}
	params := url.Values{
		"count": {fmt.Sprintf("%d", count)},
	}
	if query != "" {
		params.Set("filter[shared]", "false")
	}
	var resp DashboardListResponse
	if err := c.get(ctx, "/api/v1/dashboard", params, &resp); err != nil {
		return nil, err
	}
	// Client-side filter by query string in title/description.
	if query == "" {
		if len(resp.Dashboards) > count {
			return resp.Dashboards[:count], nil
		}
		return resp.Dashboards, nil
	}
	queryLower := strings.ToLower(query)
	var filtered []DashboardSummary
	for _, d := range resp.Dashboards {
		if strings.Contains(strings.ToLower(d.Title), queryLower) || strings.Contains(strings.ToLower(d.Description), queryLower) {
			filtered = append(filtered, d)
			if len(filtered) >= count {
				break
			}
		}
	}
	return filtered, nil
}

// --------------------------------------------------------------------------
// Formatting helpers
// --------------------------------------------------------------------------

// FormatMonitors returns a Slack-friendly summary of monitors.
func FormatMonitors(monitors []Monitor, site string) string {
	if len(monitors) == 0 {
		return "No monitors found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d monitor(s):\n\n", len(monitors))
	for _, m := range monitors {
		status := m.OverallState
		emoji := ":white_check_mark:"
		switch status {
		case "Alert":
			emoji = ":rotating_light:"
		case "Warn":
			emoji = ":warning:"
		case "No Data":
			emoji = ":grey_question:"
		}
		fmt.Fprintf(&sb, "%s *%s* (ID: %d)\n", emoji, m.Name, m.ID)
		fmt.Fprintf(&sb, "  Status: %s | Type: %s\n", status, m.Type)
		if m.Message != "" {
			msg := m.Message
			if len(msg) > 300 {
				msg = msg[:300] + "…"
			}
			fmt.Fprintf(&sb, "  Message: %s\n", msg)
		}
		if len(m.Tags) > 0 {
			fmt.Fprintf(&sb, "  Tags: %s\n", strings.Join(m.Tags, ", "))
		}
		fmt.Fprintf(&sb, "  URL: <https://app.%s/monitors/%d|View in Datadog>\n\n", site, m.ID)
	}
	return sb.String()
}

// FormatMonitorDetail returns a detailed Slack-friendly summary of a single monitor.
func FormatMonitorDetail(m *Monitor, site string) string {
	var sb strings.Builder
	status := m.OverallState
	emoji := ":white_check_mark:"
	switch status {
	case "Alert":
		emoji = ":rotating_light:"
	case "Warn":
		emoji = ":warning:"
	case "No Data":
		emoji = ":grey_question:"
	}
	fmt.Fprintf(&sb, "%s *%s* (ID: %d)\n", emoji, m.Name, m.ID)
	fmt.Fprintf(&sb, "• Type: %s\n", m.Type)
	fmt.Fprintf(&sb, "• Status: %s\n", status)
	if m.Query != "" {
		q := m.Query
		if len(q) > 500 {
			q = q[:500] + "…"
		}
		fmt.Fprintf(&sb, "• Query: `%s`\n", q)
	}
	if m.Message != "" {
		msg := m.Message
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		fmt.Fprintf(&sb, "• Message:\n%s\n", msg)
	}
	if len(m.Tags) > 0 {
		fmt.Fprintf(&sb, "• Tags: %s\n", strings.Join(m.Tags, ", "))
	}
	if m.Creator != nil {
		fmt.Fprintf(&sb, "• Creator: %s (%s)\n", m.Creator.Name, m.Creator.Email)
	}
	fmt.Fprintf(&sb, "• Created: %s\n", m.Created)
	fmt.Fprintf(&sb, "• Modified: %s\n", m.Modified)
	fmt.Fprintf(&sb, "• URL: <https://app.%s/monitors/%d|View in Datadog>\n", site, m.ID)
	return sb.String()
}

// FormatLogSearch returns a Slack-friendly summary of log search results.
func FormatLogSearch(resp *LogSearchResponse, site string) string {
	if len(resp.Data) == 0 {
		return "No log entries found matching the query."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d log entries:\n\n", len(resp.Data))
	for i, entry := range resp.Data {
		if i >= 30 {
			fmt.Fprintf(&sb, "... and %d more entries (truncated)\n", len(resp.Data)-30)
			break
		}
		attrs := entry.Attributes
		ts := attrs.Timestamp
		svc := attrs.Service
		host := attrs.Host
		status := attrs.Status
		msg := attrs.Message
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}

		statusEmoji := ""
		switch status {
		case "error", "critical", "emergency", "alert":
			statusEmoji = ":red_circle:"
		case "warn", "warning":
			statusEmoji = ":large_yellow_circle:"
		default:
			statusEmoji = ":large_blue_circle:"
		}

		fmt.Fprintf(&sb, "%s `%s` | service:%s | host:%s | %s\n", statusEmoji, ts, svc, host, status)
		if msg != "" {
			fmt.Fprintf(&sb, "  %s\n", msg)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatHosts returns a Slack-friendly summary of infrastructure hosts.
func FormatHosts(resp *HostListResponse, site string) string {
	if len(resp.HostList) == 0 {
		return "No hosts found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Infrastructure hosts (%d total, showing %d):\n\n", resp.TotalReturned, len(resp.HostList))
	for _, h := range resp.HostList {
		upEmoji := ":green_circle:"
		if !h.IsUp {
			upEmoji = ":red_circle:"
		}
		fmt.Fprintf(&sb, "%s *%s*\n", upEmoji, h.Name)
		if len(h.Apps) > 0 {
			fmt.Fprintf(&sb, "  Apps: %s\n", strings.Join(h.Apps, ", "))
		}
		if len(h.TagsBySource) > 0 {
			for src, tags := range h.TagsBySource {
				if len(tags) > 10 {
					tags = tags[:10]
				}
				fmt.Fprintf(&sb, "  Tags (%s): %s\n", src, strings.Join(tags, ", "))
			}
		}
		if h.Meta != nil {
			if h.Meta.Platform != "" {
				fmt.Fprintf(&sb, "  Platform: %s\n", h.Meta.Platform)
			}
			if h.Meta.InstanceID != "" {
				fmt.Fprintf(&sb, "  Instance: %s (%s)\n", h.Meta.InstanceID, h.Meta.InstanceType)
			}
		}
		fmt.Fprintf(&sb, "  URL: <https://app.%s/infrastructure?host=%s|View in Datadog>\n\n", site, url.QueryEscape(h.Name))
	}
	return sb.String()
}

// FormatDashboard returns a Slack-friendly summary of a dashboard.
func FormatDashboard(d *Dashboard, site string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Dashboard: %s*\n", d.Title)
	if d.Description != "" {
		fmt.Fprintf(&sb, "Description: %s\n", d.Description)
	}
	fmt.Fprintf(&sb, "Layout: %s\n", d.LayoutType)
	if d.AuthorHandle != "" {
		fmt.Fprintf(&sb, "Author: %s\n", d.AuthorHandle)
	}
	fmt.Fprintf(&sb, "URL: <https://app.%s/dashboard/%s|View in Datadog>\n", site, d.ID)

	if len(d.Widgets) > 0 {
		fmt.Fprintf(&sb, "\nWidgets (%d):\n", len(d.Widgets))
		limit := 20
		if len(d.Widgets) < limit {
			limit = len(d.Widgets)
		}
		for _, w := range d.Widgets[:limit] {
			title := "(untitled)"
			if w.Definition != nil {
				if t, ok := w.Definition["title"].(string); ok && t != "" {
					title = t
				}
				wType, _ := w.Definition["type"].(string)
				fmt.Fprintf(&sb, "  • %s (%s)\n", title, wType)
			} else {
				fmt.Fprintf(&sb, "  • %s\n", title)
			}
		}
		if len(d.Widgets) > limit {
			fmt.Fprintf(&sb, "  ... and %d more widgets\n", len(d.Widgets)-limit)
		}
	}
	return sb.String()
}

// FormatDashboardList returns a Slack-friendly summary of dashboards.
func FormatDashboardList(dashboards []DashboardSummary, site string) string {
	if len(dashboards) == 0 {
		return "No dashboards found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Dashboards (%d):\n\n", len(dashboards))
	for _, d := range dashboards {
		fmt.Fprintf(&sb, "• *%s* (ID: %s)\n", d.Title, d.ID)
		if d.Description != "" {
			desc := d.Description
			if len(desc) > 150 {
				desc = desc[:150] + "…"
			}
			fmt.Fprintf(&sb, "  %s\n", desc)
		}
		fmt.Fprintf(&sb, "  Layout: %s | Author: %s\n", d.LayoutType, d.AuthorHandle)
		fmt.Fprintf(&sb, "  URL: <https://app.%s/dashboard/%s|View in Datadog>\n\n", site, d.ID)
	}
	return sb.String()
}

// --------------------------------------------------------------------------
// HTTP transport
// --------------------------------------------------------------------------

func (c *Client) baseURL() string {
	return fmt.Sprintf("https://api.%s", c.site)
}

func (c *Client) get(ctx context.Context, path string, params url.Values, target interface{}) error {
	u := c.baseURL() + path
	if params != nil {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("datadog API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("datadog API returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, payload interface{}, target interface{}) error {
	u := c.baseURL() + path

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("datadog API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("datadog API returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("Accept", "application/json")
}

// Site returns the configured Datadog site (e.g. "datadoghq.com").
func (c *Client) Site() string {
	return c.site
}

// SiteLabel returns a short label for the site ("US" or "EU").
func (c *Client) SiteLabel() string {
	if c.site == "datadoghq.eu" {
		return "EU"
	}
	return "US"
}

// --------------------------------------------------------------------------
// MultiClient — wraps up to two site-specific clients (US + EU)
// --------------------------------------------------------------------------

// MultiClient holds Datadog clients for US and/or EU sites and routes
// requests based on an inferred or explicit site parameter.
type MultiClient struct {
	US *Client // datadoghq.com — may be nil
	EU *Client // datadoghq.eu — may be nil
}

// NewMultiClient creates a MultiClient with clients for sites that have credentials.
func NewMultiClient(apiKeyUS, appKeyUS, apiKeyEU, appKeyEU string) *MultiClient {
	mc := &MultiClient{}
	if apiKeyUS != "" && appKeyUS != "" {
		mc.US = NewClient(apiKeyUS, appKeyUS, "datadoghq.com")
	}
	if apiKeyEU != "" && appKeyEU != "" {
		mc.EU = NewClient(apiKeyEU, appKeyEU, "datadoghq.eu")
	}
	return mc
}

// Sites returns a human-readable summary of configured sites.
func (mc *MultiClient) Sites() string {
	var s []string
	if mc.US != nil {
		s = append(s, "US (datadoghq.com)")
	}
	if mc.EU != nil {
		s = append(s, "EU (datadoghq.eu)")
	}
	return strings.Join(s, ", ")
}

// InferSite examines text for Datadog site hints (URLs or domain references)
// and returns "us", "eu", or "" (unknown — query both).
func InferSite(text string) string {
	lower := strings.ToLower(text)
	hasEU := strings.Contains(lower, "datadoghq.eu")
	hasUS := strings.Contains(lower, "datadoghq.com")
	if hasEU && !hasUS {
		return "eu"
	}
	if hasUS && !hasEU {
		return "us"
	}
	return ""
}

// clients returns the Client(s) to query based on the requested site.
// site: "us", "eu", or "" (all configured).
func (mc *MultiClient) clients(site string) []*Client {
	switch strings.ToLower(site) {
	case "us":
		if mc.US != nil {
			return []*Client{mc.US}
		}
	case "eu":
		if mc.EU != nil {
			return []*Client{mc.EU}
		}
	default:
		var cs []*Client
		if mc.US != nil {
			cs = append(cs, mc.US)
		}
		if mc.EU != nil {
			cs = append(cs, mc.EU)
		}
		return cs
	}
	return nil
}

// SearchLogs queries logs on the target site(s) and returns formatted output.
func (mc *MultiClient) SearchLogs(ctx context.Context, site, query, from, to string, limit int) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		resp, err := c.SearchLogs(ctx, query, from, to, limit)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		formatted := FormatLogSearch(resp, c.Site())
		if multi {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		parts = append(parts, formatted)
	}
	if len(parts) == 0 {
		return "", lastErr
	}
	return strings.Join(parts, "\n"), nil
}

// ListMonitors queries monitors on the target site(s) and returns formatted output.
func (mc *MultiClient) ListMonitors(ctx context.Context, site, query string, limit int) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		monitors, err := c.ListMonitors(ctx, query, limit)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		formatted := FormatMonitors(monitors, c.Site())
		if multi {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		parts = append(parts, formatted)
	}
	if len(parts) == 0 {
		return "", lastErr
	}
	return strings.Join(parts, "\n"), nil
}

// GetMonitor fetches a monitor by ID. When site is empty, tries both and returns the first success.
func (mc *MultiClient) GetMonitor(ctx context.Context, site, monitorID string) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	var errs []string
	for _, c := range cs {
		m, err := c.GetMonitor(ctx, monitorID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("[%s] %v", c.SiteLabel(), err))
			continue
		}
		formatted := FormatMonitorDetail(m, c.Site())
		if len(cs) > 1 {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		return formatted, nil
	}
	return "", fmt.Errorf("monitor %s not found: %s", monitorID, strings.Join(errs, "; "))
}

// ListHosts queries hosts on the target site(s) and returns formatted output.
func (mc *MultiClient) ListHosts(ctx context.Context, site, filter string, count int) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		resp, err := c.ListHosts(ctx, filter, count)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		formatted := FormatHosts(resp, c.Site())
		if multi {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		parts = append(parts, formatted)
	}
	if len(parts) == 0 {
		return "", lastErr
	}
	return strings.Join(parts, "\n"), nil
}

// GetDashboard fetches a dashboard by ID. When site is empty, tries both and returns the first success.
func (mc *MultiClient) GetDashboard(ctx context.Context, site, dashboardID string) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	var errs []string
	for _, c := range cs {
		d, err := c.GetDashboard(ctx, dashboardID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("[%s] %v", c.SiteLabel(), err))
			continue
		}
		formatted := FormatDashboard(d, c.Site())
		if len(cs) > 1 {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		return formatted, nil
	}
	return "", fmt.Errorf("dashboard %s not found: %s", dashboardID, strings.Join(errs, "; "))
}

// ListDashboards queries dashboards on the target site(s) and returns formatted output.
func (mc *MultiClient) ListDashboards(ctx context.Context, site, query string, count int) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		dashboards, err := c.ListDashboards(ctx, query, count)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		formatted := FormatDashboardList(dashboards, c.Site())
		if multi {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		parts = append(parts, formatted)
	}
	if len(parts) == 0 {
		return "", lastErr
	}
	return strings.Join(parts, "\n"), nil
}
