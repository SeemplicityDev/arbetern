package store

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// DefaultMergeAttempts is how many times Merge re-reads and re-applies before
// giving up to a competing writer.
const DefaultMergeAttempts = 4

// Merge folds a change into the object at key with a conditional write, and is
// the read-modify-write counterpart to Documents.Update for objects that are
// not part of a cached collection — rolling aggregates and append-only feeds,
// where several replicas write the same object concurrently.
//
// fn receives the stored document, or a zero value when the object does not
// exist yet, and mutates it in place. A write that loses the race against
// another replica is retried against the document that replica left behind, so
// the change is folded into their result rather than overwriting it. The stored
// document is returned, which is the caller's merged view including whatever
// arrived from elsewhere.
//
// fn must be idempotent in the sense that it may run several times on different
// starting documents; it must apply the same change each time, not accumulate.
func Merge[T any](ctx context.Context, b *Backend, key string, attempts int, fn func(*T)) (*T, error) {
	if attempts <= 0 {
		attempts = DefaultMergeAttempts
	}
	for attempt := 0; attempt < attempts; attempt++ {
		doc, tag, err := GetJSON[T](ctx, b, key)
		cond := Condition{IfMatch: tag}
		switch {
		case errors.Is(err, ErrNotFound):
			doc = new(T)
			cond = Condition{IfNoneMatch: true}
		case err != nil:
			return nil, err
		}
		fn(doc)
		if _, err := PutJSON(ctx, b, key, doc, cond); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return nil, err
		}
		return doc, nil
	}
	return nil, ErrConflict
}

// LoadMatching decodes every JSON object under prefix whose base name starts
// with namePrefix and hands it to fn. It is the boot-time read for rolling
// aggregates kept as one object per period.
//
// An object that disappears between the listing and the read is skipped: at any
// moment another replica may be pruning. Anything else fails the load, because
// an aggregate that cannot be read at boot is a signal worth surfacing rather
// than silently starting from empty.
func LoadMatching[T any](ctx context.Context, b *Backend, prefix, namePrefix string, fn func(key string, v *T)) error {
	objs, err := b.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, o := range objs {
		name := path.Base(o.Key)
		if !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		v, _, err := GetJSON[T](ctx, b, o.Key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return fmt.Errorf("%s: %w", o.Key, err)
		}
		fn(o.Key, v)
	}
	return nil
}
