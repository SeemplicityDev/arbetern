package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSite = "datadoghq.com"

	// Response body size limit for io.LimitReader.
	// Metric queries across large kube_app_name fanouts can easily exceed
	// 5 MiB; use a generous cap to avoid truncating the JSON mid-stream.
	maxResponseBody = 64 << 20 // 64 MiB
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

// maxLogAttrLeaves caps how many flattened attribute leaves render per log
// entry so a deeply nested event can't blow up the output.
const maxLogAttrLeaves = 100

// flattenLogAttrs walks a nested log attribute value into sorted
// "dotted.path:value" leaves appended to out, so nested fields are readable in
// results, not only matchable in queries.
func flattenLogAttrs(prefix string, v interface{}, out *[]string) {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := k
			if prefix != "" {
				child = prefix + "." + k
			}
			flattenLogAttrs(child, t[k], out)
		}
	case []interface{}:
		for i, item := range t {
			flattenLogAttrs(fmt.Sprintf("%s.%d", prefix, i), item, out)
		}
	case nil:
		return
	default:
		s := fmt.Sprintf("%v", t)
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		*out = append(*out, prefix+":"+s)
	}
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
		if len(attrs.Tags) > 0 {
			fmt.Fprintf(&sb, "  %s\n", strings.Join(attrs.Tags, " "))
		}
		if len(attrs.Attributes) > 0 {
			parts := make([]string, 0, len(attrs.Attributes))
			flattenLogAttrs("", attrs.Attributes, &parts)
			if len(parts) > maxLogAttrLeaves {
				extra := len(parts) - maxLogAttrLeaves
				parts = append(parts[:maxLogAttrLeaves], fmt.Sprintf("(+%d more)", extra))
			}
			if len(parts) > 0 {
				fmt.Fprintf(&sb, "  attrs: %s\n", strings.Join(parts, " "))
			}
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

// Rate-limit / retry tuning for the Datadog transport.
const (
	// ddMaxRetries bounds retry attempts on 429 and 5xx responses.
	ddMaxRetries = 4
	// ddBaseRetryWait is the initial backoff, doubled each attempt.
	ddBaseRetryWait = 1 * time.Second
	// ddMaxRetryWait caps a single backoff wait.
	ddMaxRetryWait = 30 * time.Second
)

// doRequest issues an HTTP request and returns its body and response, retrying
// on 429 (rate limit) and 5xx with backoff. Datadog enforces per-org rate
// limits — the smaller EU org 429s readily under the perf-baseline report's
// burst of aggregate calls — and signals the cooldown via X-RateLimit-Reset
// (seconds until reset); doRequest honours that header, then Retry-After, then
// an exponential backoff. bodyBytes is nil for GET and re-read on each attempt;
// logPath is logged on retry (secrets travel in headers, not the URL).
func (c *Client) doRequest(ctx context.Context, method, u, logPath string, bodyBytes []byte) ([]byte, *http.Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
		if err != nil {
			return nil, nil, fmt.Errorf("creating request: %w", err)
		}
		c.setHeaders(req)
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("datadog API request failed: %w", err)
			if attempt >= ddMaxRetries || !ctxSleep(ctx, ddRetryWait(nil, attempt)) {
				return nil, nil, lastErr
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("reading response body: %w", readErr)
		}

		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if retryable && attempt < ddMaxRetries {
			wait := ddRetryWait(resp, attempt)
			log.Printf("[datadog:%s] %s %s -> %d, retrying in %s (attempt %d/%d)",
				c.SiteLabel(), method, logPath, resp.StatusCode, wait, attempt+1, ddMaxRetries)
			if ctxSleep(ctx, wait) {
				continue
			}
			return nil, nil, ctx.Err()
		}
		return body, resp, nil
	}
}

// ddRetryWait is the backoff before the next attempt: Datadog's
// X-RateLimit-Reset (seconds until the limit resets) when present, else a
// Retry-After header, else exponential backoff. Clamped to ddMaxRetryWait.
func ddRetryWait(resp *http.Response, attempt int) time.Duration {
	wait := ddBaseRetryWait << attempt
	if resp != nil {
		for _, h := range []string{"X-RateLimit-Reset", "Retry-After"} {
			if v := strings.TrimSpace(resp.Header.Get(h)); v != "" {
				if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
					wait = time.Duration(secs) * time.Second
					break
				}
			}
		}
	}
	if wait <= 0 {
		wait = ddBaseRetryWait
	}
	if wait > ddMaxRetryWait {
		wait = ddMaxRetryWait
	}
	return wait
}

// ctxSleep waits for d or until ctx is cancelled, returning true if the full
// wait elapsed and false if ctx was cancelled first.
func ctxSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Client) get(ctx context.Context, path string, params url.Values, target interface{}) error {
	u := c.baseURL() + path
	if params != nil {
		u += "?" + params.Encode()
	}

	body, resp, err := c.doRequest(ctx, http.MethodGet, u, path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("datadog API returned %d: %s", resp.StatusCode, string(body))
	}
	return decodeJSON(resp, body, target)
}

func (c *Client) post(ctx context.Context, path string, payload interface{}, target interface{}) error {
	u := c.baseURL() + path

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding request body: %w", err)
	}

	body, resp, err := c.doRequest(ctx, http.MethodPost, u, path, bodyBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("datadog API returned %d: %s", resp.StatusCode, string(body))
	}
	return decodeJSON(resp, body, target)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "arbetern/1.0")
}

// decodeJSON unmarshals body into target, returning a diagnostic error when
// the response is empty or not valid JSON. The HTTP status, content-length
// header, and a short body preview are included to ease debugging of
// intermittent upstream issues (e.g. empty 2xx responses from Datadog).
func decodeJSON(resp *http.Response, body []byte, target interface{}) error {
	if len(body) == 0 {
		return fmt.Errorf("datadog returned empty body (status=%d, content-length=%s)", resp.StatusCode, resp.Header.Get("Content-Length"))
	}
	if err := json.Unmarshal(body, target); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		return fmt.Errorf("decoding response (status=%d, %d bytes): %w: %s", resp.StatusCode, len(body), err, preview)
	}
	return nil
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

// --------------------------------------------------------------------------
// Metrics query (v1 /api/v1/query)
// --------------------------------------------------------------------------

// QueryMetrics runs a Datadog timeseries metrics query. `query` must be a full
// Datadog metric query expression (e.g.
// "avg:kubernetes.cpu.usage.total{*} by {kube_service}"). `from`/`to` accept
// either ISO-8601 (e.g. "2026-04-19T10:00:00Z"), unix seconds, or a relative
// shorthand ("-1h", "-15m", "-7d"). Empty `from` defaults to -1h, empty `to`
// defaults to now.
func (c *Client) QueryMetrics(ctx context.Context, query, from, to string) (*MetricsQueryResponse, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	fromUnix, err := parseTimeArg(from, time.Now().Add(-1*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("invalid from: %w", err)
	}
	toUnix, err := parseTimeArg(to, time.Now())
	if err != nil {
		return nil, fmt.Errorf("invalid to: %w", err)
	}
	if fromUnix >= toUnix {
		return nil, fmt.Errorf("from (%d) must be earlier than to (%d)", fromUnix, toUnix)
	}
	params := url.Values{
		"from":  {fmt.Sprintf("%d", fromUnix)},
		"to":    {fmt.Sprintf("%d", toUnix)},
		"query": {query},
	}
	var resp MetricsQueryResponse
	// Datadog occasionally returns an empty 2xx body (typically a transient
	// upstream hiccup); retry once before surfacing the error.
	err = c.get(ctx, "/api/v1/query", params, &resp)
	if err != nil && strings.Contains(err.Error(), "empty body") {
		time.Sleep(250 * time.Millisecond)
		resp = MetricsQueryResponse{}
		err = c.get(ctx, "/api/v1/query", params, &resp)
	}
	if err != nil {
		return nil, err
	}
	if resp.Status != "" && resp.Status != "ok" {
		msg := resp.Error
		if msg == "" {
			msg = resp.Message
		}
		return &resp, fmt.Errorf("datadog query status=%s: %s", resp.Status, msg)
	}
	return &resp, nil
}

// QueryMetrics runs a metrics query across one or both sites and returns a
// formatted result. `site` is "us", "eu", or "" for all configured sites.
func (mc *MultiClient) QueryMetrics(ctx context.Context, site, query, from, to string) (string, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return "", fmt.Errorf("no Datadog client configured for site %q", site)
	}
	multi := len(cs) > 1
	var parts []string
	var lastErr error
	for _, c := range cs {
		resp, err := c.QueryMetrics(ctx, query, from, to)
		if err != nil {
			lastErr = err
			if multi {
				parts = append(parts, fmt.Sprintf("*[%s]* Error: %v", c.SiteLabel(), err))
			}
			continue
		}
		formatted := FormatMetricsQuery(resp, c.Site())
		if multi {
			formatted = fmt.Sprintf("*[%s]*\n%s", c.SiteLabel(), formatted)
		}
		parts = append(parts, formatted)
	}
	if len(parts) == 0 {
		return "", lastErr
	}
	return strings.Join(parts, "\n\n"), nil
}

// MetricsChartSeries is one pre-aggregated series rendered as a line in a chart.
// Points is [[timestamp_ms, value], ...] with null-valued points dropped.
type MetricsChartSeries struct {
	Scope  string       `json:"scope"`
	Unit   string       `json:"unit,omitempty"`
	Avg    float64      `json:"avg"`
	Min    float64      `json:"min"`
	Max    float64      `json:"max"`
	Last   float64      `json:"last"`
	Count  int          `json:"count"`
	Points [][2]float64 `json:"points"`
}

// MetricsChartSite is the per-site payload the dashboard viewer renders as a chart.
type MetricsChartSite struct {
	Site      string               `json:"site"`
	Label     string               `json:"label"`
	Query     string               `json:"query"`
	FromDate  int64                `json:"from_date"`
	ToDate    int64                `json:"to_date"`
	Unit      string               `json:"unit,omitempty"`
	Message   string               `json:"message,omitempty"`
	Error     string               `json:"error,omitempty"`
	Series    []MetricsChartSeries `json:"series"`
	Truncated int                  `json:"truncated,omitempty"`
}

// MetricsChartPayload is the full structured result the dashboard viewer expects.
// The "kind": "datadog_metrics" marker lets the frontend detect and draw charts.
type MetricsChartPayload struct {
	Kind  string             `json:"kind"`
	Sites []MetricsChartSite `json:"sites"`
}

// QueryMetricsRaw runs a metrics query across one or both sites and returns a
// structured, chart-friendly result. Each site's series are sorted by avg desc
// and capped at topN (default 20). Points are filtered to drop Datadog's null
// buckets so the frontend can plot them directly.
func (mc *MultiClient) QueryMetricsRaw(ctx context.Context, site, query, from, to string, topN int) (*MetricsChartPayload, error) {
	cs := mc.clients(site)
	if len(cs) == 0 {
		return nil, fmt.Errorf("no Datadog client configured for site %q", site)
	}
	if topN <= 0 {
		topN = 20
	}
	if topN > 100 {
		topN = 100
	}
	payload := &MetricsChartPayload{Kind: "datadog_metrics"}
	var lastErr error
	for _, c := range cs {
		siteEntry := MetricsChartSite{
			Site:  c.Site(),
			Label: c.SiteLabel(),
			Query: query,
		}
		resp, err := c.QueryMetrics(ctx, query, from, to)
		if err != nil {
			siteEntry.Error = err.Error()
			lastErr = err
			payload.Sites = append(payload.Sites, siteEntry)
			continue
		}
		siteEntry.FromDate = resp.FromDate
		siteEntry.ToDate = resp.ToDate
		siteEntry.Message = resp.Message
		for _, s := range resp.Series {
			cs := MetricsChartSeries{Scope: s.Scope, Unit: seriesUnit(s)}
			if cs.Scope == "" {
				cs.Scope = "*"
			}
			var sum float64
			first := true
			pts := make([][2]float64, 0, len(s.PointList))
			for _, pt := range s.PointList {
				if len(pt) < 2 || pt[0] == nil || pt[1] == nil {
					continue
				}
				ts, v := *pt[0], *pt[1]
				if first {
					cs.Min, cs.Max = v, v
					first = false
				} else {
					if v < cs.Min {
						cs.Min = v
					}
					if v > cs.Max {
						cs.Max = v
					}
				}
				sum += v
				cs.Last = v
				cs.Count++
				pts = append(pts, [2]float64{ts, v})
			}
			if cs.Count > 0 {
				cs.Avg = sum / float64(cs.Count)
			}
			cs.Points = pts
			if siteEntry.Unit == "" && cs.Unit != "" {
				siteEntry.Unit = cs.Unit
			}
			siteEntry.Series = append(siteEntry.Series, cs)
		}
		// Sort by avg desc (insertion sort, same style as FormatMetricsQuery).
		for i := 1; i < len(siteEntry.Series); i++ {
			for j := i; j > 0 && siteEntry.Series[j].Avg > siteEntry.Series[j-1].Avg; j-- {
				siteEntry.Series[j], siteEntry.Series[j-1] = siteEntry.Series[j-1], siteEntry.Series[j]
			}
		}
		if len(siteEntry.Series) > topN {
			siteEntry.Truncated = len(siteEntry.Series) - topN
			siteEntry.Series = siteEntry.Series[:topN]
		}
		payload.Sites = append(payload.Sites, siteEntry)
	}
	if len(payload.Sites) == 0 {
		return nil, lastErr
	}
	return payload, nil
}

// parseTimeArg accepts ISO-8601, unix seconds, or relative shorthand like
// "-1h", "-30m", "-7d". Empty falls back to the provided default.
func parseTimeArg(v string, def time.Time) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def.Unix(), nil
	}
	if strings.HasPrefix(v, "-") || strings.HasPrefix(v, "+") {
		if d, err := time.ParseDuration(v); err == nil {
			return time.Now().Add(d).Unix(), nil
		}
	}
	if len(v) >= 8 && strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("unrecognized time format %q (use ISO-8601, unix seconds, or a duration like -1h)", v)
}

// FormatMetricsQuery renders a metrics query response into a Slack-friendly
// summary: per-series aggregates (avg/min/max/last) sorted by avg desc.
func FormatMetricsQuery(resp *MetricsQueryResponse, site string) string {
	if resp == nil {
		return "No response."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Query: `%s`\n", resp.Query)
	if resp.FromDate > 0 && resp.ToDate > 0 {
		fromT := time.Unix(resp.FromDate/1000, 0).UTC().Format(time.RFC3339)
		toT := time.Unix(resp.ToDate/1000, 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(&sb, "Window: %s → %s\n", fromT, toT)
	}
	if resp.Message != "" {
		fmt.Fprintf(&sb, "Message: %s\n", resp.Message)
	}
	if len(resp.Series) == 0 {
		sb.WriteString("\nNo series returned. The query matched zero metric data in this window.")
		return sb.String()
	}

	type row struct {
		scope               string
		unit                string
		min, avg, max, last float64
		count               int
	}
	rows := make([]row, 0, len(resp.Series))
	for _, s := range resp.Series {
		r := row{scope: s.Scope, unit: seriesUnit(s)}
		if r.scope == "" {
			r.scope = "*"
		}
		var sum float64
		first := true
		for _, pt := range s.PointList {
			if len(pt) < 2 || pt[1] == nil {
				continue
			}
			v := *pt[1]
			if first {
				r.min, r.max = v, v
				first = false
			} else {
				if v < r.min {
					r.min = v
				}
				if v > r.max {
					r.max = v
				}
			}
			sum += v
			r.last = v
			r.count++
		}
		if r.count > 0 {
			r.avg = sum / float64(r.count)
		}
		rows = append(rows, r)
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].avg > rows[j-1].avg; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}

	fmt.Fprintf(&sb, "\nSeries (%d):\n", len(rows))
	limit := 50
	if len(rows) < limit {
		limit = len(rows)
	}
	for _, r := range rows[:limit] {
		unit := ""
		if r.unit != "" {
			unit = " " + r.unit
		}
		if r.count == 0 {
			fmt.Fprintf(&sb, "• %s — no data\n", r.scope)
			continue
		}
		fmt.Fprintf(&sb, "• %s — avg=%s%s, min=%s%s, max=%s%s, last=%s%s (n=%d)\n",
			r.scope,
			formatNumber(r.avg), unit,
			formatNumber(r.min), unit,
			formatNumber(r.max), unit,
			formatNumber(r.last), unit,
			r.count,
		)
	}
	if len(rows) > limit {
		fmt.Fprintf(&sb, "… and %d more series\n", len(rows)-limit)
	}
	fmt.Fprintf(&sb, "\nExplore: <https://app.%s/metric/explorer|Metrics Explorer>\n", site)
	return sb.String()
}

// seriesUnit extracts a printable unit name from the series' unit array.
func seriesUnit(s MetricSeries) string {
	if len(s.Unit) == 0 {
		return ""
	}
	if u, ok := s.Unit[0].(map[string]any); ok {
		if name, _ := u["short_name"].(string); name != "" {
			return name
		}
		if name, _ := u["name"].(string); name != "" {
			return name
		}
	}
	return ""
}

// formatNumber prints a float with a sensible number of significant digits.
func formatNumber(v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs == 0:
		return "0"
	case abs >= 1000:
		return fmt.Sprintf("%.1f", v)
	case abs >= 10:
		return fmt.Sprintf("%.2f", v)
	case abs >= 1:
		return fmt.Sprintf("%.3f", v)
	default:
		return fmt.Sprintf("%.4g", v)
	}
}
