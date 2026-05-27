// Package azure wraps the subset of Azure APIs arbetern needs — currently
// only the Cost Management REST API (cost query + forecast + dimension
// enumeration). Credentials are resolved from Azure service-principal
// environment variables (AZURE_TENANT_ID / AZURE_CLIENT_ID /
// AZURE_CLIENT_SECRET). Cost queries can be scoped one of two ways:
//
//   - Billing-account scope (preferred for Enterprise Agreement tenants):
//     set AZURE_BILLING_ACCOUNT_ID to the EA enrollment number. The SP
//     needs the "Enterprise Reader" billing role on the billing account.
//     This is the canonical EA cost endpoint and rolls up every
//     subscription on the EA, including ones added later.
//   - Management-group scope (default for PAYG / MCA / CSP tenants):
//     the management group ID defaults to the tenant GUID (tenant root
//     MG). The SP needs Cost Management Reader at that MG scope.
//
// When AZURE_BILLING_ACCOUNT_ID is set, it wins — MG configuration is
// ignored. We intentionally avoid pulling in the (heavy) Azure SDK for
// Go: the Cost Management surface is two POSTs and a GET, all against
// management.azure.com, and a thin custom client keeps the dependency
// footprint small. Tokens are cached in-process and refreshed before
// expiry on demand.
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAuthority is the public Azure AD authority. Override with
	// AZURE_AUTHORITY_HOST for sovereign clouds (Azure Government, China).
	DefaultAuthority = "https://login.microsoftonline.com"

	// DefaultManagementHost is the public ARM endpoint. Override with
	// AZURE_MANAGEMENT_HOST for sovereign clouds.
	DefaultManagementHost = "https://management.azure.com"

	// CostMgmtAPIVersion pins the Cost Management API contract we target.
	CostMgmtAPIVersion = "2023-11-01"

	// costCacheTTL is how long an identical Cost Management request's
	// response is reused. The workflow that motivated this client tends
	// to issue the same daily-cost / top-services / MTD queries multiple
	// times within a single run (and across closely-spaced reruns); a few
	// minutes of caching collapses those into a single ARM call without
	// changing the observed answer for human-grade reporting.
	costCacheTTL = 5 * time.Minute

	// DefaultMetric is the cost type used when callers don't specify one.
	// "ActualCost" mirrors the Azure portal's default view. Pass
	// "AmortizedCost" to spread Reservation and Savings Plan up-front
	// charges across their commitment term, which is the right default
	// for accounts that buy reservations.
	DefaultMetric = "ActualCost"

	// MaxDays caps single-call time windows so workflows can't accidentally
	// scan a year of daily granularity in one shot.
	MaxDays = 90
)

// Client wraps the Azure Cost Management REST API. Scope is either an
// EA billing account (preferred when set) or a management group. The
// same client may be shared by many goroutines.
type Client struct {
	httpc             *http.Client
	tenantID          string
	clientID          string
	clientSecret      string
	managementGroupID string
	billingAccountID  string // when non-empty, queries use billing-account scope and managementGroupID is ignored.
	authority         string
	managementHost    string

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time

	// costMu serializes Cost Management requests. ARM enforces an
	// aggressive per-scope rate limit (~5 req / 30s) and parallel callers
	// just fight each other for the same shared quota, so we force a
	// single in-flight cost request at a time. Concurrency wasn't a
	// design goal for these tools — the workflows are sequential reports.
	costMu sync.Mutex

	// costCacheMu guards costCache. Entries store the marshaled response
	// body keyed by method+endpoint+body so identical follow-up requests
	// (same workflow re-asking for "top services last 9 days") skip ARM
	// entirely.
	costCacheMu sync.Mutex
	costCache   map[string]costCacheEntry
}

// costCacheEntry is one cached Cost Management response.
type costCacheEntry struct {
	expires time.Time
	body    []byte
}

// NewClient builds an Azure Cost Management client from the standard
// service-principal environment variables. tenantID, clientID and
// clientSecret are required. Scope is selected as follows:
//
//   - If billingAccountID is non-empty, cost queries hit the billing-
//     account scope. The SP must have the Enterprise Reader billing
//     role on that account. This is the right path for EA tenants.
//   - Otherwise, queries hit the management-group scope. If
//     managementGroupID is empty, it defaults to tenantID (tenant root
//     MG). The SP must have Cost Management Reader at that scope.
//
// The constructor performs a single token-fetch probe so callers
// immediately learn whether the credentials are usable, rather than
// failing on the first tool call several minutes later.
func NewClient(ctx context.Context, tenantID, clientID, clientSecret, managementGroupID, billingAccountID, authorityHost, managementHost string) (*Client, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(clientID) == "" ||
		strings.TrimSpace(clientSecret) == "" {
		return nil, fmt.Errorf("azure: tenantID, clientID and clientSecret are all required")
	}
	if strings.TrimSpace(billingAccountID) == "" && strings.TrimSpace(managementGroupID) == "" {
		// Tenant root management group: its ID is the tenant GUID.
		managementGroupID = tenantID
	}
	if authorityHost == "" {
		authorityHost = DefaultAuthority
	}
	if managementHost == "" {
		managementHost = DefaultManagementHost
	}
	c := &Client{
		httpc:             &http.Client{Timeout: 30 * time.Second},
		tenantID:          tenantID,
		clientID:          clientID,
		clientSecret:      clientSecret,
		managementGroupID: managementGroupID,
		billingAccountID:  billingAccountID,
		authority:         strings.TrimRight(authorityHost, "/"),
		managementHost:    strings.TrimRight(managementHost, "/"),
		costCache:         map[string]costCacheEntry{},
	}
	if _, err := c.token(ctx); err != nil {
		return nil, fmt.Errorf("resolve Azure access token: %w", err)
	}
	return c, nil
}

// ManagementGroupID returns the configured management-group scope (empty
// when running in billing-account mode). Useful for integration-status
// panels.
func (c *Client) ManagementGroupID() string {
	if c.billingAccountID != "" {
		return ""
	}
	return c.managementGroupID
}

// BillingAccountID returns the configured EA billing-account scope, or
// empty if the client is running in management-group mode.
func (c *Client) BillingAccountID() string { return c.billingAccountID }

// TenantID returns the configured AAD tenant. Useful for integration-status panels.
func (c *Client) TenantID() string { return c.tenantID }

// scopePath returns the ARM scope path used as the base for Cost
// Management endpoints. Billing-account scope
// (/providers/Microsoft.Billing/billingAccounts/{id}) is preferred when
// configured — it's the canonical EA endpoint and rolls up every
// subscription on the enrollment. Otherwise management-group scope
// (/providers/Microsoft.Management/managementGroups/{mgId}) is used,
// which rolls up every subscription nested below it.
func (c *Client) scopePath() string {
	if c.billingAccountID != "" {
		return "/providers/Microsoft.Billing/billingAccounts/" + url.PathEscape(c.billingAccountID)
	}
	return "/providers/Microsoft.Management/managementGroups/" + url.PathEscape(c.managementGroupID)
}

// --------------------------------------------------------------------------
// Cost & usage
// --------------------------------------------------------------------------

// CostAndUsageOpts mirrors the AWS package shape so the tool surface stays
// uniform across cloud providers.
type CostAndUsageOpts struct {
	Start         string // YYYY-MM-DD, inclusive. Defaults to 8 days ago.
	End           string // YYYY-MM-DD, exclusive. Defaults to today.
	Granularity   string // Daily (default), Monthly, or None (single bucket).
	Metric        string // ActualCost (default) or AmortizedCost.
	GroupBy       string // "" (no grouping) or any Cost Management dimension (ServiceName, ResourceGroupName, ResourceLocation, MeterCategory, ChargeType, SubscriptionName, ResourceId, ...).
	ServiceFilter string // Exact service name to restrict to (case-sensitive, e.g. "Virtual Machines"). Discover exact strings via GetDimensionValues with dimension=ServiceName.
}

// CostPeriod is one granule (day / month / total) of cost data.
type CostPeriod struct {
	Start  string             `json:"start"`
	End    string             `json:"end"`
	Total  float64            `json:"total"`
	Unit   string             `json:"unit"`
	Groups map[string]float64 `json:"groups,omitempty"`
}

// CostAndUsageResult is the flattened query response.
type CostAndUsageResult struct {
	Granularity string       `json:"granularity"`
	Metric      string       `json:"metric"`
	GroupBy     string       `json:"group_by,omitempty"`
	Start       string       `json:"start"`
	End         string       `json:"end"`
	Periods     []CostPeriod `json:"periods"`
}

// GetCostAndUsage queries the Cost Management API for per-period cost and
// returns a flattened result suitable for LLM consumption.
func (c *Client) GetCostAndUsage(ctx context.Context, opts CostAndUsageOpts) (*CostAndUsageResult, error) {
	start, end, err := resolveDateRange(opts.Start, opts.End, 8)
	if err != nil {
		return nil, err
	}
	gran, err := resolveGranularity(opts.Granularity, true)
	if err != nil {
		return nil, err
	}
	metric, err := resolveMetric(opts.Metric)
	if err != nil {
		return nil, err
	}
	groupBy := strings.TrimSpace(opts.GroupBy)

	// Build the Cost Management query payload. The "Cost" metric name
	// matches the dataset aggregation function for ActualCost reports;
	// for AmortizedCost we still aggregate the "Cost" column but the
	// top-level `type` field flips to "AmortizedCost".
	body := map[string]any{
		"type":      metric,
		"timeframe": "Custom",
		"timePeriod": map[string]string{
			// Cost Management treats both ends as INCLUSIVE on the day,
			// so we translate the AWS-style exclusive `end` (YYYY-MM-DD)
			// into the previous day in UTC for parity.
			"from": start + "T00:00:00Z",
			"to":   inclusiveEnd(end) + "T23:59:59Z",
		},
		"dataset": map[string]any{
			"granularity": gran,
			"aggregation": map[string]any{
				"totalCost": map[string]any{
					"name":     "Cost",
					"function": "Sum",
				},
			},
		},
	}
	dataset := body["dataset"].(map[string]any)
	if groupBy != "" {
		dataset["grouping"] = []map[string]any{{
			"type": "Dimension",
			"name": groupBy,
		}}
	}
	if sf := strings.TrimSpace(opts.ServiceFilter); sf != "" {
		dataset["filter"] = map[string]any{
			"dimensions": map[string]any{
				"name":     "ServiceName",
				"operator": "In",
				"values":   []string{sf},
			},
		}
	}

	endpoint := fmt.Sprintf("%s%s/providers/Microsoft.CostManagement/query?api-version=%s",
		c.managementHost, c.scopePath(), CostMgmtAPIVersion)
	var resp queryResponse
	if err := c.doCostJSON(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		return nil, fmt.Errorf("azure cost management query: %w", err)
	}

	return flattenQuery(&resp, gran, metric, groupBy, start, end)
}

// --------------------------------------------------------------------------
// Cost forecast
// --------------------------------------------------------------------------

// ForecastOpts mirrors AWS ForecastOpts.
type ForecastOpts struct {
	Start       string // YYYY-MM-DD, inclusive. Defaults to today.
	End         string // YYYY-MM-DD, exclusive. Defaults to 30 days from now.
	Granularity string // Daily (default) or Monthly.
	Metric      string // ActualCost (default) or AmortizedCost.
}

// ForecastResult is the flattened forecast response.
type ForecastResult struct {
	Granularity string       `json:"granularity"`
	Metric      string       `json:"metric"`
	Start       string       `json:"start"`
	End         string       `json:"end"`
	Total       float64      `json:"total"`
	Unit        string       `json:"unit"`
	Periods     []CostPeriod `json:"periods"`
}

// GetCostForecast projects spend forward. Cost Management's forecast
// endpoint accepts the same body shape as the query endpoint, with a
// `forecastType` discriminator instead of `type`.
func (c *Client) GetCostForecast(ctx context.Context, opts ForecastOpts) (*ForecastResult, error) {
	today := time.Now().UTC()
	start := opts.Start
	if start == "" {
		start = today.Format("2006-01-02")
	}
	end := opts.End
	if end == "" {
		end = today.AddDate(0, 0, 30).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", start); err != nil {
		return nil, fmt.Errorf("invalid start %q (want YYYY-MM-DD)", start)
	}
	if _, err := time.Parse("2006-01-02", end); err != nil {
		return nil, fmt.Errorf("invalid end %q (want YYYY-MM-DD)", end)
	}
	gran, err := resolveGranularity(opts.Granularity, false)
	if err != nil {
		return nil, err
	}
	metric, err := resolveMetric(opts.Metric)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"type":      metric,
		"timeframe": "Custom",
		"timePeriod": map[string]string{
			"from": start + "T00:00:00Z",
			"to":   inclusiveEnd(end) + "T23:59:59Z",
		},
		"dataset": map[string]any{
			"granularity": gran,
			"aggregation": map[string]any{
				"totalCost": map[string]any{
					"name":     "Cost",
					"function": "Sum",
				},
			},
		},
		"includeActualCost":       false,
		"includeFreshPartialCost": false,
	}

	endpoint := fmt.Sprintf("%s%s/providers/Microsoft.CostManagement/forecast?api-version=%s",
		c.managementHost, c.scopePath(), CostMgmtAPIVersion)
	var resp queryResponse
	if err := c.doCostJSON(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		return nil, fmt.Errorf("azure cost management forecast: %w", err)
	}

	flat, err := flattenQuery(&resp, gran, metric, "", start, end)
	if err != nil {
		return nil, err
	}
	out := &ForecastResult{
		Granularity: flat.Granularity,
		Metric:      flat.Metric,
		Start:       flat.Start,
		End:         flat.End,
		Periods:     flat.Periods,
	}
	for _, p := range out.Periods {
		out.Total += p.Total
		if out.Unit == "" {
			out.Unit = p.Unit
		}
	}
	return out, nil
}

// --------------------------------------------------------------------------
// Dimension values
// --------------------------------------------------------------------------

// DimensionValuesOpts configures GetDimensionValues. Search-time narrowing
// is done server-side via the OData $filter on `properties/data`.
type DimensionValuesOpts struct {
	Dimension string // required: ServiceName, ResourceGroupName, ResourceLocation, MeterCategory, ChargeType, SubscriptionName, ResourceId, ...
	Search    string // Optional substring; matched against returned values client-side.
	Top       int    // Optional cap on values returned (default 100, max 1000).
}

// DimensionValuesResult is the flattened response.
type DimensionValuesResult struct {
	Dimension string   `json:"dimension"`
	Values    []string `json:"values"`
}

// GetDimensionValues enumerates the values seen for a Cost Management
// dimension on the configured management-group scope (every subscription
// nested below it). Useful for discovering the exact service-name strings
// to feed into CostAndUsageOpts.ServiceFilter.
func (c *Client) GetDimensionValues(ctx context.Context, opts DimensionValuesOpts) (*DimensionValuesResult, error) {
	dim := strings.TrimSpace(opts.Dimension)
	if dim == "" {
		return nil, fmt.Errorf("dimension is required (e.g. ServiceName, ResourceGroupName, ResourceLocation)")
	}
	top := opts.Top
	if top <= 0 {
		top = 100
	}
	if top > 1000 {
		top = 1000
	}

	// The dimensions endpoint returns rows under properties.data when
	// $expand=properties/data is set. We filter to the specific dimension
	// by name via the OData $filter.
	q := url.Values{}
	q.Set("api-version", CostMgmtAPIVersion)
	q.Set("$expand", "properties/data")
	q.Set("$top", strconv.Itoa(top))
	q.Set("$filter", "name eq '"+dim+"'")
	endpoint := fmt.Sprintf("%s%s/providers/Microsoft.CostManagement/dimensions?%s",
		c.managementHost, c.scopePath(), q.Encode())

	var resp dimensionsResponse
	if err := c.doCostJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("azure cost management dimensions: %w", err)
	}

	values := []string{}
	for _, v := range resp.Value {
		for _, d := range v.Properties.Data {
			if s := strings.TrimSpace(d.Name); s != "" {
				if opts.Search != "" && !strings.Contains(strings.ToLower(s), strings.ToLower(opts.Search)) {
					continue
				}
				values = append(values, s)
			}
		}
	}
	sort.Strings(values)
	return &DimensionValuesResult{Dimension: dim, Values: values}, nil
}

// --------------------------------------------------------------------------
// HTTP plumbing
// --------------------------------------------------------------------------

type queryResponse struct {
	Properties struct {
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	} `json:"properties"`
}

type dimensionsResponse struct {
	Value []struct {
		Properties struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
		} `json:"properties"`
	} `json:"value"`
}

// token returns a cached bearer token, refreshing it via the client-credentials
// grant when the cached one is missing or within 60 seconds of expiry.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.tokenExpiry) > 60*time.Second {
		return c.accessToken, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("scope", c.managementHost+"/.default")
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", c.authority, url.PathEscape(c.tenantID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, truncate(string(body), 300))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access_token")
	}
	c.accessToken = tr.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// doCostJSON wraps doJSON with two extra layers that exist specifically
// for the Cost Management endpoints (query / forecast / dimensions):
//
//  1. A small in-process TTL cache keyed by method+endpoint+request body.
//     The daily-cost workflow re-issues the same "top services last 9
//     days" / "MTD spend" queries from multiple reasoning steps; without
//     this they each round-trip to ARM and burn quota. Cached hits skip
//     the network entirely and never count against the rate limit.
//  2. A process-wide mutex that serializes Cost Management requests.
//     ARM enforces a per-scope rate budget (~5 req / 30s) that is shared
//     across concurrent callers; running them in parallel just produces
//     more 429s, not faster throughput.
func (c *Client) doCostJSON(ctx context.Context, method, endpoint string, body any, out any) error {
	key, err := costCacheKey(method, endpoint, body)
	if err == nil {
		if raw, ok := c.cacheGet(key); ok {
			if out == nil || len(raw) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("parse cached response: %w", err)
			}
			return nil
		}
	}

	c.costMu.Lock()
	defer c.costMu.Unlock()

	// Re-check the cache after acquiring the mutex — a peer goroutine
	// that was already in flight may have just populated it.
	if key != "" {
		if raw, ok := c.cacheGet(key); ok {
			if out == nil || len(raw) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("parse cached response: %w", err)
			}
			return nil
		}
	}

	raw, err := c.doJSONRaw(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if key != "" {
		c.cachePut(key, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	return nil
}

// costCacheKey produces a stable cache key from the request shape. JSON
// marshaling of the body is sufficient because the cost-query payloads
// are built from map[string]any whose key order Go's encoder normalizes.
func costCacheKey(method, endpoint string, body any) (string, error) {
	var bodyJSON []byte
	if body != nil {
		var err error
		bodyJSON, err = json.Marshal(body)
		if err != nil {
			return "", err
		}
	}
	return method + " " + endpoint + "\n" + string(bodyJSON), nil
}

func (c *Client) cacheGet(key string) ([]byte, bool) {
	c.costCacheMu.Lock()
	defer c.costCacheMu.Unlock()
	e, ok := c.costCache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(c.costCache, key)
		return nil, false
	}
	return e.body, true
}

func (c *Client) cachePut(key string, body []byte) {
	c.costCacheMu.Lock()
	defer c.costCacheMu.Unlock()
	// Cheap opportunistic eviction: when the map grows past a soft cap,
	// drop expired entries before inserting. We don't expect more than a
	// handful of distinct cost queries per workflow run, so this stays
	// O(small) in practice.
	if len(c.costCache) > 64 {
		now := time.Now()
		for k, e := range c.costCache {
			if now.After(e.expires) {
				delete(c.costCache, k)
			}
		}
	}
	c.costCache[key] = costCacheEntry{
		expires: time.Now().Add(costCacheTTL),
		body:    body,
	}
}

// doJSONRaw is doJSON minus the unmarshal step; it returns the raw
// response body so callers (specifically doCostJSON) can cache it.
func (c *Client) doJSONRaw(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	var buf []byte
	if body != nil {
		buf, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
	}

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var reader io.Reader
		if buf != nil {
			reader = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if buf != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 300 {
			return respBody, nil
		}
		lastErr = fmt.Errorf("ARM returned %s: %s", resp.Status, truncate(string(respBody), 500))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
		if attempt == maxAttempts-1 {
			return nil, lastErr
		}
		wait := retryAfter(resp.Header, attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// retryAfter computes the sleep duration before the next retry attempt.
// Azure exposes the remaining quota window in Retry-After (seconds or
// HTTP-date) plus x-ms-ratelimit-microsoft.*-retry-after; we trust the
// largest hint we can find and fall back to capped exponential backoff
// (2s, 4s, 8s, 16s, 32s) when no header is present.
func retryAfter(h http.Header, attempt int) time.Duration {
	candidates := []string{
		h.Get("Retry-After"),
		h.Get("x-ms-ratelimit-microsoft.costmanagement-entity-retry-after"),
		h.Get("x-ms-ratelimit-microsoft.consumption-entity-retry-after"),
	}
	var best time.Duration
	for _, v := range candidates {
		if v == "" {
			continue
		}
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
			if d := time.Duration(secs) * time.Second; d > best {
				best = d
			}
			continue
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > best {
				best = d
			}
		}
	}
	if best <= 0 {
		best = time.Duration(1<<(attempt+1)) * time.Second
	}
	if best > 60*time.Second {
		best = 60 * time.Second
	}
	return best
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func resolveDateRange(start, end string, defaultDays int) (string, string, error) {
	today := time.Now().UTC()
	if end == "" {
		end = today.Format("2006-01-02")
	}
	if start == "" {
		start = today.AddDate(0, 0, -defaultDays).Format("2006-01-02")
	}
	startT, err := time.Parse("2006-01-02", start)
	if err != nil {
		return "", "", fmt.Errorf("invalid start %q (want YYYY-MM-DD)", start)
	}
	endT, err := time.Parse("2006-01-02", end)
	if err != nil {
		return "", "", fmt.Errorf("invalid end %q (want YYYY-MM-DD)", end)
	}
	if !startT.Before(endT) {
		return "", "", fmt.Errorf("start (%s) must be before end (%s); end is exclusive (matches Cost Explorer convention)", start, end)
	}
	if endT.Sub(startT).Hours()/24 > float64(MaxDays) {
		return "", "", fmt.Errorf("time range exceeds %d days — split into smaller chunks", MaxDays)
	}
	return start, end, nil
}

func resolveGranularity(g string, allowNone bool) (string, error) {
	switch strings.ToLower(strings.TrimSpace(g)) {
	case "", "daily":
		return "Daily", nil
	case "monthly":
		return "Monthly", nil
	case "none":
		if !allowNone {
			return "", fmt.Errorf("granularity 'None' is only valid for cost queries (not forecast)")
		}
		return "None", nil
	default:
		return "", fmt.Errorf("invalid granularity %q (want Daily, Monthly, or None)", g)
	}
}

func resolveMetric(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", "actualcost", "actual":
		return "ActualCost", nil
	case "amortizedcost", "amortized":
		return "AmortizedCost", nil
	default:
		return "", fmt.Errorf("invalid metric %q (want ActualCost or AmortizedCost)", m)
	}
}

// inclusiveEnd converts an AWS-style exclusive end (the day NOT included)
// into the previous day, since Azure Cost Management treats `to` as
// inclusive of the day. e.g. end="2026-04-22" → "2026-04-21".
func inclusiveEnd(end string) string {
	t, err := time.Parse("2006-01-02", end)
	if err != nil {
		return end
	}
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}

// flattenQuery transforms a Cost Management columns/rows result into the
// uniform CostPeriod shape the LLM-facing tools consume.
func flattenQuery(resp *queryResponse, gran, metric, groupBy, start, end string) (*CostAndUsageResult, error) {
	// Locate the relevant column indexes by name. Cost Management always
	// returns "Cost" (or "PreTaxCost" on some scopes) plus a "Currency"
	// column; "UsageDate"/"BillingMonth" only appear when granularity != None.
	idxCost := -1
	idxDate := -1
	idxCurrency := -1
	idxGroup := -1
	for i, col := range resp.Properties.Columns {
		switch col.Name {
		case "Cost", "CostUSD", "PreTaxCost":
			if idxCost < 0 {
				idxCost = i
			}
		case "UsageDate", "BillingMonth":
			idxDate = i
		case "Currency", "CurrencyCode":
			idxCurrency = i
		default:
			if groupBy != "" && strings.EqualFold(col.Name, groupBy) {
				idxGroup = i
			}
		}
	}
	if idxCost < 0 {
		return nil, fmt.Errorf("azure cost management response had no Cost column (columns=%v)", columnNames(resp))
	}

	// Aggregate rows into per-period buckets keyed by date.
	type bucket struct {
		total  float64
		unit   string
		groups map[string]float64
	}
	buckets := map[string]*bucket{}
	order := []string{}

	for _, row := range resp.Properties.Rows {
		cost := toFloat(row[idxCost])
		dateKey := ""
		if idxDate >= 0 {
			dateKey = formatDateCell(row[idxDate])
		}
		b, ok := buckets[dateKey]
		if !ok {
			b = &bucket{groups: map[string]float64{}}
			buckets[dateKey] = b
			order = append(order, dateKey)
		}
		b.total += cost
		if idxCurrency >= 0 && b.unit == "" {
			if s, ok := row[idxCurrency].(string); ok {
				b.unit = s
			}
		}
		if idxGroup >= 0 {
			if s, ok := row[idxGroup].(string); ok && s != "" {
				b.groups[s] += cost
			}
		}
	}

	sort.Strings(order)
	out := &CostAndUsageResult{
		Granularity: gran,
		Metric:      metric,
		GroupBy:     groupBy,
		Start:       start,
		End:         end,
		Periods:     make([]CostPeriod, 0, len(order)),
	}
	for _, k := range order {
		b := buckets[k]
		p := CostPeriod{
			Start: k,
			End:   k,
			Total: b.total,
			Unit:  b.unit,
		}
		if len(b.groups) > 0 {
			p.Groups = b.groups
		}
		out.Periods = append(out.Periods, p)
	}
	return out, nil
}

func columnNames(resp *queryResponse) []string {
	names := make([]string, 0, len(resp.Properties.Columns))
	for _, c := range resp.Properties.Columns {
		names = append(names, c.Name)
	}
	return names
}

// toFloat coerces the JSON-decoded cost cell (json.Number-ish) into a float.
func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return f
		}
	}
	return 0
}

// formatDateCell turns the date column into a YYYY-MM-DD string. Cost
// Management returns dates as integers like 20250115 (UsageDate) for daily
// granularity and 202501 for monthly billing months. None granularity has
// no date column at all (handled by the caller).
func formatDateCell(v any) string {
	s := ""
	switch x := v.(type) {
	case float64:
		s = strconv.FormatInt(int64(x), 10)
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case string:
		s = x
	}
	switch len(s) {
	case 8: // YYYYMMDD
		return s[:4] + "-" + s[4:6] + "-" + s[6:]
	case 6: // YYYYMM (billing month)
		return s[:4] + "-" + s[4:6]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}
