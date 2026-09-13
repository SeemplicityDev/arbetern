package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
)

const (
	loadConcurrency   = 8
	maxUpdateAttempts = 4
)

var errInvalid = errors.New("invalid document")

// Documents caches every JSON document under one prefix. Writes go through
// to the bucket first and use the cached ETag so concurrent writers from
// other replicas are detected instead of overwritten; Refresh pulls in what
// other replicas wrote.
type Documents[T any] struct {
	b        *Backend
	prefix   string
	validate func(*T) error

	mu    sync.RWMutex
	items map[string]*entry[T]
}

type entry[T any] struct {
	val  *T
	etag string
}

// Change reports one document that differed from the cache during Refresh.
// Old is nil for a new document, New is nil for a deleted one.
type Change[T any] struct {
	Key string
	Old *T
	New *T
}

// NewDocuments returns an empty cache for the documents under prefix. validate
// may be nil; documents it rejects are skipped with a log line.
func NewDocuments[T any](b *Backend, prefix string, validate func(*T) error) *Documents[T] {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Documents[T]{b: b, prefix: prefix, validate: validate, items: map[string]*entry[T]{}}
}

// Prefix is the object prefix the collection lives under.
func (d *Documents[T]) Prefix() string { return d.prefix }

// Load replaces the cache with everything currently in the bucket.
func (d *Documents[T]) Load(ctx context.Context) error {
	objs, err := d.b.List(ctx, d.prefix)
	if err != nil {
		return err
	}
	items := make(map[string]*entry[T], len(objs))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	sem := make(chan struct{}, loadConcurrency)
	for _, o := range objs {
		key := strings.TrimPrefix(o.Key, d.prefix)
		if !strings.HasSuffix(key, ".json") {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			e, err := d.fetch(ctx, key)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrNotFound):
			case errors.Is(err, errInvalid):
				log.Printf("[store] skipping %s%s: %v", d.prefix, key, err)
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			default:
				items[key] = e
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	d.mu.Lock()
	d.items = items
	d.mu.Unlock()
	return nil
}

func (d *Documents[T]) fetch(ctx context.Context, key string) (*entry[T], error) {
	body, tag, err := d.b.Get(ctx, d.prefix+key)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalid, err)
	}
	if d.validate != nil {
		if err := d.validate(&v); err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalid, err)
		}
	}
	return &entry[T]{val: &v, etag: tag}, nil
}

// Get returns a shallow copy of the document at key.
func (d *Documents[T]) Get(key string) (*T, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	e, ok := d.items[key]
	if !ok {
		return nil, false
	}
	cp := *e.val
	return &cp, true
}

// Range calls fn with a shallow copy of every cached document.
func (d *Documents[T]) Range(fn func(key string, v *T)) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for k, e := range d.items {
		cp := *e.val
		fn(k, &cp)
	}
}

// Len is the number of cached documents.
func (d *Documents[T]) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.items)
}

// Create stores a document that must not exist yet; ErrConflict otherwise.
func (d *Documents[T]) Create(ctx context.Context, key string, v *T) error {
	tag, err := PutJSON(ctx, d.b, d.prefix+key, v, Condition{IfNoneMatch: true})
	if err != nil {
		return err
	}
	d.set(key, &entry[T]{val: v, etag: tag})
	return nil
}

// Update applies fn to a deep copy of the latest document and writes it back,
// re-reading and re-applying when another replica wrote in between. It
// returns the stored document, or ErrNotFound.
func (d *Documents[T]) Update(ctx context.Context, key string, fn func(*T) error) (*T, error) {
	for attempt := 0; ; attempt++ {
		d.mu.RLock()
		e, ok := d.items[key]
		d.mu.RUnlock()
		if !ok {
			fresh, err := d.fetch(ctx, key)
			if err != nil {
				return nil, err
			}
			d.set(key, fresh)
			e = fresh
		}
		v, err := clone(e.val)
		if err != nil {
			return nil, err
		}
		if err := fn(v); err != nil {
			return nil, err
		}
		tag, err := PutJSON(ctx, d.b, d.prefix+key, v, Condition{IfMatch: e.etag})
		switch {
		case errors.Is(err, ErrConflict) && attempt < maxUpdateAttempts:
			fresh, ferr := d.fetch(ctx, key)
			if errors.Is(ferr, ErrNotFound) {
				d.remove(key)
				return nil, ErrNotFound
			}
			if ferr != nil {
				return nil, ferr
			}
			d.set(key, fresh)
			continue
		case errors.Is(err, ErrNotFound):
			d.remove(key)
			return nil, ErrNotFound
		case err != nil:
			return nil, err
		}
		d.set(key, &entry[T]{val: v, etag: tag})
		cp := *v
		return &cp, nil
	}
}

// Delete removes the document; a missing document is not an error.
func (d *Documents[T]) Delete(ctx context.Context, key string) error {
	if err := d.b.Delete(ctx, d.prefix+key, ""); err != nil {
		return err
	}
	d.remove(key)
	return nil
}

// Refresh reconciles the cache with the bucket and reports every document
// that was created, replaced or deleted by someone else.
func (d *Documents[T]) Refresh(ctx context.Context) ([]Change[T], error) {
	objs, err := d.b.List(ctx, d.prefix)
	if err != nil {
		return nil, err
	}
	remote := make(map[string]string, len(objs))
	for _, o := range objs {
		key := strings.TrimPrefix(o.Key, d.prefix)
		if strings.HasSuffix(key, ".json") {
			remote[key] = o.ETag
		}
	}
	var changes []Change[T]
	var stale []string
	d.mu.Lock()
	for key, e := range d.items {
		tag, ok := remote[key]
		if !ok {
			changes = append(changes, Change[T]{Key: key, Old: e.val})
			delete(d.items, key)
			continue
		}
		if tag != e.etag {
			stale = append(stale, key)
		}
	}
	for key := range remote {
		if _, ok := d.items[key]; !ok {
			stale = append(stale, key)
		}
	}
	d.mu.Unlock()

	for _, key := range stale {
		fresh, err := d.fetch(ctx, key)
		switch {
		case errors.Is(err, ErrNotFound):
			if old := d.remove(key); old != nil {
				changes = append(changes, Change[T]{Key: key, Old: old.val})
			}
			continue
		case errors.Is(err, errInvalid):
			log.Printf("[store] skipping %s%s: %v", d.prefix, key, err)
			continue
		case err != nil:
			return changes, err
		}
		old := d.set(key, fresh)
		if old != nil && old.etag == fresh.etag {
			continue
		}
		c := Change[T]{Key: key, New: fresh.val}
		if old != nil {
			c.Old = old.val
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// StartRefresh runs Refresh every interval until ctx ends, handing each
// non-empty change set to onChange.
func (d *Documents[T]) StartRefresh(ctx context.Context, interval time.Duration, onChange func([]Change[T])) {
	name := "store: refresh " + d.prefix
	safego.Go(name, func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				safego.Run(name, func() {
					changes, err := d.Refresh(ctx)
					if err != nil {
						log.Printf("[store] refresh %s: %v", d.prefix, err)
					}
					if len(changes) > 0 && onChange != nil {
						onChange(changes)
					}
				})
			}
		}
	})
}

func (d *Documents[T]) set(key string, e *entry[T]) *entry[T] {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.items[key]
	d.items[key] = e
	return old
}

func (d *Documents[T]) remove(key string) *entry[T] {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.items[key]
	delete(d.items, key)
	return old
}

func clone[T any](v *T) (*T, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
