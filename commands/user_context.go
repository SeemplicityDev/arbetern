package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/vectors"
)

// UserContextStore keeps a rolling log of per-user, per-agent turns in the
// state bucket so later requests from the same user can be grounded in what
// they asked before. With a vector index attached, the turns shown to the
// model are the ones semantically closest to the current question plus the
// latest few; without one, the most recent turns are shown.
//
// Layout: <userContextPrefix><agentID>/<userID>.json, one vector per entry
// keyed <agentID>/<userID>/<entryID>.
type UserContextStore struct {
	b     *store.Backend
	index *vectors.Index
}

const (
	userContextPrefix = "user-context/"
	// UserContextTTL is how long a user's context survives without activity.
	UserContextTTL = 30 * 24 * time.Hour
	// Entry and document caps. Indexed stores keep more history because only
	// the relevant part of it reaches the prompt.
	userContextMaxEntries         = 50
	userContextMaxEntriesIndexed  = 200
	userContextMaxDocBytes        = 96 * 1024
	userContextMaxDocBytesIndexed = 512 * 1024
	userContextMaxAnswerLen       = 1200
	userContextMaxQuestionLen     = 800
	// Prompt budget: the recency fallback may use the whole document; the
	// semantic selection is a handful of entries.
	userContextMaxPromptBytes   = 96 * 1024
	userContextMaxRelevantBytes = 24 * 1024
	userContextTopK             = 8
	userContextRecentAnchors    = 2
	userContextEntrySep         = "\n---\n"
	userContextIndexTimeout     = 30 * time.Second
)

// safeIDRe restricts agent IDs and Slack user IDs to characters that are
// safe inside object keys and vector keys.
var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type userContextDoc struct {
	Entries []userContextEntry `json:"entries"`
}

type userContextEntry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Question string    `json:"q"`
	Answer   string    `json:"a"`
}

// NewUserContextStore returns a store over b. index may be nil, in which case
// retrieval is recency-based.
func NewUserContextStore(b *store.Backend, index *vectors.Index) *UserContextStore {
	return &UserContextStore{b: b, index: index}
}

// Semantic reports whether retrieval is similarity-based.
func (s *UserContextStore) Semantic() bool { return s != nil && s.index != nil }

func (s *UserContextStore) key(agentID, userID string) string {
	if !safeIDRe.MatchString(agentID) || !safeIDRe.MatchString(userID) {
		return ""
	}
	return userContextPrefix + agentID + "/" + userID + ".json"
}

func vectorKeyPrefix(agentID, userID string) string { return agentID + "/" + userID + "/" }

// Context returns the user's prior turns worth showing the model alongside
// question, or "" when there are none or the store is not configured.
func (s *UserContextStore) Context(ctx context.Context, agentID, userID, question string) string {
	if s == nil {
		return ""
	}
	key := s.key(agentID, userID)
	if key == "" {
		return ""
	}
	doc, _, err := store.GetJSON[userContextDoc](ctx, s.b, key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[user-context] read failed agent=%s user=%s: %v", agentID, userID, err)
		}
		return ""
	}
	if len(doc.Entries) == 0 {
		return ""
	}
	if s.index != nil && strings.TrimSpace(question) != "" {
		picked, err := s.relevant(ctx, agentID, userID, question, doc.Entries)
		if err != nil {
			log.Printf("[user-context] semantic retrieval failed agent=%s user=%s: %v", agentID, userID, err)
		} else if len(picked) > 0 {
			return renderEntries(picked, userContextMaxRelevantBytes)
		}
	}
	return renderEntries(lastEntries(doc.Entries, userContextMaxEntries), userContextMaxPromptBytes)
}

// relevant picks the entries closest to question plus the latest few, in
// chronological order.
func (s *UserContextStore) relevant(ctx context.Context, agentID, userID, question string, entries []userContextEntry) ([]userContextEntry, error) {
	filter := vectors.Eq(map[string]any{"agent": agentID, "user": userID})
	matches, err := s.index.Query(ctx, question, userContextTopK, filter)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]userContextEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	prefix := vectorKeyPrefix(agentID, userID)
	seen := map[string]bool{}
	picked := make([]userContextEntry, 0, len(matches)+userContextRecentAnchors)
	for _, m := range matches {
		if !strings.HasPrefix(m.Key, prefix) {
			continue
		}
		id := strings.TrimPrefix(m.Key, prefix)
		if e, ok := byID[id]; ok && !seen[id] {
			seen[id] = true
			picked = append(picked, e)
		}
	}
	for _, e := range lastEntries(entries, userContextRecentAnchors) {
		if !seen[e.ID] {
			seen[e.ID] = true
			picked = append(picked, e)
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].At.Before(picked[j].At) })
	return picked, nil
}

func lastEntries(entries []userContextEntry, n int) []userContextEntry {
	if len(entries) > n {
		return entries[len(entries)-n:]
	}
	return entries
}

// renderEntries formats entries oldest first, dropping the oldest while the
// result exceeds maxBytes.
func renderEntries(entries []userContextEntry, maxBytes int) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("[%s]\nQ: %s\nA: %s", e.At.UTC().Format(time.RFC3339), e.Question, e.Answer))
	}
	out := strings.Join(parts, userContextEntrySep)
	for len(parts) > 1 && len(out) > maxBytes {
		parts = parts[1:]
		out = strings.Join(parts, userContextEntrySep)
	}
	return out
}

// Append records a completed (question, answer) turn, trims the document to
// its caps, and indexes the new entry when a vector index is attached.
// Errors are logged, never returned: context persistence is best-effort.
func (s *UserContextStore) Append(ctx context.Context, agentID, userID, question, answer string) {
	if s == nil {
		return
	}
	key := s.key(agentID, userID)
	if key == "" {
		return
	}
	question = truncate(strings.TrimSpace(question), userContextMaxQuestionLen)
	answer = truncate(strings.TrimSpace(answer), userContextMaxAnswerLen)
	if question == "" && answer == "" {
		return
	}
	id, err := store.NewID()
	if err != nil {
		log.Printf("[user-context] id generation failed: %v", err)
		return
	}
	entry := userContextEntry{ID: id, At: time.Now().UTC(), Question: question, Answer: answer}
	maxEntries, maxBytes := userContextMaxEntries, userContextMaxDocBytes
	if s.index != nil {
		maxEntries, maxBytes = userContextMaxEntriesIndexed, userContextMaxDocBytesIndexed
	}
	var dropped []string
	err = s.update(ctx, key, func(d *userContextDoc) {
		dropped = dropped[:0]
		d.Entries = append(d.Entries, entry)
		for len(d.Entries) > 1 && (len(d.Entries) > maxEntries || d.size() > maxBytes) {
			dropped = append(dropped, d.Entries[0].ID)
			d.Entries = d.Entries[1:]
		}
	})
	if err != nil {
		log.Printf("[user-context] write failed agent=%s user=%s: %v", agentID, userID, err)
		return
	}
	if s.index == nil {
		return
	}
	safego.Go("user context: index", func() {
		ictx, cancel := context.WithTimeout(context.Background(), userContextIndexTimeout)
		defer cancel()
		item := vectors.Item{
			Key:      vectorKeyPrefix(agentID, userID) + entry.ID,
			Text:     "Q: " + entry.Question + "\nA: " + entry.Answer,
			Metadata: map[string]any{"agent": agentID, "user": userID, "at": entry.At.Unix()},
		}
		if err := s.index.Upsert(ictx, []vectors.Item{item}); err != nil {
			log.Printf("[user-context] index failed agent=%s user=%s: %v", agentID, userID, err)
		}
		if len(dropped) == 0 {
			return
		}
		keys := make([]string, 0, len(dropped))
		for _, id := range dropped {
			keys = append(keys, vectorKeyPrefix(agentID, userID)+id)
		}
		if err := s.index.Delete(ictx, keys); err != nil {
			log.Printf("[user-context] unindex failed agent=%s user=%s: %v", agentID, userID, err)
		}
	})
}

func (d *userContextDoc) size() int {
	n := 0
	for _, e := range d.Entries {
		n += len(e.Question) + len(e.Answer) + 96
	}
	return n
}

// update applies fn to the latest document and writes it back, creating the
// document when it does not exist and retrying when another replica wrote in
// between.
func (s *UserContextStore) update(ctx context.Context, key string, fn func(*userContextDoc)) error {
	for attempt := 0; attempt < 4; attempt++ {
		doc, tag, err := store.GetJSON[userContextDoc](ctx, s.b, key)
		cond := store.Condition{IfMatch: tag}
		switch {
		case errors.Is(err, store.ErrNotFound):
			doc = &userContextDoc{}
			cond = store.Condition{IfNoneMatch: true}
		case err != nil:
			return err
		}
		fn(doc)
		if _, err := store.PutJSON(ctx, s.b, key, doc, cond); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return store.ErrConflict
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// StartGC deletes the documents (and vectors) of users inactive for longer
// than UserContextTTL, once at start and then every interval until ctx ends.
func (s *UserContextStore) StartGC(ctx context.Context, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	sweep := func() { safego.Run("user context: sweep", func() { s.sweep(ctx) }) }
	safego.Go("user context: sweep loop", func() {
		sweep()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweep()
			}
		}
	})
}

func (s *UserContextStore) sweep(ctx context.Context) {
	objs, err := s.b.List(ctx, userContextPrefix)
	if err != nil {
		log.Printf("[user-context] sweep error: %v", err)
		return
	}
	cutoff := time.Now().Add(-UserContextTTL)
	removed := 0
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") || o.LastModified.After(cutoff) {
			continue
		}
		if s.index != nil {
			s.unindexAll(ctx, o.Key)
		}
		if err := s.b.Delete(ctx, o.Key, ""); err != nil {
			log.Printf("[user-context] delete %s: %v", o.Key, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Printf("[user-context] gc removed %d stale document(s)", removed)
	}
}

func (s *UserContextStore) unindexAll(ctx context.Context, key string) {
	doc, _, err := store.GetJSON[userContextDoc](ctx, s.b, key)
	if err != nil || len(doc.Entries) == 0 {
		return
	}
	prefix := strings.TrimSuffix(strings.TrimPrefix(key, userContextPrefix), ".json") + "/"
	keys := make([]string, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		keys = append(keys, prefix+e.ID)
	}
	if err := s.index.Delete(ctx, keys); err != nil {
		log.Printf("[user-context] unindex %s: %v", key, err)
	}
}
