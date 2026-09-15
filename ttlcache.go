package main

import (
	"context"
	"sync"
	"time"
)

// ttlCacheFailCooldown is how long a failed refresh is remembered. Callers
// queue behind one mutex, so retrying on every request turns one slow upstream
// into a line of full timeouts: the tenth viewer waits for ten of them.
const ttlCacheFailCooldown = 15 * time.Second

// ttlCache serves one fetched value from memory for ttl, single-flights
// concurrent misses, keeps the last good value when a refresh fails, and holds
// off on retrying a failing fetch.
type ttlCache[T any] struct {
	ttl   time.Duration
	fetch func(ctx context.Context) (T, error)

	mu        sync.Mutex
	data      T
	ok        bool
	expiresAt time.Time
	failUntil time.Time
	lastErr   error
}

func newTTLCache[T any](ttl time.Duration, fetch func(ctx context.Context) (T, error)) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, fetch: fetch}
}

func (c *ttlCache[T]) get(ctx context.Context) (T, error) {
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
			c.failUntil, c.lastErr = time.Now().Add(ttlCacheFailCooldown), err
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
