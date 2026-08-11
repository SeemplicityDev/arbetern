// Package clickhouse wraps two independent ClickHouse surfaces used by
// arbetern: the Cloud billing API (the usage-cost report) and a read-only SQL
// query interface against a service's HTTP endpoint. Both authenticate with
// HTTP Basic and are configured separately (see config.Credentials).
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

	"github.com/justmike1/arbetern/internal/safego"
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

	// userAgent identifies arbetern to ClickHouse; it appears as
	// http_user_agent in system.query_log so queries are attributable.
	userAgent = "arbetern/clickhouse-connector"
)

// Client talks to a ClickHouse organization (billing) and/or service (SQL).
// It is safe for concurrent use.
type Client struct {
	baseURL        string
	keyID          string
	keySecret      string
	organizationID string
	httpClient     *http.Client

	// SQL query interface: a service's HTTPS endpoint plus a read-only user.
	queryEndpoint string
	queryUser     string
	queryPassword string

	mu             sync.Mutex
	connected      bool // set once a billing request has authenticated.
	queryConnected bool // set once a SQL query has succeeded.
}

// NewClient builds a ClickHouse client. The key ID/secret/organization ID
// enable the billing API; queryEndpoint/queryUser/queryPassword enable the
// read-only SQL interface. Each configured surface is probed in the background
// (usageCost for billing, SELECT 1 for SQL) and retried every 5s, so its tool
// becomes available once the first call succeeds without blocking startup.
func NewClient(keyID, keySecret, organizationID, queryEndpoint, queryUser, queryPassword string) *Client {
	c := &Client{
		baseURL:        defaultBaseURL,
		keyID:          strings.TrimSpace(keyID),
		keySecret:      strings.TrimSpace(keySecret),
		organizationID: strings.TrimSpace(organizationID),
		queryEndpoint:  strings.TrimRight(strings.TrimSpace(queryEndpoint), "/"),
		queryUser:      strings.TrimSpace(queryUser),
		queryPassword:  queryPassword,
		httpClient:     &http.Client{Timeout: httpTimeout},
	}

	if c.keyID != "" && c.keySecret != "" && c.organizationID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.ping(ctx); err != nil {
			log.Printf("[clickhouse] initial connectivity check failed, will retry every 5s: %v", err)
			safego.Go("clickhouse: connect retry", c.retryConnect)
		} else {
			log.Printf("[clickhouse] connected for organization %s", c.organizationID)
		}
		cancel()
	}

	if c.queryEndpoint != "" && c.queryUser != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.pingQuery(ctx); err != nil {
			log.Printf("[clickhouse] initial SQL query check failed, will retry every 5s: %v", err)
			safego.Go("clickhouse: query connect retry", c.retryQueryConnect)
		} else {
			log.Printf("[clickhouse] SQL query interface connected (%s)", c.queryEndpoint)
		}
		cancel()
	}

	return c
}

// Ready reports whether the billing API has authenticated. Gates the
// usage-cost tool.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// QueryReady reports whether the SQL query interface has succeeded at least
// once. Gates the read-only query tool.
func (c *Client) QueryReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queryConnected
}

// QueryEndpoint returns the configured SQL query endpoint (may be empty).
func (c *Client) QueryEndpoint() string { return c.queryEndpoint }

// OrganizationID returns the configured organization ID.
func (c *Client) OrganizationID() string { return c.organizationID }

func (c *Client) markConnected() {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
}

func (c *Client) markQueryConnected() {
	c.mu.Lock()
	c.queryConnected = true
	c.mu.Unlock()
}

// retryConnect re-probes billing connectivity every 5 seconds until it succeeds.
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

// retryQueryConnect re-probes the SQL query interface every 5 seconds until it
// succeeds.
func (c *Client) retryQueryConnect() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.pingQuery(ctx)
		cancel()
		if err != nil {
			log.Printf("[clickhouse] SQL query retry failed: %v", err)
			continue
		}
		log.Printf("[clickhouse] SQL query interface connected after retry (%s)", c.queryEndpoint)
		return
	}
}

// ping validates the billing credentials with a minimal single-day usageCost
// call — the same endpoint and permission scope the tool uses, so a
// least-privilege billing key is recognised as ready. On success it marks the
// client connected.
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
// decodes the JSON response into out. On success it marks the client connected.
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
	req.Header.Set("User-Agent", userAgent)

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
