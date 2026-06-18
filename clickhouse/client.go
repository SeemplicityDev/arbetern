// Package clickhouse wraps the subset of the ClickHouse Cloud API that
// arbetern needs — currently the billing "usage cost" report
// (GET /v1/organizations/{organizationId}/usageCost). It lets an agent pull a
// per-day, per-entity breakdown of an organization's spend in ClickHouse
// Credits (CHC) over a date range.
//
// Authentication is HTTP Basic: a key ID (username) and key secret (password)
// generated in the ClickHouse Cloud console. The organization ID selects which
// organization the report covers. All three are supplied via configuration
// (see config.Credentials).
package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// defaultBaseURL is the ClickHouse Cloud API root. Resource paths such as
	// /v1/organizations/{orgID}/usageCost are appended to it.
	defaultBaseURL = "https://api.clickhouse.cloud"

	// maxResponseBody caps the response body read via io.LimitReader. A wide
	// usage-cost window returns one record per entity per day, so allow room.
	maxResponseBody = 64 << 20 // 64 MiB.

	// httpTimeout bounds a single HTTP round-trip.
	httpTimeout = 60 * time.Second

	// maxWindowDays is the widest span the usageCost endpoint accepts: to_date
	// may be at most 30 days after from_date (an inclusive 31-day window).
	maxWindowDays = 30

	// dateLayout is the ISO-8601 date format the endpoint expects (UTC).
	dateLayout = "2006-01-02"
)

// Client talks to the ClickHouse Cloud API for one organization. It is safe
// for concurrent use.
type Client struct {
	baseURL        string
	keyID          string
	keySecret      string
	organizationID string
	httpClient     *http.Client

	mu        sync.Mutex
	connected bool // true once at least one request has authenticated successfully.
}

// NewClient builds a ClickHouse Cloud client from HTTP Basic credentials (key
// ID + key secret) and the target organization ID. It probes connectivity in
// the background — a minimal single-day usage-cost call, the same endpoint the
// tool uses — and retries every 5 seconds until it succeeds, so the service
// never blocks on startup and the usage-cost tool becomes available once the
// first call authenticates.
func NewClient(keyID, keySecret, organizationID string) *Client {
	c := &Client{
		baseURL:        defaultBaseURL,
		keyID:          strings.TrimSpace(keyID),
		keySecret:      strings.TrimSpace(keySecret),
		organizationID: strings.TrimSpace(organizationID),
		httpClient:     &http.Client{Timeout: httpTimeout},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ping(ctx); err != nil {
		log.Printf("[clickhouse] initial connectivity check failed, will retry every 5s: %v", err)
		go c.retryConnect()
	} else {
		log.Printf("[clickhouse] connected for organization %s", c.organizationID)
	}
	return c
}

// Ready reports whether at least one API call has authenticated successfully.
// Mirrors the OAuthClient interface the command layer uses to gate tools.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// OrganizationID returns the configured organization ID. Useful for
// integration-status panels.
func (c *Client) OrganizationID() string { return c.organizationID }

func (c *Client) markConnected() {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
}

// retryConnect re-probes connectivity every 5 seconds until it succeeds.
func (c *Client) retryConnect() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.ping(ctx)
		cancel()
		if err != nil {
			log.Printf("[clickhouse] connectivity retry failed: %v", err)
			continue
		}
		log.Printf("[clickhouse] connected after retry for organization %s", c.organizationID)
		return
	}
}

// ping performs a lightweight authenticated request against the usage-cost
// endpoint — the SAME endpoint (and permission scope) the tool itself uses —
// for a minimal single-day window, to validate the credentials and
// organization ID. Probing the real endpoint rather than a broader
// "organization details" call ensures a least-privilege, billing-scoped API
// key is recognised as ready (an org-details probe can be forbidden for such a
// key even when usageCost succeeds). On success it marks the client connected.
func (c *Client) ping(ctx context.Context) error {
	if c.keyID == "" || c.keySecret == "" || c.organizationID == "" {
		return fmt.Errorf("missing key ID, key secret or organization ID")
	}
	today := time.Now().UTC().Format(dateLayout)
	q := url.Values{}
	q.Set("from_date", today)
	q.Set("to_date", today)
	path := "/v1/organizations/" + url.PathEscape(c.organizationID) + "/usageCost"
	var discard usageCostResponse
	return c.get(ctx, path, q, &discard)
}

// GetUsageCost returns the organization's usage-cost report between fromDate
// and toDate (inclusive), both ISO-8601 dates (YYYY-MM-DD, evaluated in UTC).
// The API caps the window at 31 days: toDate may be at most 30 days after
// fromDate. filters optionally narrows the report by resource tag, e.g.
// "tag:Environment=Production"; pass nil for none.
func (c *Client) GetUsageCost(ctx context.Context, fromDate, toDate string, filters []string) (*UsageCostReport, error) {
	if c.organizationID == "" {
		return nil, fmt.Errorf("no organization configured (set clickhouse-organization-id)")
	}
	from, to, err := validateWindow(fromDate, toDate)
	if err != nil {
		return nil, err
	}

	cleanFilters := make([]string, 0, len(filters))
	for _, f := range filters {
		if f = strings.TrimSpace(f); f != "" {
			cleanFilters = append(cleanFilters, f)
		}
	}

	q := url.Values{}
	q.Set("from_date", from)
	q.Set("to_date", to)
	for _, f := range cleanFilters {
		q.Add("filter", f)
	}

	path := "/v1/organizations/" + url.PathEscape(c.organizationID) + "/usageCost"
	var resp usageCostResponse
	if err := c.get(ctx, path, q, &resp); err != nil {
		return nil, err
	}

	return &UsageCostReport{
		OrganizationID: c.organizationID,
		FromDate:       from,
		ToDate:         to,
		Filters:        cleanFilters,
		GrandTotalCHC:  resp.Result.GrandTotalCHC,
		Records:        resp.Result.Costs,
	}, nil
}

// validateWindow checks the from/to dates parse as ISO-8601, are ordered, and
// span at most maxWindowDays (the API rejects wider windows). It returns the
// normalized dates.
func validateWindow(fromDate, toDate string) (string, string, error) {
	fromDate = strings.TrimSpace(fromDate)
	toDate = strings.TrimSpace(toDate)
	if fromDate == "" || toDate == "" {
		return "", "", fmt.Errorf("from_date and to_date are required (YYYY-MM-DD)")
	}
	from, err := time.Parse(dateLayout, fromDate)
	if err != nil {
		return "", "", fmt.Errorf("invalid from_date %q: expected YYYY-MM-DD", fromDate)
	}
	to, err := time.Parse(dateLayout, toDate)
	if err != nil {
		return "", "", fmt.Errorf("invalid to_date %q: expected YYYY-MM-DD", toDate)
	}
	if to.Before(from) {
		return "", "", fmt.Errorf("to_date %s is before from_date %s", toDate, fromDate)
	}
	if to.Sub(from) > maxWindowDays*24*time.Hour {
		return "", "", fmt.Errorf("date window too wide: to_date may be at most %d days after from_date (max %d-day period)", maxWindowDays, maxWindowDays+1)
	}
	return from.Format(dateLayout), to.Format(dateLayout), nil
}

// get performs an authenticated GET against the ClickHouse Cloud API and
// decodes the JSON response into out. query may be nil. It maps non-2xx
// responses (including the API's {status,error,requestId} envelope) to a
// descriptive error and, on success, marks the client connected.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth(c.keyID, c.keySecret)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, body)
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	c.markConnected()
	return nil
}

// apiError converts a non-2xx response into an error, preferring the API's
// structured {status,error,requestId} envelope when present.
func apiError(status int, body []byte) error {
	var e errorResponse
	if err := json.Unmarshal(body, &e); err == nil && strings.TrimSpace(e.Error) != "" {
		return fmt.Errorf("clickhouse API error (HTTP %d): %s", status, e.Error)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		return fmt.Errorf("clickhouse API error: HTTP %d", status)
	}
	return fmt.Errorf("clickhouse API error (HTTP %d): %s", status, msg)
}
