// Package ttlcache serves one fetched value from memory for a TTL, single-
// flighting concurrent misses and keeping the last good value across a failed
// refresh.
package ttlcache

import (
	"context"
	"sync"
	"time"
)

// failCooldown is how long a failed refresh is remembered. Callers queue
// behind one mutex, so retrying on every request turns one slow upstream into
// a line of full timeouts: the tenth viewer waits for ten of them.
const failCooldown = 15 * time.Second

// Cache holds a single value refreshed by fetch once it is older than ttl.
type Cache[T any] struct {
	ttl   time.Duration
	fetch func(ctx context.Context) (T, error)

	mu        sync.Mutex
	data      T
	ok        bool
	expiresAt time.Time
	failUntil time.Time
	lastErr   error
}

// New returns a cache that refreshes via fetch once the value is older than ttl.
func New[T any](ttl time.Duration, fetch func(ctx context.Context) (T, error)) *Cache[T] {
	return &Cache[T]{ttl: ttl, fetch: fetch}
}

// Get returns the cached value, refreshing it when stale. A failed refresh
// returns the previous value when there is one.
func (c *Cache[T]) Get(ctx context.Context) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.ok && now.Before(c.expiresAt) {
		return c.data, nil
	}
	if now.Before(c.failUntil) {
		if c.ok {
			return c.data, nil
		}
		var zero T
		return zero, c.lastErr
	}
	v, err := c.fetch(ctx)
	if err != nil {
		// A caller that walked away says nothing about the upstream.
		if ctx.Err() == nil {
			c.failUntil, c.lastErr = time.Now().Add(failCooldown), err
		}
		if c.ok {
			return c.data, nil
		}
		var zero T
		return zero, err
	}
	c.data, c.ok, c.expiresAt = v, true, time.Now().Add(c.ttl)
	c.failUntil, c.lastErr = time.Time{}, nil
	return v, nil
}

// Invalidate drops the cached value so the next Get refetches.
func (c *Cache[T]) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	c.data, c.ok, c.expiresAt = zero, false, time.Time{}
	c.failUntil, c.lastErr = time.Time{}, nil
}
