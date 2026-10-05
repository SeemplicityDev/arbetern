// Package clickhouse wraps a read-only SQL query interface against a
// ClickHouse service's HTTP endpoint, authenticated with HTTP Basic (see
// config.Credentials).
package clickhouse

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
)

const (
	// maxResponseBody caps the response body read via io.LimitReader.
	maxResponseBody = 64 << 20 // 64 MiB.

	// httpTimeout bounds a single HTTP round-trip.
	httpTimeout = 60 * time.Second

	// userAgent identifies arbetern to ClickHouse; it appears as
	// http_user_agent in system.query_log so queries are attributable.
	userAgent = "arbetern/clickhouse-connector"
)

// Client talks to a ClickHouse service over its SQL query interface.
// It is safe for concurrent use.
type Client struct {
	httpClient *http.Client

	// SQL query interface: a service's HTTP(S) endpoint plus a read-only user.
	queryEndpoint string
	queryUser     string
	queryPassword string

	mu             sync.Mutex
	queryConnected bool // set once a SQL query has succeeded.
}

// NewClient builds a ClickHouse client for the read-only SQL interface. The
// endpoint is probed in the background (SELECT 1) and retried every 5s, so the
// query tool becomes available once the first call succeeds without blocking
// startup.
func NewClient(queryEndpoint, queryUser, queryPassword string) *Client {
	c := &Client{
		queryEndpoint: strings.TrimRight(strings.TrimSpace(queryEndpoint), "/"),
		queryUser:     strings.TrimSpace(queryUser),
		queryPassword: queryPassword,
		httpClient:    &http.Client{Timeout: httpTimeout},
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

// QueryReady reports whether the SQL query interface has succeeded at least
// once. Gates the read-only query tool.
func (c *Client) QueryReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queryConnected
}

// QueryEndpoint returns the configured SQL query endpoint (may be empty).
func (c *Client) QueryEndpoint() string { return c.queryEndpoint }

func (c *Client) markQueryConnected() {
	c.mu.Lock()
	c.queryConnected = true
	c.mu.Unlock()
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
