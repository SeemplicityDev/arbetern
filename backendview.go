package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/ttlcache"
	"github.com/justmike1/arbetern/internal/vectors"
)

const (
	backendListingTTL     = 30 * time.Second
	backendMaxObjects     = 20000
	backendMaxObjectBytes = 1 << 20
	backendMaxKeyLength   = 1024
	backendVectorSample   = 200
)

var sensitiveKeyRe = regexp.MustCompile(`(?i)(token|secret|passw|authorization|api[_-]?key|private[_-]?key|cookie|credential)`)

// backendView exposes the state bucket and the vector index read-only to the
// console, for the users its allow function admits.
type backendView struct {
	store   *store.Backend
	index   atomic.Pointer[vectors.Index]
	allow   func(*http.Request) bool
	listing *ttlcache.Cache[[]store.Object]
}

type backendObject struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

func newBackendView(b *store.Backend, allow func(*http.Request) bool) *backendView {
	v := &backendView{store: b, allow: allow}
	v.listing = ttlcache.New(backendListingTTL, func(ctx context.Context) ([]store.Object, error) {
		return b.List(ctx, "")
	})
	return v
}

func (v *backendView) setIndex(ix *vectors.Index) {
	if ix != nil {
		v.index.Store(ix)
	}
}

func (v *backendView) mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/backend", v.guard(v.handleSummary))
	mux.HandleFunc("/api/backend/objects", v.guard(v.handleObjects))
	mux.HandleFunc("/api/backend/object", v.guard(v.handleObject))
	mux.HandleFunc("/api/backend/vectors", v.guard(v.handleVectors))
}

func (v *backendView) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !v.allow(r) {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next(w, r)
	}
}

func (v *backendView) writeJSON(w http.ResponseWriter, payload any) {
	httpx.WriteJSON(w, http.StatusOK, payload)
}

func (v *backendView) objects(ctx context.Context) ([]store.Object, bool, error) {
	objs, err := v.listing.Get(ctx)
	if err != nil {
		return nil, false, err
	}
	if len(objs) > backendMaxObjects {
		return objs[:backendMaxObjects], true, nil
	}
	return objs, false, nil
}

func (v *backendView) handleSummary(w http.ResponseWriter, r *http.Request) {
	objs, truncated, err := v.objects(r.Context())
	if err != nil {
		http.Error(w, "failed to list state objects", http.StatusBadGateway)
		return
	}
	var bytes int64
	for _, o := range objs {
		bytes += o.Size
	}
	out := struct {
		State struct {
			Bucket    string `json:"bucket"`
			Prefix    string `json:"prefix"`
			Region    string `json:"region"`
			Objects   int    `json:"objects"`
			Bytes     int64  `json:"bytes"`
			Truncated bool   `json:"truncated"`
		} `json:"state"`
		Vectors *vectors.Info `json:"vectors"`
	}{}
	out.State.Bucket, out.State.Prefix, out.State.Region = v.store.Bucket(), v.store.Prefix(), v.store.Region()
	out.State.Objects, out.State.Bytes, out.State.Truncated = len(objs), bytes, truncated
	if ix := v.index.Load(); ix != nil {
		info := ix.Describe()
		out.Vectors = &info
	}
	v.writeJSON(w, out)
}

func (v *backendView) handleObjects(w http.ResponseWriter, r *http.Request) {
	objs, truncated, err := v.objects(r.Context())
	if err != nil {
		http.Error(w, "failed to list state objects", http.StatusBadGateway)
		return
	}
	list := make([]backendObject, 0, len(objs))
	for _, o := range objs {
		list = append(list, backendObject{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
	}
	v.writeJSON(w, struct {
		Objects   []backendObject `json:"objects"`
		Truncated bool            `json:"truncated"`
	}{list, truncated})
}

func validObjectKey(key string) bool {
	if key == "" || len(key) > backendMaxKeyLength || strings.HasPrefix(key, "/") {
		return false
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (v *backendView) handleObject(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if !validObjectKey(key) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	objs, _, err := v.objects(r.Context())
	if err != nil {
		http.Error(w, "failed to list state objects", http.StatusBadGateway)
		return
	}
	var meta *store.Object
	for i := range objs {
		if objs[i].Key == key {
			meta = &objs[i]
			break
		}
	}
	if meta == nil {
		http.Error(w, "object not found", http.StatusNotFound)
		return
	}
	out := struct {
		backendObject
		Kind    string `json:"kind"`
		Content string `json:"content,omitempty"`
	}{Key: meta.Key, Size: meta.Size, LastModified: meta.LastModified}
	if meta.Size > backendMaxObjectBytes {
		out.Kind = "large"
		v.writeJSON(w, out)
		return
	}
	body, _, err := v.store.Get(r.Context(), key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "object not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "failed to read object", http.StatusBadGateway)
		return
	}
	out.Kind, out.Content = renderObject(body)
	v.writeJSON(w, out)
}

// renderObject classifies the body and returns it as displayable text, with
// secret-looking JSON values masked.
func renderObject(body []byte) (kind, content string) {
	if !utf8.Valid(body) {
		return "binary", ""
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err == nil {
		pretty, err := json.MarshalIndent(redactSensitive(doc), "", "  ")
		if err == nil {
			return "json", string(pretty)
		}
	}
	return "text", string(body)
}

func redactSensitive(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKeyRe.MatchString(k) {
				if _, isContainer := val.(map[string]any); !isContainer {
					t[k] = "[redacted]"
					continue
				}
			}
			t[k] = redactSensitive(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactSensitive(t[i])
		}
		return t
	}
	return v
}

func (v *backendView) handleVectors(w http.ResponseWriter, r *http.Request) {
	ix := v.index.Load()
	if ix == nil {
		http.Error(w, "vector index not configured", http.StatusServiceUnavailable)
		return
	}
	sample, truncated, err := ix.Sample(r.Context(), backendVectorSample)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list vectors: %v", err), http.StatusBadGateway)
		return
	}
	for i := range sample {
		if m, ok := redactSensitive(sample[i].Metadata).(map[string]any); ok {
			sample[i].Metadata = m
		}
	}
	v.writeJSON(w, struct {
		Index     vectors.Info     `json:"index"`
		Vectors   []vectors.Vector `json:"vectors"`
		Truncated bool             `json:"truncated"`
	}{ix.Describe(), sample, truncated})
}
