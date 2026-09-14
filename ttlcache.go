package main

import (
	"context"
	"sync"
	"time"
)

// ttlCache serves one fetched value from memory for ttl, single-flights
// concurrent misses, and keeps the last good value when a refresh fails.
type ttlCache[T any] struct {
	ttl   time.Duration
	fetch func(ctx context.Context) (T, error)

	mu        sync.Mutex
	data      T
	ok        bool
	expiresAt time.Time
}

func newTTLCache[T any](ttl time.Duration, fetch func(ctx context.Context) (T, error)) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, fetch: fetch}
}

func (c *ttlCache[T]) get(ctx context.Context) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && time.Now().Before(c.expiresAt) {
		return c.data, nil
	}
	v, err := c.fetch(ctx)
	if err != nil {
		if c.ok {
			return c.data, nil
		}
		var zero T
		return zero, err
	}
	c.data, c.ok, c.expiresAt = v, true, time.Now().Add(c.ttl)
	return v, nil
}
