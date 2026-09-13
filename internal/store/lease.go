package store

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"sync"
	"time"
)

// Lease is a time-bounded exclusive claim on one object, taken and renewed
// with conditional writes so at most one replica holds it at a time.
type Lease struct {
	b      *Backend
	key    string
	holder string
	ttl    time.Duration
}

type leaseRecord struct {
	Holder  string    `json:"holder"`
	Expires time.Time `json:"expires"`
}

// NewLease describes the lease at key for holder. ttl bounds how long a
// crashed holder blocks others.
func NewLease(b *Backend, key, holder string, ttl time.Duration) *Lease {
	return &Lease{b: b, key: key, holder: holder, ttl: ttl}
}

// Key is the lease object key.
func (l *Lease) Key() string { return l.key }

// Acquire takes the lease when it is free or expired. On success the returned
// context stays live while the lease is held and renewed in the background;
// release gives the lease up. ok is false when someone else holds it.
func (l *Lease) Acquire(ctx context.Context) (held context.Context, release func(), ok bool, err error) {
	tag, ok, err := l.claim(ctx)
	if err != nil || !ok {
		return nil, nil, false, err
	}
	held, cancel := context.WithCancel(ctx)
	h := &holding{lease: l, tag: tag, expires: time.Now().Add(l.ttl), cancel: cancel, stop: make(chan struct{})}
	go h.renew(held)
	return held, h.release, true, nil
}

func (l *Lease) claim(ctx context.Context) (string, bool, error) {
	now := time.Now().UTC()
	rec := leaseRecord{Holder: l.holder, Expires: now.Add(l.ttl)}
	cur, tag, err := GetJSON[leaseRecord](ctx, l.b, l.key)
	switch {
	case errors.Is(err, ErrNotFound):
		tag, err = PutJSON(ctx, l.b, l.key, rec, Condition{IfNoneMatch: true})
	case err != nil:
		return "", false, err
	case cur.Holder != l.holder && cur.Expires.After(now):
		return "", false, nil
	default:
		tag, err = PutJSON(ctx, l.b, l.key, rec, Condition{IfMatch: tag})
	}
	if errors.Is(err, ErrConflict) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return tag, true, nil
}

type holding struct {
	lease  *Lease
	cancel context.CancelFunc
	stop   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	tag     string
	expires time.Time
}

func (h *holding) renew(ctx context.Context) {
	t := time.NewTicker(h.lease.ttl / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		tag := h.tag
		h.mu.Unlock()
		rec := leaseRecord{Holder: h.lease.holder, Expires: time.Now().UTC().Add(h.lease.ttl)}
		newTag, err := PutJSON(ctx, h.lease.b, h.lease.key, rec, Condition{IfMatch: tag})
		if err == nil {
			h.mu.Lock()
			h.tag, h.expires = newTag, rec.Expires
			h.mu.Unlock()
			continue
		}
		h.mu.Lock()
		expired := time.Now().After(h.expires)
		h.mu.Unlock()
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) || expired {
			log.Printf("[lease] %s lost by %s: %v", h.lease.key, h.lease.holder, err)
			h.cancel()
			return
		}
		log.Printf("[lease] %s renew failed (retrying): %v", h.lease.key, err)
	}
}

func (h *holding) release() {
	h.once.Do(func() {
		close(h.stop)
		h.cancel()
		h.mu.Lock()
		tag := h.tag
		h.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := h.lease.b.Delete(ctx, h.lease.key, tag); err != nil && !errors.Is(err, ErrConflict) {
			log.Printf("[lease] release %s: %v", h.lease.key, err)
		}
	})
}

// RunElection keeps competing for the lease until ctx ends and runs lead for
// every term it wins. lead must return once its context is cancelled.
func RunElection(ctx context.Context, l *Lease, lead func(held context.Context)) {
	for {
		held, release, ok, err := l.Acquire(ctx)
		if err != nil {
			log.Printf("[lease] acquire %s: %v", l.key, err)
		}
		if ok {
			lead(held)
			release()
		}
		wait := l.ttl/2 + time.Duration(rand.Int64N(int64(l.ttl/4)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
