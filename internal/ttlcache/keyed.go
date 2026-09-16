package ttlcache

import (
	"context"
	"sync"
	"time"
)

// Keyed caches one value per key for a TTL, single-flighting concurrent
// misses on the same key. Failures are not cached: the entry is dropped so a
// transient error does not stick for the whole TTL.
type Keyed[K comparable, V any] struct {
	ttl   time.Duration
	max   int
	fetch func(ctx context.Context, key K) (V, error)

	mu      sync.Mutex
	entries map[K]*keyedEntry[V]
}

type keyedEntry[V any] struct {
	ready     chan struct{}
	val       V
	err       error
	expiresAt time.Time
}

// NewKeyed returns a cache holding at most max entries, each refreshed by
// fetch once it is older than ttl. A max of 0 or less means unbounded.
func NewKeyed[K comparable, V any](ttl time.Duration, max int, fetch func(ctx context.Context, key K) (V, error)) *Keyed[K, V] {
	return &Keyed[K, V]{ttl: ttl, max: max, fetch: fetch, entries: make(map[K]*keyedEntry[V])}
}

// Get returns the cached value for key, fetching it when absent or stale.
// Concurrent callers for the same key share one fetch.
func (c *Keyed[K, V]) Get(ctx context.Context, key K) (V, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && time.Now().Before(e.expiresAt) {
		c.mu.Unlock()
		select {
		case <-e.ready:
			return e.val, e.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
	e := &keyedEntry[V]{ready: make(chan struct{}), expiresAt: time.Now().Add(c.ttl)}
	c.entries[key] = e
	c.evictLocked()
	c.mu.Unlock()

	e.val, e.err = c.fetch(ctx, key)
	close(e.ready)
	if e.err != nil {
		c.mu.Lock()
		// Only drop our own entry: a later miss may already have replaced it.
		if cur, ok := c.entries[key]; ok && cur == e {
			delete(c.entries, key)
		}
		c.mu.Unlock()
	}
	return e.val, e.err
}

// Cached returns the value for key when it is already cached and fresh.
func (c *Keyed[K, V]) Cached(key K) (V, bool) {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok || time.Now().After(e.expiresAt) {
		var zero V
		return zero, false
	}
	select {
	case <-e.ready:
		return e.val, e.err == nil
	default:
		var zero V
		return zero, false
	}
}

// evictLocked keeps the map under max, dropping expired entries first and
// then whichever entry expires soonest.
func (c *Keyed[K, V]) evictLocked() {
	if c.max <= 0 || len(c.entries) <= c.max {
		return
	}
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) > c.max {
		var oldestKey K
		var oldest time.Time
		first := true
		for k, e := range c.entries {
			if first || e.expiresAt.Before(oldest) {
				oldestKey, oldest, first = k, e.expiresAt, false
			}
		}
		delete(c.entries, oldestKey)
	}
}
