// Package grafana is a read-only client for the Grafana HTTP API: dashboards,
// datasources, annotations, saved panel queries and ClickHouse queries.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/text"
	"github.com/justmike1/arbetern/internal/ttlcache"
)

const (
	maxResponseBody = 16 << 20
	httpTimeout     = 90 * time.Second
	userAgent       = "arbetern/grafana-connector"

	maxRetries           = 3
	connectRetryInterval = 30 * time.Second

	dashboardTTL       = 2 * time.Minute
	maxCachedDashboard = 32
	datasourcesTTL     = 5 * time.Minute
)

// Client talks to one Grafana instance. It is safe for concurrent use.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client

	mu       sync.Mutex
	ready    bool
	identity string

	dashboards  *ttlcache.Keyed[string, *dashboardEnvelope]
	datasources *ttlcache.Cache[[]Datasource]
}

// NewClient builds a client; the token is verified in the background and Ready reports success.
func NewClient(baseURL, token string) (*Client, error) {
	base, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("grafana service-account token is empty")
	}
	c := &Client{
		baseURL: base,
		token:   token,
		httpClient: &http.Client{
			Timeout: httpTimeout,
			// A redirect would carry the token to wherever Grafana points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	c.dashboards = ttlcache.NewKeyed(dashboardTTL, maxCachedDashboard, c.fetchDashboard)
	c.datasources = ttlcache.New(datasourcesTTL, c.fetchDatasources)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.verify(ctx); err != nil {
		log.Printf("[grafana] initial token check failed, will retry every %s: %v", connectRetryInterval, ErrorDetail(err))
		safego.Go("grafana: connect retry", c.retryConnect)
	} else {
		log.Printf("[grafana] connected to %s as %s", c.baseURL, c.Identity())
	}
	return c, nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", errors.New("grafana url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("grafana url %q must be an absolute http(s) URL", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("grafana url %q must not carry credentials, a query or a fragment", raw)
	}
	return raw, nil
}

// Ready reports whether the token has been accepted.
func (c *Client) Ready() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// BaseURL returns the instance URL without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// Identity returns the service account's name and login once verified.
func (c *Client) Identity() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identity
}

func (c *Client) retryConnect() {
	ticker := time.NewTicker(connectRetryInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := c.verify(ctx)
		cancel()
		if err != nil {
			log.Printf("[grafana] token check retry failed: %v", ErrorDetail(err))
			continue
		}
		log.Printf("[grafana] connected to %s after retry as %s", c.baseURL, c.Identity())
		return
	}
}

func (c *Client) verify(ctx context.Context) error {
	var u struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := c.get(ctx, "/api/user", nil, &u); err != nil {
		return err
	}
	id := u.Login
	if u.Name != "" && u.Name != u.Login {
		id = fmt.Sprintf("%s (%s)", u.Name, u.Login)
	}
	c.mu.Lock()
	c.ready, c.identity = true, id
	c.mu.Unlock()
	return nil
}

func (c *Client) link(path string) string {
	if path == "" || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return path
	}
	// Grafana's own links already include a sub-path the instance is served under.
	if u.Path != "" && strings.HasPrefix(path, u.Path+"/") {
		return u.Scheme + "://" + u.Host + path
	}
	return c.baseURL + "/" + strings.TrimLeft(path, "/")
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	if c == nil {
		return errors.New("grafana client is not configured")
	}
	fullURL := c.baseURL + path
	if len(q) > 0 {
		fullURL += "?" + q.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encoding grafana request: %w", err)
		}
	}

	var (
		lastErr  error
		nextWait time.Duration
	)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := httpx.SleepCtx(ctx, nextWait); err != nil {
			return err
		}
		nextWait = httpx.Backoff(attempt+1, 0)

		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
		if err != nil {
			return fmt.Errorf("building grafana request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("grafana request failed: %w", err)
			if method != http.MethodGet {
				return lastErr
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("reading grafana response: %w", readErr)
			continue
		}
		if len(raw) > maxResponseBody {
			return fmt.Errorf("the grafana response exceeded %d MB; narrow the request", maxResponseBody>>20)
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("decoding grafana response from %s: %w", path, err)
			}
			return nil
		case attempt < maxRetries && retryable(method, resp.StatusCode):
			lastErr = newAPIError(resp.StatusCode, raw)
			nextWait = httpx.Backoff(attempt+1, httpx.RetryAfter(resp.Header))
			log.Printf("[grafana] retrying in %s (attempt %d/%d) on %s %s — %s",
				nextWait.Round(time.Millisecond), attempt+1, maxRetries, method, path, ErrorDetail(lastErr))
			continue
		default:
			return newAPIError(resp.StatusCode, raw)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("grafana request exhausted %d attempts", maxRetries+1)
	}
	return lastErr
}

// Queries are not repeated on 500 or 504, where a retry would re-run a slow or failing query.
func retryable(method string, status int) bool {
	if method == http.MethodGet {
		return httpx.RetryableStatus(status)
	}
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable
}

// APIError is a non-2xx response; Error is model-facing and Detail is for logs.
type APIError struct {
	Status  int
	Message string
	TraceID string
	body    []byte
}

func newAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status, body: body}
	var p struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		TraceID string `json:"traceID"`
		Results map[string]struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if json.Unmarshal(body, &p) == nil {
		e.TraceID = p.TraceID
		switch {
		case p.Message != "":
			e.Message = p.Message
		case p.Error != "":
			e.Message = p.Error
		}
		for _, r := range p.Results {
			if r.Error != "" {
				e.Message = r.Error
				break
			}
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	e.Message = text.Truncate(e.Message, 600)
	return e
}

// Detail returns the full diagnostic form, for logs.
func (e *APIError) Detail() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "grafana API error: HTTP %d", e.Status)
	if e.Message != "" {
		fmt.Fprintf(&sb, ": %s", e.Message)
	}
	if e.TraceID != "" {
		fmt.Fprintf(&sb, " (trace %s)", e.TraceID)
	}
	return sb.String()
}

// Error returns the sanitized, model-facing form.
func (e *APIError) Error() string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return "Grafana rejected the service-account token (missing, expired or revoked). This is a configuration matter — retrying will not help; report it as a configuration gap."
	case e.Status == http.StatusForbidden:
		return "the Grafana service account is not allowed to do this: its role or the resource's permissions exclude it (some datasources are restricted to admins). Retrying will not help."
	case e.Status == http.StatusNotFound:
		return "Grafana has nothing at that UID or name, or the service account cannot see it. Look it up again with a search or list tool instead of retrying the same value."
	case e.Status == http.StatusTooManyRequests:
		return "Grafana rate-limited the request and it did not succeed after several retries. Wait a minute, then retry with fewer calls."
	case e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity:
		if e.Message != "" {
			return "Grafana rejected the request: " + e.Message
		}
		return "Grafana rejected the request parameters."
	case e.Status >= 500:
		if e.Message != "" {
			return fmt.Sprintf("Grafana returned HTTP %d: %s", e.Status, e.Message)
		}
		return fmt.Sprintf("Grafana returned HTTP %d. Retry shortly if it looks transient.", e.Status)
	default:
		return fmt.Sprintf("Grafana returned HTTP %d. Do not retry blindly; report it if it persists.", e.Status)
	}
}

// ErrorDetail returns the log form of err.
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
