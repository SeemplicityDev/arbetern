// Package chat implements a lightweight, centralized chat interface for
// agents. When an agent enables chat (`chat_enabled: true` in its
// config.yaml), the management UI lets anyone converse with that agent's LLM
// persona.
//
// Like widely used assistant apps (ChatGPT, Claude), each agent can hold many
// independent conversations: a left-bar history of titled threads plus a "New
// chat" action. There is no per-user authentication in arbetern yet, so the
// conversations are deliberately *centralized* — every viewer shares the same
// list of threads, and they are persisted to disk so they survive restarts.
// Each turn records an optional free-text display name purely for context — it
// is not an identity and grants no access.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/store"
)

// DefaultDir is used when CHAT_DIR is unset.
const DefaultDir = "./data/chat"

// maxMessages caps the persisted transcript length per conversation so the
// history file (and the LLM context we replay) cannot grow without bound.
const maxMessages = 500

// historyContextLimit bounds how many prior messages are replayed to the LLM
// as conversation context on each turn.
const historyContextLimit = 30

// ErrNotFound is returned when a conversation does not exist.
var ErrNotFound = errors.New("conversation not found")

// Message is a single chat turn. Role is "user" or "assistant".
type Message struct {
	Role    string    `json:"role"`
	User    string    `json:"user,omitempty"`
	Content string    `json:"content"`
	Time    time.Time `json:"time"`
}

// Responder produces an assistant reply for an agent given the prior
// transcript (most recent last) and the new user message. It is implemented in
// main using the shared LLM client and the agent's system prompt.
type Responder func(ctx context.Context, agent string, history []Message, userMessage string) (string, error)

// transcript is the on-disk shape of a single conversation. Each agent may
// have many, stored at <root>/<agent>/<id>.json.
type transcript struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  []Message `json:"messages"`
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

// Registry persists per-agent chat transcripts on disk and tracks which agents
// have chat enabled. All operations are safe for concurrent use.
type Registry struct {
	root    string
	respond Responder

	mu        sync.Mutex
	enabled   map[string]bool
	authorize func(req *http.Request, agent string) bool
}

// New constructs a Registry rooted at dir (falling back to DefaultDir when
// empty). respond is invoked to generate assistant replies.
func New(dir string, respond Responder) *Registry {
	if dir == "" {
		dir = DefaultDir
	}
	return &Registry{root: dir, respond: respond, enabled: make(map[string]bool)}
}

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

// ListConversations returns summaries of an agent's conversations, most
// recently updated first. Returns an empty slice when the agent has none.
func (r *Registry) ListConversations(agent string) ([]ConversationSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := os.ReadDir(store.AgentDir(r.root, agent))
	if err != nil {
		if os.IsNotExist(err) {
			return []ConversationSummary{}, nil
		}
		return nil, err
	}
	out := make([]ConversationSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		t, err := r.readLocked(agent, id)
		if err != nil || t == nil {
			continue
		}
		out = append(out, ConversationSummary{
			ID:           t.ID,
			Title:        t.Title,
			CreatedAt:    t.CreatedAt,
			UpdatedAt:    t.UpdatedAt,
			MessageCount: len(t.Messages),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// CreateConversation starts a new, empty conversation for an agent and
// persists it so it appears in the history sidebar immediately.
func (r *Registry) CreateConversation(agent string) (*transcript, error) {
	id, err := store.NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t := &transcript{ID: id, Agent: agent, Title: "New chat", CreatedAt: now, UpdatedAt: now, Messages: []Message{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.writeLocked(t); err != nil {
		return nil, err
	}
	return t, nil
}

// Conversation returns a single conversation (nil when it does not exist).
func (r *Registry) Conversation(agent, id string) (*transcript, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(agent, id)
}

// DeleteConversation removes a conversation. Deleting a missing conversation is
// a no-op.
func (r *Registry) DeleteConversation(agent, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := os.Remove(store.PathFor(r.root, agent, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RenameConversation sets a conversation's title. Returns nil when the
// conversation does not exist.
func (r *Registry) RenameConversation(agent, id, title string) (*transcript, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.readLocked(agent, id)
	if err != nil || t == nil {
		return nil, err
	}
	t.Title = title
	t.UpdatedAt = time.Now().UTC()
	if err := r.writeLocked(t); err != nil {
		return nil, err
	}
	return t, nil
}

// StartRetention launches a background sweeper that deletes conversations whose
// last activity is older than retention, across all agents. It runs once
// immediately and then every interval until ctx is cancelled. A non-positive
// retention disables the sweeper. Safe to call once at startup.
func (r *Registry) StartRetention(ctx context.Context, retention, interval time.Duration) {
	if retention <= 0 {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		r.purgeExpired(retention)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.purgeExpired(retention)
			}
		}
	}()
}

// purgeExpired deletes every conversation (for every agent) whose UpdatedAt is
// older than now-retention. Errors on individual files are logged and skipped
// so one bad file cannot stall the sweep.
func (r *Registry) purgeExpired(retention time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().UTC().Add(-retention)
	agents, err := os.ReadDir(r.root)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("chat retention: read root %q: %v", r.root, err)
		}
		return
	}
	removed := 0
	for _, ad := range agents {
		if !ad.IsDir() {
			continue
		}
		agent := ad.Name()
		entries, err := os.ReadDir(store.AgentDir(r.root, agent))
		if err != nil {
			log.Printf("chat retention: read agent dir %q: %v", agent, err)
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			t, err := r.readLocked(agent, id)
			if err != nil || t == nil {
				continue
			}
			if t.UpdatedAt.Before(cutoff) {
				if err := os.Remove(store.PathFor(r.root, agent, id)); err != nil && !os.IsNotExist(err) {
					log.Printf("chat retention: delete %s/%s: %v", agent, id, err)
					continue
				}
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("chat retention: removed %d conversation(s) inactive for >%s", removed, retention)
	}
}

// readLocked reads one conversation from disk, normalizing any legacy or
// partial record (e.g. the pre-multi-conversation single history.json) so
// callers always receive a complete transcript. Callers must hold r.mu.
func (r *Registry) readLocked(agent, id string) (*transcript, error) {
	t, err := store.ReadJSON[transcript](store.PathFor(r.root, agent, id), nil)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
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
	return t, nil
}

// writeLocked persists a conversation, trimming to maxMessages. Callers must
// hold r.mu.
func (r *Registry) writeLocked(t *transcript) error {
	if len(t.Messages) > maxMessages {
		t.Messages = t.Messages[len(t.Messages)-maxMessages:]
	}
	return store.WriteJSON(r.root, t.Agent, t.ID, t)
}

// Post appends the user's message to the given conversation, asks the responder
// for a reply, appends the reply, persists, and returns the assistant message.
// The user's turn is persisted before the (slow) LLM call so it is never lost.
// Returns ErrNotFound when the conversation does not exist.
func (r *Registry) Post(ctx context.Context, agent, id, user, message string) (Message, error) {
	if r.respond == nil {
		return Message{}, fmt.Errorf("chat responder not configured")
	}

	r.mu.Lock()
	t, err := r.readLocked(agent, id)
	if err != nil {
		r.mu.Unlock()
		return Message{}, err
	}
	if t == nil {
		r.mu.Unlock()
		return Message{}, ErrNotFound
	}
	contextMsgs := trimContext(t.Messages)
	now := time.Now().UTC()
	t.Messages = append(t.Messages, Message{Role: "user", User: user, Content: message, Time: now})
	if t.Title == "" || t.Title == "New chat" {
		t.Title = deriveTitle(message)
	}
	t.UpdatedAt = now
	if err := r.writeLocked(t); err != nil {
		r.mu.Unlock()
		return Message{}, err
	}
	r.mu.Unlock()

	reply, err := r.respond(ctx, agent, contextMsgs, message)
	if err != nil {
		return Message{}, err
	}
	assistantMsg := Message{Role: "assistant", Content: reply, Time: time.Now().UTC()}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-read so a concurrent Post's turns are not clobbered.
	current, err := r.readLocked(agent, id)
	if err != nil {
		return Message{}, err
	}
	if current == nil {
		return Message{}, ErrNotFound
	}
	current.Messages = append(current.Messages, assistantMsg)
	current.UpdatedAt = assistantMsg.Time
	if err := r.writeLocked(current); err != nil {
		return Message{}, err
	}
	return assistantMsg, nil
}

// trimContext returns the most recent historyContextLimit messages, copied
// into a fresh slice so callers cannot mutate the registry's data.
func trimContext(msgs []Message) []Message {
	if len(msgs) > historyContextLimit {
		msgs = msgs[len(msgs)-historyContextLimit:]
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
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
