// Package catalog indexes workflow and dashboard descriptors in the vector
// index so agents and the console can find them by meaning.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/vectors"
	"github.com/justmike1/arbetern/workflows"
)

// ErrDisabled is returned by Search while no vector index is attached.
var ErrDisabled = errors.New("catalog search is not configured")

const (
	manifestKey = "catalog/manifest.json"
	keyPrefix   = "registry/"
	maxText     = 3000
	maxDistance = 0.75
	// MaxResults caps one search.
	MaxResults = 10
)

// Kinds of catalogued items.
const (
	KindWorkflow  = "workflow"
	KindDashboard = "dashboard"
)

// Index keeps the vector index in step with the registries and answers
// searches. Sync runs on the scheduling replica; Search on any.
type Index struct {
	b  *store.Backend
	wf *workflows.Registry
	db *dashboards.Registry
	ix atomic.Pointer[vectors.Index]
}

// Hit is one search result.
type Hit struct {
	Kind        string  `json:"kind"`
	Agent       string  `json:"agent"`
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ShortName   string  `json:"short_name,omitempty"`
	Description string  `json:"description,omitempty"`
	Distance    float32 `json:"distance"`
}

type manifest struct {
	Items map[string]string `json:"items"`
}

type item struct {
	key  string
	text string
	meta map[string]any
}

// New returns a catalog over the registries. Call SetIndex to enable it.
func New(b *store.Backend, wf *workflows.Registry, db *dashboards.Registry) *Index {
	return &Index{b: b, wf: wf, db: db}
}

// SetIndex attaches the vector index; nil disables search.
func (c *Index) SetIndex(ix *vectors.Index) {
	if c != nil {
		c.ix.Store(ix)
	}
}

// Enabled reports whether searches can be answered.
func (c *Index) Enabled() bool { return c != nil && c.ix.Load() != nil }

func itemKey(kind, agent, id string) string { return keyPrefix + kind + "/" + agent + "/" + id }

func clip(s string) string {
	if len(s) > maxText {
		return s[:maxText]
	}
	return s
}

func (c *Index) desired() map[string]item {
	out := map[string]item{}
	for _, w := range c.wf.List("") {
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n%s\n%s\n", w.Name, w.ShortName, w.Description)
		fmt.Fprintf(&b, "pattern %s, schedule %s, trigger %s %s\n", w.Pattern(), w.Cron, w.Trigger.Type, w.Trigger.Ref)
		if len(w.Tasks) > 0 {
			for _, t := range w.Tasks {
				fmt.Fprintf(&b, "%s: %s\n", t.Name, t.Prompt)
			}
		} else {
			b.WriteString(w.Prompt)
		}
		k := itemKey(KindWorkflow, w.Agent, w.ID)
		out[k] = item{key: k, text: clip(b.String()), meta: map[string]any{"kind": KindWorkflow, "agent": w.Agent, "name": clip(w.Name)}}
	}
	for _, d := range c.db.List("") {
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n%s\n%s\n", d.Name, d.ShortName, d.Description)
		if d.Kind == dashboards.KindPrompt {
			fmt.Fprintf(&b, "prompt report every %s\n%s", d.SyncInterval, d.Prompt)
		} else {
			fmt.Fprintf(&b, "data dashboard every %s\n", d.SyncInterval)
			for _, src := range d.Sources {
				fmt.Fprintf(&b, "%s %s\n", src.Type, src.Name)
			}
		}
		k := itemKey(KindDashboard, d.Agent, d.ID)
		out[k] = item{key: k, text: clip(b.String()), meta: map[string]any{"kind": KindDashboard, "agent": d.Agent, "name": clip(d.Name)}}
	}
	return out
}

func hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// Sync embeds every descriptor whose text changed since the last sync and
// removes vectors of deleted descriptors.
func (c *Index) Sync(ctx context.Context) error {
	ix := c.ix.Load()
	if ix == nil {
		return ErrDisabled
	}
	want := c.desired()
	have := &manifest{Items: map[string]string{}}
	if m, _, err := store.GetJSON[manifest](ctx, c.b, manifestKey); err == nil && m.Items != nil {
		have = m
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var upserts []vectors.Item
	next := make(map[string]string, len(want))
	for k, it := range want {
		h := hash(it.text)
		next[k] = h
		if have.Items[k] == h {
			continue
		}
		upserts = append(upserts, vectors.Item{Key: it.key, Text: it.text, Metadata: it.meta})
	}
	var removals []string
	for k := range have.Items {
		if _, ok := want[k]; !ok {
			removals = append(removals, k)
		}
	}
	if len(upserts) > 0 {
		if err := ix.Upsert(ctx, upserts); err != nil {
			return err
		}
	}
	if len(removals) > 0 {
		if err := ix.Delete(ctx, removals); err != nil {
			return err
		}
	}
	if len(upserts) > 0 || len(removals) > 0 {
		log.Printf("[catalog] indexed %d, removed %d (%d catalogued)", len(upserts), len(removals), len(next))
	}
	_, err := store.PutJSON(ctx, c.b, manifestKey, manifest{Items: next}, store.Condition{})
	return err
}

// StartSync runs Sync now and then every interval until ctx ends.
func (c *Index) StartSync(ctx context.Context, interval time.Duration) {
	if c == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	run := func() {
		safego.Run("catalog: sync", func() {
			if err := c.Sync(ctx); err != nil && !errors.Is(err, ErrDisabled) {
				log.Printf("[catalog] sync failed: %v", err)
			}
		})
	}
	safego.Go("catalog: sync loop", func() {
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	})
}

// Search returns the descriptors closest to query. kind and agent narrow the
// search when set.
func (c *Index) Search(ctx context.Context, kind, agent, query string, k int) ([]Hit, error) {
	ix := c.ix.Load()
	if ix == nil {
		return nil, ErrDisabled
	}
	k = max(1, min(k, MaxResults))
	fields := map[string]any{}
	if kind != "" {
		fields["kind"] = kind
	}
	if agent != "" {
		fields["agent"] = agent
	}
	var filter map[string]any
	if len(fields) > 0 {
		filter = vectors.Eq(fields)
	}
	matches, err := ix.Query(ctx, query, k*2, filter)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(matches))
	for _, m := range matches {
		parts := strings.Split(strings.TrimPrefix(m.Key, keyPrefix), "/")
		if !strings.HasPrefix(m.Key, keyPrefix) || len(parts) != 3 {
			continue
		}
		d := ix.CosineDistance(m.Distance)
		if d > maxDistance {
			continue
		}
		hit := Hit{Kind: parts[0], Agent: parts[1], ID: parts[2], Distance: d}
		switch hit.Kind {
		case KindWorkflow:
			w, ok := c.wf.Get(hit.Agent, hit.ID)
			if !ok {
				continue
			}
			hit.Name, hit.ShortName, hit.Description = w.Name, w.ShortName, w.Description
		case KindDashboard:
			dsh, ok := c.db.Get(hit.Agent, hit.ID)
			if !ok {
				continue
			}
			hit.Name, hit.ShortName, hit.Description = dsh.Name, dsh.ShortName, dsh.Description
		default:
			continue
		}
		hits = append(hits, hit)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Distance < hits[j].Distance })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}
