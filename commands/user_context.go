package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justmike1/arbetern/internal/queue"
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
	tasks  atomic.Pointer[queue.Queue]
	linker atomic.Pointer[IdentityLinker]
}

// IdentityLinker maps one stored identity key to the other keys the same
// person is recorded under — the Slack ID the bot sees and the email the
// console signs them in with are the same person, and only the caller can
// resolve one to the other.
type IdentityLinker func(ctx context.Context, id string) []string

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
	// userContextReadTimeout bounds the retrieval a turn does before its first
	// model call. Everything it fetches is optional — the prompt falls back to
	// recency — so a degraded embeddings or vector backend must cost this much
	// and no more, rather than stalling the answer behind its own retries.
	userContextReadTimeout = 20 * time.Second
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
	// Aggregated per-person profiles: one document per identity, holding that
	// person's turns across every agent. Larger than a single agent's document
	// because it merges all of them, and capped so the prompt-side read and
	// the console page stay one bounded object.
	userProfilePrefix   = "user-profiles/"
	userProfileMaxTurns = 300
	userProfileMaxBytes = 256 * 1024
	// What the profile contributes to another agent's prompt: the person's
	// latest questions elsewhere, never the answers.
	userProfileElsewhereTurns = 6
	userProfileElsewhereBytes = 2 * 1024
	// One deferred or background profile write.
	userProfileWriteTimeout = 60 * time.Second
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
	// Elsewhere is what the same person recently asked the other agents,
	// questions only, from their aggregated profile.
	Elsewhere string
}

// Empty reports whether no context was found.
func (v UserContextView) Empty() bool {
	return v.Recent == "" && v.Relevant == "" && v.Shared == "" && v.Elsewhere == ""
}

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

// SetIdentityLinker installs the resolver that folds a person's identities
// together. Without one every identity is treated as its own person.
func (s *UserContextStore) SetIdentityLinker(fn IdentityLinker) {
	if s != nil && fn != nil {
		s.linker.Store(&fn)
	}
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
//
// The agent's own document is the authority for this agent's turns. The
// person's cached profile adds what the same person said to this agent under
// another identity — the Slack user and the console sign-in are one person —
// and what they have been asking the other agents.
func (s *UserContextStore) Context(ctx context.Context, agentID, userID, channelID, question string) UserContextView {
	var view UserContextView
	if s == nil {
		return view
	}
	ctx, cancel := context.WithTimeout(ctx, userContextReadTimeout)
	defer cancel()
	userID = UserContextID(userID)
	doc := s.load(ctx, agentID, userID)
	index := s.index.Load()
	ids := []string{userID}
	prof, cached := s.cachedProfile(ctx, ids)
	if cached {
		if len(prof.Identities) > 0 {
			ids = prof.Identities
		}
		view.Elsewhere = renderElsewhere(prof.elsewhere(agentID, userProfileElsewhereTurns), userProfileElsewhereBytes)
	}
	entries := mergedEntries(doc, prof, agentID, userID)
	if len(entries) > 0 {
		recent := recentEntries(entries, channelID, time.Now())
		view.Recent = renderConversation(recent)
		seen := make(map[string]bool, len(recent))
		for _, e := range recent {
			seen[e.ID] = true
		}
		older := make([]userContextEntry, 0, len(entries))
		for _, e := range entries {
			if !seen[e.ID] {
				older = append(older, e)
			}
		}
		if index != nil && strings.TrimSpace(question) != "" {
			picked, err := s.relevant(ctx, index, agentID, ids, question, older)
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
		shared, err := s.sharedEntries(ctx, index, agentID, ids, question)
		if err != nil {
			log.Printf("[user-context] shared retrieval failed agent=%s: %v", agentID, err)
		} else {
			view.Shared = renderShared(shared, userContextMaxSharedBytes)
		}
	}
	return view
}

// mergedEntries is the agent's own document plus the turns this person had
// with the same agent under their other identities, oldest first.
func mergedEntries(doc *userContextDoc, prof *UserContextProfile, agentID, userID string) []userContextEntry {
	var out []userContextEntry
	if doc != nil {
		out = append(out, doc.Entries...)
	}
	if prof != nil {
		seen := make(map[string]bool, len(out))
		for _, e := range out {
			seen[e.ID] = true
		}
		for _, t := range prof.Turns {
			if t.Agent != agentID || t.Identity == userID || seen[t.ID] {
				continue
			}
			out = append(out, userContextEntry{ID: t.ID, At: t.At, Channel: t.Channel, Question: t.Question, Answer: t.Answer})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
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
func (s *UserContextStore) relevant(ctx context.Context, index *vectors.Index, agentID string, ids []string, question string, entries []userContextEntry) ([]userContextEntry, error) {
	byID := make(map[string]userContextEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	seen := map[string]bool{}
	picked := make([]userContextEntry, 0, userContextTopK+userContextRecentAnchors)
	if len(entries) > 0 {
		matches, err := index.Query(ctx, question, userContextTopK, searchFilter(agentID, vectors.In("user", ids)))
		if err != nil {
			return nil, err
		}
		mine := idSet(ids)
		for _, m := range keepClose(index, matches, userContextMaxDistance) {
			parts := strings.Split(m.Key, "/")
			if len(parts) != 3 || parts[0] != agentID || !mine[parts[1]] {
				continue
			}
			id := parts[2]
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
func (s *UserContextStore) sharedEntries(ctx context.Context, index *vectors.Index, agentID string, ids []string, question string) ([]userContextEntry, error) {
	matches, err := index.Query(ctx, question, userContextSharedTopK*2, searchFilter(agentID, vectors.NotIn("user", ids)))
	if err != nil {
		return nil, err
	}
	mine := idSet(ids)
	docs := map[string]*userContextDoc{}
	var out []userContextEntry
	for _, m := range keepClose(index, matches, userContextSharedMaxDistance) {
		parts := strings.Split(m.Key, "/")
		if len(parts) != 3 || parts[0] != agentID || mine[parts[1]] {
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

// renderElsewhere lists what the person recently asked the other agents,
// oldest first. Questions only: the answers are those agents' context, and
// what is useful here is what this person is working on.
func renderElsewhere(turns []UserContextTurn, maxBytes int) string {
	parts := make([]string, 0, len(turns))
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if strings.TrimSpace(t.Question) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s] to %s: %s", t.At.UTC().Format("2006-01-02"), t.Agent, t.Question))
	}
	return joinLines(parts, maxBytes)
}

func joinLines(parts []string, maxBytes int) string {
	out := strings.Join(parts, "\n")
	for len(parts) > 1 && len(out) > maxBytes {
		parts = parts[1:]
		out = strings.Join(parts, "\n")
	}
	return out
}

func joinWithin(parts []string, maxBytes int) string {
	out := strings.Join(parts, userContextEntrySep)
	for len(parts) > 1 && len(out) > maxBytes {
		parts = parts[1:]
		out = strings.Join(parts, userContextEntrySep)
	}
	return out
}

// UserContextTurn is one remembered turn, as the console shows it back to the
// person it belongs to and as the profile carries it between agents.
type UserContextTurn struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Identity string    `json:"identity,omitempty"`
	At       time.Time `json:"at"`
	Channel  string    `json:"channel,omitempty"`
	Question string    `json:"question"`
	Answer   string    `json:"answer"`
}

// UserContextAgentMemory is what one agent remembers of one person.
type UserContextAgentMemory struct {
	Agent  string    `json:"agent"`
	Key    string    `json:"key"`
	Turns  int       `json:"turns"`
	Bytes  int64     `json:"bytes"`
	Oldest time.Time `json:"oldest"`
	Newest time.Time `json:"newest"`
}

// UserContextProfile is one person's whole stored context — every agent that
// remembers them, every turn, newest first — aggregated from the per-agent
// documents and cached in the bucket under each of the person's identities.
// The per-agent documents remain the source of truth; this is derived state,
// rebuilt from them on a schedule and safe to delete at any time.
type UserContextProfile struct {
	Identities []string                 `json:"identities"`
	Agents     []UserContextAgentMemory `json:"agents"`
	Turns      []UserContextTurn        `json:"turns"`
	Bytes      int64                    `json:"bytes"`
	Updated    time.Time                `json:"updated"`
	// Fingerprint covers what the profile is made of, not when it was made, so
	// a rebuild that finds nothing new writes nothing.
	Fingerprint string `json:"fingerprint,omitempty"`

	// Filled in on read from the store's current configuration rather than
	// from the cached document, which may predate a config change.
	Semantic      bool `json:"semantic"`
	RetentionDays int  `json:"retention_days"`
	MaxTurns      int  `json:"max_turns"`
}

func profileKey(id string) string { return userProfilePrefix + id + ".json" }

func idSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// elsewhere is the person's most recent turns with agents other than agentID.
func (p *UserContextProfile) elsewhere(agentID string, n int) []UserContextTurn {
	out := make([]UserContextTurn, 0, n)
	for _, t := range p.Turns {
		if t.Agent == agentID {
			continue
		}
		out = append(out, t)
		if len(out) == n {
			break
		}
	}
	return out
}

// trim drops the oldest turns until the profile fits both caps.
func (p *UserContextProfile) trim() {
	if len(p.Turns) > userProfileMaxTurns {
		p.Turns = p.Turns[:userProfileMaxTurns]
	}
	size := 0
	for _, t := range p.Turns {
		size += len(t.Question) + len(t.Answer) + 128
	}
	for len(p.Turns) > 1 && size > userProfileMaxBytes {
		last := p.Turns[len(p.Turns)-1]
		size -= len(last.Question) + len(last.Answer) + 128
		p.Turns = p.Turns[:len(p.Turns)-1]
	}
}

func (p *UserContextProfile) stamp() {
	h := sha256.New()
	for _, id := range p.Identities {
		_, _ = h.Write([]byte(id + "\x00"))
	}
	for _, t := range p.Turns {
		_, _ = h.Write([]byte(t.Agent + "/" + t.ID + "\x00"))
	}
	p.Fingerprint = hex.EncodeToString(h.Sum(nil)[:16])
	p.Updated = time.Now().UTC()
}

// insert adds a turn the person has just had, keeping the profile ordered and
// within its caps. It reports whether anything changed.
func (p *UserContextProfile) insert(turn UserContextTurn, ids []string) bool {
	for _, t := range p.Turns {
		if t.ID == turn.ID {
			return false
		}
	}
	at := len(p.Turns)
	for i, t := range p.Turns {
		if turn.At.After(t.At) {
			at = i
			break
		}
	}
	p.Turns = append(p.Turns, UserContextTurn{})
	copy(p.Turns[at+1:], p.Turns[at:])
	p.Turns[at] = turn

	found := false
	for i := range p.Agents {
		if p.Agents[i].Agent != turn.Agent {
			continue
		}
		found = true
		p.Agents[i].Turns++
		if turn.At.After(p.Agents[i].Newest) {
			p.Agents[i].Newest = turn.At
		}
		if p.Agents[i].Oldest.IsZero() || turn.At.Before(p.Agents[i].Oldest) {
			p.Agents[i].Oldest = turn.At
		}
	}
	if !found {
		p.Agents = append(p.Agents, UserContextAgentMemory{
			Agent: turn.Agent, Key: userContextPrefix + turn.Agent + "/" + turn.Identity + ".json",
			Turns: 1, Oldest: turn.At, Newest: turn.At,
		})
		sort.Slice(p.Agents, func(i, j int) bool { return p.Agents[i].Agent < p.Agents[j].Agent })
	}
	p.Bytes += int64(len(turn.Question) + len(turn.Answer) + 96)
	if len(ids) > len(p.Identities) {
		p.Identities = ids
	}
	p.trim()
	p.stamp()
	return true
}

// describe fills the fields that come from the store's current configuration
// rather than from the cached document.
func (s *UserContextStore) describe(p *UserContextProfile) {
	p.Semantic = s.Semantic()
	p.RetentionDays = int(UserContextTTL / (24 * time.Hour))
	p.MaxTurns = userContextMaxEntries
	if s.Semantic() {
		p.MaxTurns = userContextMaxEntriesIndexed
	}
	if p.Identities == nil {
		p.Identities = []string{}
	}
	if p.Agents == nil {
		p.Agents = []UserContextAgentMemory{}
	}
	if p.Turns == nil {
		p.Turns = []UserContextTurn{}
	}
}

// identities expands the given keys into every identity of the same person,
// in a stable order, so the same person always produces the same set.
func (s *UserContextStore) identities(ctx context.Context, ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids)+1)
	add := func(raw string) {
		id := UserContextID(raw)
		if id == "" || seen[id] || !safeIDRe.MatchString(id) {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range ids {
		add(id)
	}
	if link := s.linker.Load(); link != nil {
		for _, id := range append([]string(nil), out...) {
			for _, alias := range (*link)(ctx, id) {
				add(alias)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Profile returns a person's aggregated context: the cached document when one
// exists, otherwise built from the per-agent documents and cached in the
// background, so a person who has never been aggregated still gets an answer.
func (s *UserContextStore) Profile(ctx context.Context, ids []string) (UserContextProfile, error) {
	var out UserContextProfile
	if s == nil {
		s.describe(&out)
		return out, nil
	}
	expanded := s.identities(ctx, ids)
	if len(expanded) == 0 {
		s.describe(&out)
		return out, nil
	}
	if prof, ok := s.cachedProfile(ctx, expanded); ok {
		s.describe(prof)
		return *prof, nil
	}
	prof, err := s.aggregate(ctx, expanded, nil)
	if err != nil {
		s.describe(&prof)
		return prof, err
	}
	s.describe(&prof)
	if len(prof.Turns) > 0 {
		cached := prof
		safego.Go("user context: cache profile", func() {
			wctx, cancel := context.WithTimeout(context.Background(), userProfileWriteTimeout)
			defer cancel()
			if _, err := s.writeProfile(wctx, cached); err != nil {
				log.Printf("[user-context] profile cache write failed: %v", err)
			}
		})
	}
	return prof, nil
}

// cachedProfile reads the stored aggregate under the first of ids that has one.
func (s *UserContextStore) cachedProfile(ctx context.Context, ids []string) (*UserContextProfile, bool) {
	for _, id := range ids {
		if id == "" || !safeIDRe.MatchString(id) {
			continue
		}
		prof, _, err := store.GetJSON[UserContextProfile](ctx, s.b, profileKey(id))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("[user-context] profile read failed id=%s: %v", id, err)
			}
			continue
		}
		return prof, true
	}
	return nil, false
}

// aggregate builds a person's profile from the per-agent documents. objs is a
// listing of userContextPrefix already in hand, or nil to list it.
func (s *UserContextStore) aggregate(ctx context.Context, ids []string, objs []store.Object) (UserContextProfile, error) {
	out := UserContextProfile{Identities: ids, Agents: []UserContextAgentMemory{}, Turns: []UserContextTurn{}}
	if len(ids) == 0 {
		return out, nil
	}
	if objs == nil {
		var err error
		objs, err = s.b.List(ctx, userContextPrefix)
		if err != nil {
			return out, err
		}
	}
	wanted := idSet(ids)
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		agentID, userID, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(o.Key, userContextPrefix), ".json"), "/")
		if !ok || !wanted[userID] {
			continue
		}
		doc := s.load(ctx, agentID, userID)
		if doc == nil || len(doc.Entries) == 0 {
			continue
		}
		mem := UserContextAgentMemory{Agent: agentID, Key: o.Key, Turns: len(doc.Entries), Bytes: o.Size}
		for _, e := range doc.Entries {
			if mem.Oldest.IsZero() || e.At.Before(mem.Oldest) {
				mem.Oldest = e.At
			}
			if e.At.After(mem.Newest) {
				mem.Newest = e.At
			}
			out.Turns = append(out.Turns, UserContextTurn{
				ID: e.ID, Agent: agentID, Identity: userID, At: e.At,
				Channel: e.Channel, Question: e.Question, Answer: e.Answer,
			})
		}
		out.Bytes += o.Size
		out.Agents = append(out.Agents, mem)
	}
	sort.Slice(out.Agents, func(i, j int) bool { return out.Agents[i].Agent < out.Agents[j].Agent })
	sort.Slice(out.Turns, func(i, j int) bool { return out.Turns[i].At.After(out.Turns[j].At) })
	out.trim()
	out.stamp()
	return out, nil
}

// writeProfile stores prof under every identity of the person and reports how
// many copies it had to write. A copy already holding the same content is left
// alone; one that is missing or behind is written, so the decision is made per
// copy rather than from whichever copy happened to be read first.
func (s *UserContextStore) writeProfile(ctx context.Context, prof UserContextProfile) (int, error) {
	var firstErr error
	written := 0
	for _, id := range prof.Identities {
		if !safeIDRe.MatchString(id) {
			continue
		}
		changed := false
		err := updateDoc(ctx, s.b, profileKey(id), func(d *UserContextProfile) bool {
			if d.Fingerprint != "" && d.Fingerprint == prof.Fingerprint {
				return false
			}
			*d = prof
			changed = true
			return true
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if changed {
			written++
		}
	}
	return written, firstErr
}

// noteProfileTurn folds a turn that was just recorded into the person's cached
// profile, so the aggregate is current within seconds of the turn rather than
// at the next rebuild. The merge happens inside the conditional write, so two
// replicas recording turns for the same person cannot lose one another's.
func (s *UserContextStore) noteProfileTurn(ctx context.Context, agentID, userID string, entry userContextEntry) error {
	ids := s.identities(ctx, []string{userID})
	if len(ids) == 0 {
		return nil
	}
	turn := UserContextTurn{
		ID: entry.ID, Agent: agentID, Identity: UserContextID(userID), At: entry.At,
		Channel: entry.Channel, Question: entry.Question, Answer: entry.Answer,
	}
	uncached := false
	for _, id := range ids {
		err := updateDoc(ctx, s.b, profileKey(id), func(d *UserContextProfile) bool {
			if len(d.Turns) == 0 && len(d.Identities) == 0 {
				uncached = true
				return false
			}
			return d.insert(turn, ids)
		})
		if err != nil {
			return err
		}
	}
	if !uncached {
		return nil
	}
	prof, err := s.aggregate(ctx, ids, nil)
	if err != nil {
		return err
	}
	if len(prof.Turns) == 0 {
		return nil
	}
	_, err = s.writeProfile(ctx, prof)
	return err
}

// groupIdentities folds the identities that belong to the same person into one
// group. A link may only resolve one way — a Slack ID knows its email, an
// email key cannot be turned back into a Slack ID — so the groups are built by
// union rather than by expanding each identity on its own, and a person comes
// out as one group whichever of their identities is seen first.
func (s *UserContextStore) groupIdentities(ctx context.Context, ids []string) [][]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		p, ok := parent[x]
		if !ok {
			parent[x] = x
			return x
		}
		if p != x {
			parent[x] = find(p)
		}
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, id := range ids {
		find(id)
		for _, alias := range s.identities(ctx, []string{id}) {
			union(id, alias)
		}
	}
	keys := make([]string, 0, len(parent))
	for id := range parent {
		keys = append(keys, id)
	}
	byRoot := map[string][]string{}
	for _, id := range keys {
		root := find(id)
		byRoot[root] = append(byRoot[root], id)
	}
	out := make([][]string, 0, len(byRoot))
	for _, group := range byRoot {
		sort.Strings(group)
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// rebuildProfiles re-derives every cached profile from the per-agent documents
// and removes the ones whose documents are gone. It is the self-healing pass
// behind the incremental updates: a profile that was missed, half-written,
// left behind by the TTL sweep, or written before two identities were known to
// belong to the same person is corrected here.
func (s *UserContextStore) rebuildProfiles(ctx context.Context) {
	objs, err := s.b.List(ctx, userContextPrefix)
	if err != nil {
		log.Printf("[user-context] profile rebuild list error: %v", err)
		return
	}
	seen := map[string]bool{}
	sources := make([]string, 0, len(objs))
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		_, userID, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(o.Key, userContextPrefix), ".json"), "/")
		if !ok || seen[userID] || !safeIDRe.MatchString(userID) {
			continue
		}
		seen[userID] = true
		sources = append(sources, userID)
	}
	expected := map[string]bool{}
	people, written := 0, 0
	for _, ids := range s.groupIdentities(ctx, sources) {
		prof, err := s.aggregate(ctx, ids, objs)
		if err != nil {
			log.Printf("[user-context] profile rebuild failed for %d identity(ies): %v", len(ids), err)
			continue
		}
		if len(prof.Turns) == 0 {
			continue
		}
		people++
		for _, id := range prof.Identities {
			expected[profileKey(id)] = true
		}
		n, err := s.writeProfile(ctx, prof)
		if err != nil {
			log.Printf("[user-context] profile write failed: %v", err)
		}
		written += n
	}
	stale, err := s.b.List(ctx, userProfilePrefix)
	if err != nil {
		log.Printf("[user-context] profile sweep list error: %v", err)
		return
	}
	removed := 0
	for _, o := range stale {
		if expected[o.Key] || !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		if err := s.b.Delete(ctx, o.Key, ""); err != nil {
			log.Printf("[user-context] profile delete %s: %v", o.Key, err)
			continue
		}
		removed++
	}
	if written > 0 || removed > 0 {
		log.Printf("[user-context] profiles: %d person(s), %d copy(ies) written, %d stale removed", people, written, removed)
	}
}

// QueueTopic is the deferred-work topic a completed turn's memory is written
// through.
const QueueTopic = "user-context"

// queueAppendTimeout bounds one deferred write, document and vector together.
const queueAppendTimeout = 2 * time.Minute

// appendTask is the queued form of one completed turn. The entry ID travels
// with it so a redelivered task recognises the write it already made instead
// of appending the turn twice.
type appendTask struct {
	Agent   string    `json:"agent"`
	User    string    `json:"user"`
	Entry   string    `json:"entry"`
	At      time.Time `json:"at"`
	Channel string    `json:"channel,omitempty"`
	Q       string    `json:"q,omitempty"`
	A       string    `json:"a,omitempty"`
}

// UseQueue defers the write of a completed turn to the durable queue, so the
// reply a requester is waiting for is not held behind a conditional write and
// an embedding call. Nothing reads this document again until the next turn, so
// the delay costs nothing; what it buys is that the write survives the replica
// that produced it.
func (s *UserContextStore) UseQueue(q *queue.Queue) {
	if s == nil || q == nil {
		return
	}
	s.tasks.Store(q)
	q.Register(QueueTopic, queue.Options{Timeout: queueAppendTimeout, MaxAttempts: 4}, s.runAppendTask)
}

func (s *UserContextStore) runAppendTask(ctx context.Context, payload []byte) error {
	var t appendTask
	if err := json.Unmarshal(payload, &t); err != nil {
		return err
	}
	entry := userContextEntry{ID: t.Entry, At: t.At, Channel: t.Channel, Question: t.Q, Answer: t.A}
	dropped, err := s.appendEntry(ctx, t.Agent, t.User, entry)
	if err != nil {
		return err
	}
	if err := s.indexEntry(ctx, t.Agent, t.User, entry, dropped); err != nil {
		return err
	}
	// The profile is derived state the hourly rebuild repairs, so a failure
	// here is logged rather than retried with the whole task.
	if err := s.noteProfileTurn(ctx, t.Agent, t.User, entry); err != nil {
		log.Printf("[user-context] profile update failed agent=%s user=%s: %v", t.Agent, t.User, err)
	}
	return nil
}

// Append records a completed (question, answer) turn; a turn with no answer is
// not memory and is dropped. With a queue attached the write is handed off and
// this returns immediately; without one it falls back to writing the document
// inline and indexing in the background.
func (s *UserContextStore) Append(ctx context.Context, agentID, userID, channelID, question, answer string) {
	if s == nil {
		return
	}
	userID = UserContextID(userID)
	if s.key(agentID, userID) == "" {
		return
	}
	question = text.Truncate(strings.TrimSpace(question), userContextMaxQuestionLen)
	answer = text.Truncate(strings.TrimSpace(answer), userContextMaxAnswerLen)
	if answer == "" {
		return
	}
	id, err := store.NewID()
	if err != nil {
		log.Printf("[user-context] id generation failed: %v", err)
		return
	}
	entry := userContextEntry{ID: id, At: time.Now().UTC(), Channel: channelID, Question: question, Answer: answer}

	if q := s.tasks.Load(); q != nil {
		task := appendTask{Agent: agentID, User: userID, Entry: entry.ID, At: entry.At, Channel: channelID, Q: question, A: answer}
		err := q.Enqueue(ctx, QueueTopic, task)
		if err == nil {
			return
		}
		log.Printf("[user-context] enqueue failed, writing inline: %v", err)
	}

	dropped, err := s.appendEntry(ctx, agentID, userID, entry)
	if err != nil {
		log.Printf("[user-context] write failed agent=%s user=%s: %v", agentID, userID, err)
		return
	}
	safego.Go("user context: index", func() {
		ictx, cancel := context.WithTimeout(context.Background(), userContextIndexTimeout)
		defer cancel()
		if err := s.indexEntry(ictx, agentID, userID, entry, dropped); err != nil {
			log.Printf("[user-context] index failed agent=%s user=%s: %v", agentID, userID, err)
		}
		if err := s.noteProfileTurn(ictx, agentID, userID, entry); err != nil {
			log.Printf("[user-context] profile update failed agent=%s user=%s: %v", agentID, userID, err)
		}
	})
}

// appendEntry adds the entry to the user's document and trims the oldest ones
// back under the caps, returning the IDs it dropped. An entry already present
// is left alone, so a redelivered task is a no-op rather than a duplicate.
func (s *UserContextStore) appendEntry(ctx context.Context, agentID, userID string, entry userContextEntry) ([]string, error) {
	key := s.key(agentID, userID)
	if key == "" {
		return nil, fmt.Errorf("invalid agent %q or user %q", agentID, userID)
	}
	maxEntries, maxBytes := userContextMaxEntries, userContextMaxDocBytes
	if s.index.Load() != nil {
		maxEntries, maxBytes = userContextMaxEntriesIndexed, userContextMaxDocBytesIndexed
	}
	var dropped []string
	err := updateDoc(ctx, s.b, key, func(d *userContextDoc) bool {
		dropped = dropped[:0]
		for _, e := range d.Entries {
			if e.ID == entry.ID {
				return false
			}
		}
		d.Entries = append(d.Entries, entry)
		for len(d.Entries) > 1 && (len(d.Entries) > maxEntries || d.size() > maxBytes) {
			dropped = append(dropped, d.Entries[0].ID)
			d.Entries = d.Entries[1:]
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return dropped, nil
}

// indexEntry upserts the entry's vector and removes the vectors of the entries
// the document dropped. A no-op when no vector index is attached.
func (s *UserContextStore) indexEntry(ctx context.Context, agentID, userID string, entry userContextEntry, dropped []string) error {
	index := s.index.Load()
	if index == nil {
		return nil
	}
	if err := index.Upsert(ctx, []vectors.Item{vectorItem(agentID, userID, entry)}); err != nil {
		return err
	}
	if len(dropped) == 0 {
		return nil
	}
	keys := make([]string, 0, len(dropped))
	for _, id := range dropped {
		keys = append(keys, vectorKeyPrefix(agentID, userID)+id)
	}
	return index.Delete(ctx, keys)
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

// updateDoc applies fn to the latest version of the document at key and writes
// it back when fn reports a change, creating the document when it does not
// exist and retrying when another replica wrote in between. fn runs again on
// each retry, so a merge written inside it cannot lose the other write.
func updateDoc[T any](ctx context.Context, b *store.Backend, key string, fn func(*T) bool) error {
	for attempt := 0; attempt < 4; attempt++ {
		doc, tag, err := store.GetJSON[T](ctx, b, key)
		cond := store.Condition{IfMatch: tag}
		switch {
		case errors.Is(err, store.ErrNotFound):
			doc = new(T)
			cond = store.Condition{IfNoneMatch: true}
		case err != nil:
			return err
		}
		if !fn(doc) {
			return nil
		}
		if _, err := store.PutJSON(ctx, b, key, doc, cond); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return store.ErrConflict
}

// StartGC runs the three background passes over the user context, once at
// start and then every interval until ctx ends: it removes the documents (and
// vectors) of users inactive for longer than UserContextTTL, repairs the index
// against the surviving documents, and rebuilds the aggregated per-person
// profiles from them. They run in that order so each works on what the one
// before it left. One replica should run this.
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
		safego.Run("user context: profiles", func() { s.rebuildProfiles(ctx) })
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
