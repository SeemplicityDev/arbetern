package commands

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// DefaultSessionTTL is used when no custom TTL is provided.
const DefaultSessionTTL = 3 * time.Minute

// ThreadSession represents an active conversational bridge for a specific
// Slack thread. It is created when a /command posts an audit message and
// remains alive for TTL, refreshed on every interaction.
type ThreadSession struct {
	ChannelID string
	ThreadTS  string
	UserID    string
	AgentID   string
	Router    *Router
	CreatedAt time.Time
	LastSeen  time.Time

	mu    sync.Mutex
	timer *time.Timer
}

// SessionStore tracks active thread sessions. Safe for concurrent use.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*ThreadSession // key: "channelID:threadTS"
	ttl      time.Duration

	// Observability counters
	counterMu     sync.Mutex
	totalOpened   int64
	totalExpired  int64
	totalExplicit int64 // closed explicitly (errors, etc.)
}

// NewSessionStore creates a store with the given default TTL per session.
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionStore{
		sessions: make(map[string]*ThreadSession),
		ttl:      ttl,
	}
}

func sessionKey(channelID, threadTS string) string {
	return channelID + ":" + threadTS
}

// TTL returns the configured session time-to-live.
func (s *SessionStore) TTL() time.Duration {
	return s.ttl
}

// Open creates (or re-opens) a session for the given thread.
// If a session already exists, its TTL is refreshed.
func (s *SessionStore) Open(channelID, threadTS, userID, agentID string, router *Router) {
	key := sessionKey(channelID, threadTS)

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.sessions[key]; ok {
		existing.refresh(s.ttl)
		log.Printf("[session] refreshed channel=%s thread=%s user=%s agent=%s ttl=%s",
			channelID, threadTS, userID, agentID, s.ttl)
		return
	}

	sess := &ThreadSession{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		UserID:    userID,
		AgentID:   agentID,
		Router:    router,
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}

	sess.timer = time.AfterFunc(s.ttl, func() {
		s.expire(key, sess)
	})

	s.sessions[key] = sess

	s.counterMu.Lock()
	s.totalOpened++
	s.counterMu.Unlock()

	log.Printf("[session] opened channel=%s thread=%s user=%s agent=%s ttl=%s",
		channelID, threadTS, userID, agentID, s.ttl)
}

// Lookup returns the session for a thread, or nil if none / expired.
// If found, the TTL is refreshed automatically.
func (s *SessionStore) Lookup(channelID, threadTS string) *ThreadSession {
	key := sessionKey(channelID, threadTS)

	s.mu.RLock()
	sess, ok := s.sessions[key]
	s.mu.RUnlock()

	if !ok {
		return nil
	}

	sess.refresh(s.ttl)
	return sess
}

// Close explicitly removes a session (e.g., on error).
func (s *SessionStore) Close(channelID, threadTS, reason string) {
	key := sessionKey(channelID, threadTS)

	s.mu.Lock()
	sess, ok := s.sessions[key]
	if ok {
		sess.mu.Lock()
		sess.timer.Stop()
		sess.mu.Unlock()
		delete(s.sessions, key)
	}
	s.mu.Unlock()

	if ok {
		duration := time.Since(sess.CreatedAt).Round(time.Millisecond)
		s.counterMu.Lock()
		s.totalExplicit++
		s.counterMu.Unlock()
		log.Printf("[session] closed channel=%s thread=%s reason=%q duration=%s",
			channelID, threadTS, reason, duration)
	}
}

// ActiveCount returns the number of currently active sessions.
func (s *SessionStore) ActiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// Stats returns basic observability counters.
func (s *SessionStore) Stats() (active int, opened, expired, explicit int64) {
	s.mu.RLock()
	active = len(s.sessions)
	s.mu.RUnlock()

	s.counterMu.Lock()
	opened = s.totalOpened
	expired = s.totalExpired
	explicit = s.totalExplicit
	s.counterMu.Unlock()
	return
}

// expire is the callback fired when a session's TTL timer triggers.
func (s *SessionStore) expire(key string, sess *ThreadSession) {
	s.mu.Lock()
	// Only delete if the map entry still points to the same session object
	// (guards against a race with Close or re-Open).
	if current, ok := s.sessions[key]; ok && current == sess {
		delete(s.sessions, key)
	}
	s.mu.Unlock()

	duration := time.Since(sess.CreatedAt).Round(time.Millisecond)

	s.counterMu.Lock()
	s.totalExpired++
	s.counterMu.Unlock()

	log.Printf("[session] expired channel=%s thread=%s user=%s agent=%s duration=%s",
		sess.ChannelID, sess.ThreadTS, sess.UserID, sess.AgentID, duration)

	// Notify the user in the thread that the session has expired and how to continue.
	if sess.Router != nil && sess.Router.slackClient != nil {
		msg := fmt.Sprintf(
			"_:hourglass: Thread session expired after %d min of inactivity._\n"+
				"To continue this conversation, copy the link to this thread and paste it in a new `/%s` message — the bot will pick up the context automatically.\n"+
				"_(Right-click the thread timestamp → Copy link)_",
			int(s.ttl.Minutes()), sess.AgentID,
		)
		if err := sess.Router.slackClient.PostThreadReply(sess.ChannelID, sess.ThreadTS, msg); err != nil {
			log.Printf("[session] failed to post expiry notice channel=%s thread=%s: %v",
				sess.ChannelID, sess.ThreadTS, err)
		}
	}
}

// refresh resets the session timer and updates LastSeen.
func (sess *ThreadSession) refresh(ttl time.Duration) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.timer.Reset(ttl)
	sess.LastSeen = time.Now()
}
