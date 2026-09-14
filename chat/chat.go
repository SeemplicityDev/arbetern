// Package chat implements a lightweight, centralized chat interface for
// agents. When an agent enables chat (`chat_enabled: true` in its
// config.yaml), the management UI lets anyone converse with that agent's LLM
// persona.
//
// Like widely used assistant apps (ChatGPT, Claude), each agent can hold many
// independent conversations: a left-bar history of titled threads plus a "New
// chat" action. Conversations are stored in the state bucket so they survive
// restarts, and each one records the identity that created it. Every read and
// write is scoped to that owner, so one viewer never sees another viewer's
// threads. The owner is the proxy-verified email when an upstream OAuth proxy
// is in front; a deployment with no proxy has no identity to scope by, and
// there every caller shares the one ownerless set.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/progress"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
)

// Prefix is the object prefix transcripts are stored under.
const Prefix = "chat/"

// maxMessages caps the stored transcript length per conversation so the
// document (and the LLM context we replay) cannot grow without bound.
const maxMessages = 500

// historyContextLimit bounds how many prior messages are replayed to the LLM
// as conversation context on each turn.
const historyContextLimit = 30

// turnTimeout bounds one background reply, tool loop included.
const turnTimeout = 30 * time.Minute

// pendingFlushEvery is how often in-flight progress is written to the transcript.
const pendingFlushEvery = 5 * time.Second

const finishTimeout = 30 * time.Second

// ErrNotFound is returned when a conversation does not exist.
var ErrNotFound = errors.New("conversation not found")

// ErrBusy is returned when a reply is already being produced for the conversation.
var ErrBusy = errors.New("a reply is already in progress")

// ErrForbidden is returned when a conversation exists but belongs to someone
// else. Handlers translate it to 404 so the response does not confirm that the
// conversation is real.
var ErrForbidden = errors.New("conversation belongs to another user")

var errNoChange = errors.New("no change")

// Message is a single chat turn. Role is "user" or "assistant". Error marks an
// assistant turn that reports a failure instead of an answer.
type Message struct {
	Role    string    `json:"role"`
	User    string    `json:"user,omitempty"`
	Content string    `json:"content"`
	Time    time.Time `json:"time"`
	Error   bool      `json:"error,omitempty"`
}

// Responder produces an assistant reply for an agent given the prior
// transcript (most recent last) and the new user message. user is the
// resolved sender identity (the OAuth-proxy-verified email when a proxy is in
// front, else any client-supplied name), or "" when unknown. It is implemented
// in main using the shared LLM client and the agent's system prompt.
type Responder func(ctx context.Context, agent, user string, history []Message, userMessage string, tracker *progress.Tracker) (string, error)

// transcript is the stored shape of a single conversation, kept at
// <Prefix><agent>/<id>.json.
type transcript struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	// Owner is the normalized identity that created the conversation and the
	// only one allowed to read or change it. Empty means the conversation was
	// created without an identity source, and only an equally unidentified
	// caller can reach it.
	Owner     string    `json:"owner,omitempty"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  []Message `json:"messages"`
	// Pending is set while a reply is being produced.
	Pending *progress.Snapshot `json:"pending,omitempty"`
}

// ConversationSummary is the lightweight shape returned when listing an
// agent's conversations for the history sidebar.
type ConversationSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
}

// Registry serves per-agent chat transcripts from the state bucket and tracks
// which agents have chat enabled. All operations are safe for concurrent use.
type Registry struct {
	docs    *store.Documents[transcript]
	respond Responder

	mu          sync.Mutex
	enabled     map[string]bool
	authorize   func(req *http.Request, agent string) bool
	resolveUser func(req *http.Request) string
}

// New constructs a Registry over b. respond is invoked to generate assistant
// replies. Call Load before serving.
func New(b *store.Backend, respond Responder) *Registry {
	return &Registry{docs: store.NewDocuments[transcript](b, Prefix, store.AgentKeyRe, nil), respond: respond, enabled: make(map[string]bool)}
}

// Load reads every stored conversation into the cache.
func (r *Registry) Load(ctx context.Context) error {
	if err := r.docs.Load(ctx); err != nil {
		return fmt.Errorf("load chat transcripts: %w", err)
	}
	return nil
}

// StartRefresh picks up conversations written by other replicas every interval.
func (r *Registry) StartRefresh(ctx context.Context, interval time.Duration) {
	r.docs.StartRefresh(ctx, interval, nil)
}

// Count is the number of stored conversations.
func (r *Registry) Count() int { return r.docs.Len() }

// SetEnabled records whether chat is enabled for an agent.
func (r *Registry) SetEnabled(agent string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled[agent] = enabled
}

// IsEnabled reports whether chat is enabled for an agent.
func (r *Registry) IsEnabled(agent string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled[agent]
}

// SetAuthorizer installs an optional per-request access check. When set, it is
// consulted after the agent is resolved and chat is confirmed enabled; a false
// result rejects the request with 403. A nil authorizer (the default) allows
// every authenticated request.
func (r *Registry) SetAuthorizer(fn func(req *http.Request, agent string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorize = fn
}

// authorized applies the configured authorizer (if any) for the given request.
func (r *Registry) authorized(req *http.Request, agent string) bool {
	r.mu.Lock()
	fn := r.authorize
	r.mu.Unlock()
	if fn == nil {
		return true
	}
	return fn(req, agent)
}

// SetUserResolver installs an optional function that derives the sender's
// display identity from the request — typically the email an upstream OAuth
// proxy injects after a successful login. When it returns a non-empty value,
// that identity is recorded as the message author in place of any
// client-supplied name. A nil resolver (the default) falls back to the optional
// free-text name in the request body.
func (r *Registry) SetUserResolver(fn func(req *http.Request) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveUser = fn
}

// userFor returns the resolved sender identity for a request, or "" when no
// resolver is configured or it yields nothing (e.g. local dev with no proxy).
func (r *Registry) userFor(req *http.Request) string {
	r.mu.Lock()
	fn := r.resolveUser
	r.mu.Unlock()
	if fn == nil {
		return ""
	}
	return fn(req)
}

// OwnerKey normalizes a caller identity into the form stored on a transcript.
func OwnerKey(user string) string { return strings.ToLower(strings.TrimSpace(user)) }

// owns reports whether a caller identity may reach this conversation.
func owns(t *transcript, owner string) bool { return t.Owner == owner }

// splitKey returns the agent and conversation id encoded in a document key.
func splitKey(key string) (agent, id string) {
	agent, rest, _ := strings.Cut(key, "/")
	return agent, strings.TrimSuffix(rest, ".json")
}

// ListConversations returns summaries of an agent's conversations, most
// recently updated first. Returns an empty slice when the agent has none.
func (r *Registry) ListConversations(agent, owner string) ([]ConversationSummary, error) {
	if !store.AgentRe.MatchString(agent) {
		return nil, fmt.Errorf("invalid agent %q", agent)
	}
	out := []ConversationSummary{}
	r.docs.Range(func(key string, t *transcript) {
		a, id := splitKey(key)
		if a != agent {
			return
		}
		normalize(t, a, id)
		if !owns(t, owner) {
			return
		}
		out = append(out, ConversationSummary{
			ID:           t.ID,
			Title:        t.Title,
			CreatedAt:    t.CreatedAt,
			UpdatedAt:    t.UpdatedAt,
			MessageCount: len(t.Messages),
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// CreateConversation starts a new, empty conversation for an agent and stores
// it so it appears in the history sidebar immediately.
func (r *Registry) CreateConversation(ctx context.Context, agent, owner string) (*transcript, error) {
	id, err := store.NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t := &transcript{ID: id, Agent: agent, Owner: owner, Title: "New chat", CreatedAt: now, UpdatedAt: now, Messages: []Message{}}
	if err := r.docs.Create(ctx, store.Key(agent, id), t); err != nil {
		return nil, err
	}
	return t, nil
}

// Conversation returns a single conversation (nil when it does not exist).
func (r *Registry) Conversation(agent, id, owner string) (*transcript, error) {
	t, ok := r.docs.Get(store.Key(agent, id))
	if !ok {
		return nil, nil
	}
	normalize(t, agent, id)
	if !owns(t, owner) {
		return nil, ErrForbidden
	}
	if stalePending(t.Pending) {
		t.Pending = nil
	}
	return t, nil
}

func stalePending(p *progress.Snapshot) bool {
	return p != nil && time.Since(p.StartedAt) > turnTimeout+time.Minute
}

// DeleteConversation removes a conversation. Deleting a missing conversation is
// a no-op.
func (r *Registry) DeleteConversation(ctx context.Context, agent, id, owner string) error {
	key := store.Key(agent, id)
	if t, ok := r.docs.Get(key); ok {
		normalize(t, agent, id)
		if !owns(t, owner) {
			return ErrForbidden
		}
	}
	return r.docs.Delete(ctx, key)
}

// RenameConversation sets a conversation's title. Returns nil when the
// conversation does not exist.
func (r *Registry) RenameConversation(ctx context.Context, agent, id, owner, title string) (*transcript, error) {
	t, err := r.docs.Update(ctx, store.Key(agent, id), func(t *transcript) error {
		normalize(t, agent, id)
		if !owns(t, owner) {
			return ErrForbidden
		}
		t.Title = title
		t.UpdatedAt = time.Now().UTC()
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return t, err
}

// StartRetention launches a background sweeper that deletes conversations whose
// last activity is older than retention, across all agents. It runs once
// immediately and then every interval until ctx is cancelled. A non-positive
// retention disables the sweeper.
func (r *Registry) StartRetention(ctx context.Context, retention, interval time.Duration) {
	if retention <= 0 {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	purge := func() {
		safego.Run("chat: purge expired", func() { r.purgeExpired(ctx, retention) })
	}
	safego.Go("chat: purge loop", func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		purge()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				purge()
			}
		}
	})
}

// purgeExpired deletes every conversation (for every agent) whose UpdatedAt is
// older than now-retention. Errors on individual documents are logged and
// skipped so one bad document cannot stall the sweep.
func (r *Registry) purgeExpired(ctx context.Context, retention time.Duration) {
	cutoff := time.Now().UTC().Add(-retention)
	var expired []string
	r.docs.Range(func(key string, t *transcript) {
		a, id := splitKey(key)
		normalize(t, a, id)
		if t.UpdatedAt.Before(cutoff) {
			expired = append(expired, key)
		}
	})
	removed := 0
	for _, key := range expired {
		if err := r.docs.Delete(ctx, key); err != nil {
			log.Printf("chat retention: delete %s: %v", key, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Printf("chat retention: removed %d conversation(s) inactive for >%s", removed, retention)
	}
}

// normalize fills in any legacy or partial record (e.g. the
// pre-multi-conversation single history.json) so callers always receive a
// complete transcript.
func normalize(t *transcript, agent, id string) {
	if t.ID == "" {
		t.ID = id
	}
	if t.Agent == "" {
		t.Agent = agent
	}
	if t.Title == "" {
		t.Title = titleFromMessages(t.Messages)
	}
	if t.CreatedAt.IsZero() {
		if len(t.Messages) > 0 {
			t.CreatedAt = t.Messages[0].Time
		} else {
			t.CreatedAt = time.Now().UTC()
		}
	}
	if t.UpdatedAt.IsZero() {
		if n := len(t.Messages); n > 0 {
			t.UpdatedAt = t.Messages[n-1].Time
		} else {
			t.UpdatedAt = t.CreatedAt
		}
	}
	if t.Messages == nil {
		t.Messages = []Message{}
	}
}

func trimMessages(t *transcript) {
	if len(t.Messages) > maxMessages {
		t.Messages = t.Messages[len(t.Messages)-maxMessages:]
	}
}

// Start appends the user's message, marks the conversation as answering, and
// produces the reply in the background so the HTTP request never waits on the
// tool loop. The reply, or the failure, is appended when the turn ends. Returns
// ErrBusy while an earlier turn is still in flight and ErrNotFound when the
// conversation does not exist.
func (r *Registry) Start(agent, id, owner, user, message string) error {
	if r.respond == nil {
		return fmt.Errorf("chat responder not configured")
	}
	key := store.Key(agent, id)
	tracker := progress.NewTracker()
	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	var contextMsgs []Message
	_, err := r.docs.Update(ctx, key, func(t *transcript) error {
		normalize(t, agent, id)
		if !owns(t, owner) {
			return ErrForbidden
		}
		if t.Pending != nil && !stalePending(t.Pending) {
			return ErrBusy
		}
		contextMsgs = trimContext(t.Messages)
		now := time.Now().UTC()
		t.Messages = append(t.Messages, Message{Role: "user", User: user, Content: message, Time: now})
		if t.Title == "" || t.Title == "New chat" {
			t.Title = deriveTitle(message)
		}
		t.UpdatedAt = now
		snap := tracker.Snapshot()
		t.Pending = &snap
		trimMessages(t)
		return nil
	})
	if err != nil {
		cancel()
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	safego.Go("chat: turn "+key, func() {
		defer cancel()
		stop := make(chan struct{})
		tracker.Watch(stop, pendingFlushEvery, pendingFlushEvery, func(s progress.Snapshot) { r.flushPending(ctx, key, s) })
		reply, err := r.respond(ctx, agent, user, contextMsgs, message, tracker)
		close(stop)
		msg := Message{Role: "assistant", Content: reply, Time: time.Now().UTC()}
		if err != nil {
			msg.Content, msg.Error = "Error: "+err.Error(), true
		}
		fctx, fcancel := context.WithTimeout(context.Background(), finishTimeout)
		defer fcancel()
		_, err = r.docs.Update(fctx, key, func(t *transcript) error {
			normalize(t, agent, id)
			t.Pending = nil
			t.Messages = append(t.Messages, msg)
			t.UpdatedAt = msg.Time
			trimMessages(t)
			return nil
		})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("chat: failed to store reply for %s: %v", key, err)
		}
	})
	return nil
}

func (r *Registry) flushPending(ctx context.Context, key string, s progress.Snapshot) {
	_, err := r.docs.Update(ctx, key, func(t *transcript) error {
		if t.Pending == nil || (t.Pending.ToolCalls == s.ToolCalls && t.Pending.LastTool == s.LastTool) {
			return errNoChange
		}
		t.Pending = &s
		return nil
	})
	if err != nil && !errors.Is(err, errNoChange) && !errors.Is(err, store.ErrNotFound) {
		log.Printf("chat: failed to store progress for %s: %v", key, err)
	}
}

// trimContext returns the most recent historyContextLimit messages that were
// real turns, leaving out stored failure notices.
func trimContext(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if !m.Error {
			out = append(out, m)
		}
	}
	if len(out) > historyContextLimit {
		out = out[len(out)-historyContextLimit:]
	}
	return out
}

// titleFromMessages derives a title from the first non-empty user message,
// falling back to "New chat".
func titleFromMessages(msgs []Message) string {
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			return deriveTitle(m.Content)
		}
	}
	return "New chat"
}

// deriveTitle builds a short conversation title from a message: its first line,
// trimmed and capped, like the auto-titles in ChatGPT/Claude.
func deriveTitle(message string) string {
	s := strings.TrimSpace(message)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "New chat"
	}
	const max = 60
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}
