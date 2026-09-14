package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/vectors"

	"github.com/justmike1/arbetern/internal/text"
)

// UserContextStore keeps a rolling log of per-user, per-agent turns in the
// state bucket. It serves three views of that log: the turns of the last few
// minutes in the same channel (the working memory of a conversation), the
// older turns semantically close to the current question when a vector
// index is attached (or the latest turns without one), and, for agents that
// opt in, related turns of other users.
//
// Layout: <userContextPrefix><agentID>/<userID>.json, one vector per entry
// keyed <agentID>/<userID>/<entryID>.
type UserContextStore struct {
	b      *store.Backend
	index  atomic.Pointer[vectors.Index]
	shared atomic.Pointer[map[string]bool]
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
	userContextMaxSharedBytes   = 8 * 1024
	userContextTopK             = 8
	userContextSharedTopK       = 4
	userContextRecentAnchors    = 2
	userContextEntrySep         = "\n---\n"
	userContextIndexTimeout     = 30 * time.Second
	// Working memory: turns in the same channel within this window.
	userContextRecentWindow = 10 * time.Minute
	userContextRecentTurns  = 10
	// Only turns from this window are searched semantically.
	userContextSearchWindow = 90 * 24 * time.Hour
	// Cosine-distance cutoffs: a match must be close in absolute terms and
	// not much farther than the best match.
	userContextMaxDistance       = 0.6
	userContextSharedMaxDistance = 0.45
	userContextDistanceSpread    = 0.2
	// Vectors younger than this are never treated as orphans by the repair
	// sweep, since their document write may still be in flight.
	userContextOrphanGrace = time.Hour
)

// safeIDRe restricts agent IDs and user IDs to characters that are safe
// inside object keys and vector keys.
var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var userIDCleanRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// UserContextID maps any user identity (a Slack ID, an email) to a stable
// key-safe identifier.
func UserContextID(user string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		return ""
	}
	if safeIDRe.MatchString(user) {
		return user
	}
	lower := strings.ToLower(user)
	slug := strings.Trim(userIDCleanRe.ReplaceAllString(lower, "_"), "_")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(lower))
	return slug + "-" + hex.EncodeToString(sum[:4])
}

type userContextDoc struct {
	Entries []userContextEntry `json:"entries"`
}

type userContextEntry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Channel  string    `json:"c,omitempty"`
	Question string    `json:"q"`
	Answer   string    `json:"a"`
}

// UserContextView is what the handlers add to the system prompt.
type UserContextView struct {
	// Recent is the conversation of the last few minutes in the same channel.
	Recent string
	// Relevant is the user's older turns that relate to the question.
	Relevant string
	// Shared is related turns of other users, for agents with shared memory.
	Shared string
}

// Empty reports whether no context was found.
func (v UserContextView) Empty() bool { return v.Recent == "" && v.Relevant == "" && v.Shared == "" }

// NewUserContextStore returns a store over b. index may be nil, in which case
// retrieval is recency-based until SetIndex installs one.
func NewUserContextStore(b *store.Backend, index *vectors.Index) *UserContextStore {
	s := &UserContextStore{b: b}
	s.index.Store(index)
	empty := map[string]bool{}
	s.shared.Store(&empty)
	return s
}

// SetIndex switches retrieval to the given vector index; nil returns to
// recency. Safe to call while requests are in flight.
func (s *UserContextStore) SetIndex(index *vectors.Index) {
	if s != nil {
		s.index.Store(index)
	}
}

// SetSharedAgents names the agents whose users may see each other's related
// turns.
func (s *UserContextStore) SetSharedAgents(agentIDs []string) {
	if s == nil {
		return
	}
	m := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		m[id] = true
	}
	s.shared.Store(&m)
}

// Semantic reports whether retrieval is similarity-based.
func (s *UserContextStore) Semantic() bool { return s != nil && s.index.Load() != nil }

func (s *UserContextStore) key(agentID, userID string) string {
	if !safeIDRe.MatchString(agentID) || !safeIDRe.MatchString(userID) {
		return ""
	}
	return userContextPrefix + agentID + "/" + userID + ".json"
}

func vectorKeyPrefix(agentID, userID string) string { return agentID + "/" + userID + "/" }

func (s *UserContextStore) load(ctx context.Context, agentID, userID string) *userContextDoc {
	key := s.key(agentID, userID)
	if key == "" {
		return nil
	}
	doc, _, err := store.GetJSON[userContextDoc](ctx, s.b, key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[user-context] read failed agent=%s user=%s: %v", agentID, userID, err)
		}
		return nil
	}
	return doc
}

// Context returns the user's prior turns worth showing the model alongside
// question. channelID scopes the working memory and may be empty.
func (s *UserContextStore) Context(ctx context.Context, agentID, userID, channelID, question string) UserContextView {
	var view UserContextView
	if s == nil {
		return view
	}
	userID = UserContextID(userID)
	doc := s.load(ctx, agentID, userID)
	index := s.index.Load()
	if doc != nil && len(doc.Entries) > 0 {
		recent := recentEntries(doc.Entries, channelID, time.Now())
		view.Recent = renderConversation(recent)
		seen := make(map[string]bool, len(recent))
		for _, e := range recent {
			seen[e.ID] = true
		}
		older := make([]userContextEntry, 0, len(doc.Entries))
		for _, e := range doc.Entries {
			if !seen[e.ID] {
				older = append(older, e)
			}
		}
		if index != nil && strings.TrimSpace(question) != "" {
			picked, err := s.relevant(ctx, index, agentID, userID, question, older)
			if err != nil {
				log.Printf("[user-context] semantic retrieval failed agent=%s user=%s: %v", agentID, userID, err)
				view.Relevant = renderEntries(lastEntries(older, userContextMaxEntries), userContextMaxPromptBytes)
			} else {
				view.Relevant = renderEntries(picked, userContextMaxRelevantBytes)
			}
		} else {
			view.Relevant = renderEntries(lastEntries(older, userContextMaxEntries), userContextMaxPromptBytes)
		}
	}
	if index != nil && (*s.shared.Load())[agentID] && strings.TrimSpace(question) != "" {
		shared, err := s.sharedEntries(ctx, index, agentID, userID, question)
		if err != nil {
			log.Printf("[user-context] shared retrieval failed agent=%s: %v", agentID, err)
		} else {
			view.Shared = renderShared(shared, userContextMaxSharedBytes)
		}
	}
	return view
}

// recentEntries is the same-channel conversation of the last few minutes.
func recentEntries(entries []userContextEntry, channelID string, now time.Time) []userContextEntry {
	if channelID == "" {
		return nil
	}
	cutoff := now.Add(-userContextRecentWindow)
	var out []userContextEntry
	for _, e := range entries {
		if e.Channel == channelID && e.At.After(cutoff) {
			out = append(out, e)
		}
	}
	return lastEntries(out, userContextRecentTurns)
}

func searchFilter(agentID string, extra ...any) map[string]any {
	since := time.Now().Add(-userContextSearchWindow).Unix()
	return vectors.And(append([]any{vectors.Eq(map[string]any{"agent": agentID}), vectors.Gte("at", since)}, extra...)...)
}

// keepClose drops matches that are far from the question in absolute terms
// or relative to the best match.
func keepClose(index *vectors.Index, matches []vectors.Match, maxDistance float32) []vectors.Match {
	var out []vectors.Match
	best := float32(-1)
	for _, m := range matches {
		d := index.CosineDistance(m.Distance)
		if best < 0 || d < best {
			best = d
		}
	}
	for _, m := range matches {
		d := index.CosineDistance(m.Distance)
		if d <= maxDistance && d <= best+userContextDistanceSpread {
			out = append(out, m)
		}
	}
	return out
}

// relevant picks the entries closest to question plus the latest few, in
// chronological order.
func (s *UserContextStore) relevant(ctx context.Context, index *vectors.Index, agentID, userID, question string, entries []userContextEntry) ([]userContextEntry, error) {
	byID := make(map[string]userContextEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	seen := map[string]bool{}
	picked := make([]userContextEntry, 0, userContextTopK+userContextRecentAnchors)
	if len(entries) > 0 {
		matches, err := index.Query(ctx, question, userContextTopK, searchFilter(agentID, vectors.Eq(map[string]any{"user": userID})))
		if err != nil {
			return nil, err
		}
		prefix := vectorKeyPrefix(agentID, userID)
		for _, m := range keepClose(index, matches, userContextMaxDistance) {
			if !strings.HasPrefix(m.Key, prefix) {
				continue
			}
			id := strings.TrimPrefix(m.Key, prefix)
			if e, ok := byID[id]; ok && !seen[id] {
				seen[id] = true
				picked = append(picked, e)
			}
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

// sharedEntries finds related turns of other users of the same agent.
func (s *UserContextStore) sharedEntries(ctx context.Context, index *vectors.Index, agentID, userID, question string) ([]userContextEntry, error) {
	matches, err := index.Query(ctx, question, userContextSharedTopK*2, searchFilter(agentID, vectors.Ne("user", userID)))
	if err != nil {
		return nil, err
	}
	docs := map[string]*userContextDoc{}
	var out []userContextEntry
	for _, m := range keepClose(index, matches, userContextSharedMaxDistance) {
		parts := strings.Split(m.Key, "/")
		if len(parts) != 3 || parts[0] != agentID || parts[1] == userID {
			continue
		}
		doc, ok := docs[parts[1]]
		if !ok {
			doc = s.load(ctx, agentID, parts[1])
			docs[parts[1]] = doc
		}
		if doc == nil {
			continue
		}
		for _, e := range doc.Entries {
			if e.ID == parts[2] {
				out = append(out, e)
				break
			}
		}
		if len(out) >= userContextSharedTopK {
			break
		}
	}
	return out, nil
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
	return joinWithin(parts, maxBytes)
}

// renderConversation formats the working memory as alternating turns.
func renderConversation(entries []userContextEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "User: %s\n", e.Question)
		if e.Answer != "" {
			fmt.Fprintf(&sb, "Assistant: %s\n", e.Answer)
		}
	}
	return sb.String()
}

// renderShared formats other users' turns without naming them.
func renderShared(entries []userContextEntry, maxBytes int) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("[%s] A teammate asked: %s\nAnswer given: %s", e.At.UTC().Format("2006-01-02"), e.Question, e.Answer))
	}
	return joinWithin(parts, maxBytes)
}

func joinWithin(parts []string, maxBytes int) string {
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
func (s *UserContextStore) Append(ctx context.Context, agentID, userID, channelID, question, answer string) {
	if s == nil {
		return
	}
	userID = UserContextID(userID)
	key := s.key(agentID, userID)
	if key == "" {
		return
	}
	question = text.Truncate(strings.TrimSpace(question), userContextMaxQuestionLen)
	answer = text.Truncate(strings.TrimSpace(answer), userContextMaxAnswerLen)
	if question == "" && answer == "" {
		return
	}
	id, err := store.NewID()
	if err != nil {
		log.Printf("[user-context] id generation failed: %v", err)
		return
	}
	entry := userContextEntry{ID: id, At: time.Now().UTC(), Channel: channelID, Question: question, Answer: answer}
	index := s.index.Load()
	maxEntries, maxBytes := userContextMaxEntries, userContextMaxDocBytes
	if index != nil {
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
	if index == nil {
		return
	}
	safego.Go("user context: index", func() {
		ictx, cancel := context.WithTimeout(context.Background(), userContextIndexTimeout)
		defer cancel()
		if err := index.Upsert(ictx, []vectors.Item{vectorItem(agentID, userID, entry)}); err != nil {
			log.Printf("[user-context] index failed agent=%s user=%s: %v", agentID, userID, err)
		}
		if len(dropped) == 0 {
			return
		}
		keys := make([]string, 0, len(dropped))
		for _, id := range dropped {
			keys = append(keys, vectorKeyPrefix(agentID, userID)+id)
		}
		if err := index.Delete(ictx, keys); err != nil {
			log.Printf("[user-context] unindex failed agent=%s user=%s: %v", agentID, userID, err)
		}
	})
}

func vectorItem(agentID, userID string, e userContextEntry) vectors.Item {
	return vectors.Item{
		Key:      vectorKeyPrefix(agentID, userID) + e.ID,
		Text:     "Q: " + e.Question + "\nA: " + e.Answer,
		Metadata: map[string]any{"agent": agentID, "user": userID, "at": e.At.Unix()},
	}
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

// StartGC removes the documents (and vectors) of users inactive for longer
// than UserContextTTL and repairs the index against the documents, once at
// start and then every interval until ctx ends. One replica should run it.
func (s *UserContextStore) StartGC(ctx context.Context, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	sweep := func() {
		safego.Run("user context: sweep", func() { s.sweep(ctx) })
		safego.Run("user context: repair", func() { s.repair(ctx) })
	}
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
	index := s.index.Load()
	removed := 0
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") || o.LastModified.After(cutoff) {
			continue
		}
		if index != nil {
			s.unindexAll(ctx, index, o.Key)
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

func (s *UserContextStore) unindexAll(ctx context.Context, index *vectors.Index, key string) {
	doc, _, err := store.GetJSON[userContextDoc](ctx, s.b, key)
	if err != nil || len(doc.Entries) == 0 {
		return
	}
	prefix := strings.TrimSuffix(strings.TrimPrefix(key, userContextPrefix), ".json") + "/"
	keys := make([]string, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		keys = append(keys, prefix+e.ID)
	}
	if err := index.Delete(ctx, keys); err != nil {
		log.Printf("[user-context] unindex %s: %v", key, err)
	}
}

// repair indexes entries whose vector is missing (an indexing call that
// failed, or turns recorded while the index was unavailable) and deletes
// vectors whose entry is gone.
func (s *UserContextStore) repair(ctx context.Context) {
	index := s.index.Load()
	if index == nil {
		return
	}
	objs, err := s.b.List(ctx, userContextPrefix)
	if err != nil {
		log.Printf("[user-context] repair list error: %v", err)
		return
	}
	expected := map[string]vectors.Item{}
	for _, o := range objs {
		rel := strings.TrimSuffix(strings.TrimPrefix(o.Key, userContextPrefix), ".json")
		agentID, userID, ok := strings.Cut(rel, "/")
		if !ok || !strings.HasSuffix(o.Key, ".json") || !safeIDRe.MatchString(agentID) || !safeIDRe.MatchString(userID) {
			continue
		}
		doc, _, err := store.GetJSON[userContextDoc](ctx, s.b, o.Key)
		if err != nil {
			continue
		}
		for _, e := range doc.Entries {
			item := vectorItem(agentID, userID, e)
			expected[item.Key] = item
		}
	}
	keys := make([]string, 0, len(expected))
	for k := range expected {
		keys = append(keys, k)
	}
	present, err := index.Exists(ctx, keys)
	if err != nil {
		log.Printf("[user-context] repair check error: %v", err)
		return
	}
	var missing []vectors.Item
	for _, k := range keys {
		if !present[k] {
			missing = append(missing, expected[k])
		}
	}
	if len(missing) > 0 {
		if err := index.Upsert(ctx, missing); err != nil {
			log.Printf("[user-context] repair index error: %v", err)
		} else {
			log.Printf("[user-context] repair indexed %d missing vector(s)", len(missing))
		}
	}
	grace := time.Now().Add(-userContextOrphanGrace).Unix()
	var orphans []string
	err = index.Each(ctx, func(key string, meta map[string]any) bool {
		if strings.Count(key, "/") != 2 || strings.HasPrefix(key, "registry/") {
			return true
		}
		if _, ok := expected[key]; ok {
			return true
		}
		if at, ok := meta["at"].(float64); ok && int64(at) > grace {
			return true
		}
		orphans = append(orphans, key)
		return true
	})
	if err != nil {
		log.Printf("[user-context] repair scan error: %v", err)
		return
	}
	if len(orphans) > 0 {
		if err := index.Delete(ctx, orphans); err != nil {
			log.Printf("[user-context] repair delete error: %v", err)
		} else {
			log.Printf("[user-context] repair removed %d orphan vector(s)", len(orphans))
		}
	}
}
