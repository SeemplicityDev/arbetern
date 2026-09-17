package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/journal"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
)

// DefaultSessionTTL is used when no custom TTL is provided. Kept in sync with
// config.defaultThreadSessionTTL.
const DefaultSessionTTL = 7 * time.Minute

const (
	sessionPrefix       = "sessions/"
	sessionStatsKey     = sessionPrefix + "_stats.json"
	sessionLockPrefix   = "locks/sessions/"
	processingLeaseTTL  = 2 * time.Minute
	sessionCacheFresh   = 3 * time.Second
	sessionMissTTL      = 10 * time.Second
	sessionStatsFresh   = 10 * time.Second
	sessionTouchEvery   = 5 * time.Second
	sessionPersistLimit = 15 * time.Second
	sessionKeepAlive    = time.Minute
)

var sessionSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ActiveBranchInfo holds metadata about a branch+PR created during a session.
type ActiveBranchInfo struct {
	BranchName string `json:"branch"`
	BaseBranch string `json:"base"`
	PrURL      string `json:"pr_url"`
}

type sessionRecord struct {
	ChannelID      string                       `json:"channel_id"`
	ThreadTS       string                       `json:"thread_ts"`
	UserID         string                       `json:"user_id"`
	AgentID        string                       `json:"agent_id"`
	CreatedAt      time.Time                    `json:"created_at"`
	LastSeen       time.Time                    `json:"last_seen"`
	Expires        time.Time                    `json:"expires"`
	ActiveBranches map[string]*ActiveBranchInfo `json:"active_branches,omitempty"`
}

type sessionStats struct {
	Opened  int64 `json:"opened"`
	Expired int64 `json:"expired"`
	Closed  int64 `json:"closed"`
}

// ThreadSession is the conversational bridge for one Slack thread. It is
// stored in the bucket so a follow-up may be served by any replica, and the
// processing lock is a lease so replicas never answer the same message twice.
type ThreadSession struct {
	ChannelID string
	ThreadTS  string
	UserID    string
	AgentID   string
	Router    *Router
	CreatedAt time.Time
	LastSeen  time.Time

	mu             sync.Mutex
	store          *SessionStore
	expires        time.Time
	checkedAt      time.Time
	touchedAt      time.Time
	processing     bool
	closed         bool
	release        func()
	keepalive      chan struct{}
	ActiveBranches map[string]*ActiveBranchInfo
}

func (sess *ThreadSession) record() sessionRecord {
	branches := make(map[string]*ActiveBranchInfo, len(sess.ActiveBranches))
	for k, v := range sess.ActiveBranches {
		cp := *v
		branches[k] = &cp
	}
	return sessionRecord{
		ChannelID: sess.ChannelID, ThreadTS: sess.ThreadTS, UserID: sess.UserID, AgentID: sess.AgentID,
		CreatedAt: sess.CreatedAt, LastSeen: sess.LastSeen, Expires: sess.expires, ActiveBranches: branches,
	}
}

func (sess *ThreadSession) applyRecord(rec *sessionRecord) {
	sess.UserID = rec.UserID
	sess.AgentID = rec.AgentID
	sess.CreatedAt = rec.CreatedAt
	sess.LastSeen = rec.LastSeen
	sess.expires = rec.Expires
	sess.ActiveBranches = rec.ActiveBranches
	sess.checkedAt = time.Now()
}

// TryStartProcessing claims the thread for this request. It is false while
// this replica or another one is still answering an earlier message. The
// session is kept alive until DoneProcessing, so a long turn never expires
// under the user.
func (sess *ThreadSession) TryStartProcessing() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.processing {
		return false
	}
	if sess.store != nil && sess.store.b != nil {
		lease := store.NewLease(sess.store.b, sessionLockPrefix+sess.ChannelID+"/"+sess.ThreadTS, store.InstanceID(), processingLeaseTTL)
		_, release, ok, err := lease.Acquire(context.Background())
		switch {
		case err != nil:
			log.Printf("[session] processing lease unavailable channel=%s thread=%s: %v", sess.ChannelID, sess.ThreadTS, err)
		case !ok:
			return false
		default:
			sess.release = release
		}
	}
	sess.processing = true
	if sess.store != nil {
		stop := make(chan struct{})
		sess.keepalive = stop
		safego.Go("session: keepalive", func() { sess.store.keepAlive(sess, stop) })
	}
	return true
}

// DoneProcessing frees the thread for the next message and restarts the TTL
// from the moment the reply landed.
func (sess *ThreadSession) DoneProcessing() {
	sess.mu.Lock()
	release, stop := sess.release, sess.keepalive
	sess.release, sess.keepalive = nil, nil
	sess.processing = false
	sess.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if release != nil {
		release()
	}
	if sess.store != nil {
		sess.store.touch(sess)
	}
}

// Save writes the session, including its branch state, to the bucket.
func (sess *ThreadSession) Save() {
	if sess.store == nil {
		return
	}
	sess.mu.Lock()
	rec := sess.record()
	sess.mu.Unlock()
	if err := sess.store.put(rec); err != nil {
		log.Printf("[session] save failed channel=%s thread=%s: %v", sess.ChannelID, sess.ThreadTS, err)
	}
}

// SessionStore keeps thread sessions in the bucket with a short per-replica
// cache. Safe for concurrent use.
type SessionStore struct {
	b       *store.Backend
	ttl     time.Duration
	slack   SlackClient
	routers func(agentID string) *Router

	mu     sync.Mutex
	live   map[string]*ThreadSession
	misses map[string]time.Time

	statsMu     sync.Mutex
	statsAt     time.Time
	statsActive int
	statsTotals sessionStats
}

// NewSessionStore creates a store over b with the given TTL per session.
func NewSessionStore(b *store.Backend, ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionStore{b: b, ttl: ttl, live: map[string]*ThreadSession{}, misses: map[string]time.Time{}}
}

// SetSlack sets the client used for expiry notices.
func (s *SessionStore) SetSlack(c SlackClient) { s.slack = c }

// SetRouterResolver sets how a stored session finds its agent's router.
func (s *SessionStore) SetRouterResolver(fn func(agentID string) *Router) { s.routers = fn }

// TTL returns the configured session time-to-live.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

func sessionKey(channelID, threadTS string) string { return channelID + ":" + threadTS }

func sessionObjectKey(channelID, threadTS string) string {
	if !sessionSegmentRe.MatchString(channelID) || !sessionSegmentRe.MatchString(threadTS) {
		return ""
	}
	return sessionPrefix + channelID + "/" + threadTS + ".json"
}

func (s *SessionStore) put(rec sessionRecord) error {
	key := sessionObjectKey(rec.ChannelID, rec.ThreadTS)
	if key == "" {
		return fmt.Errorf("invalid session id %q/%q", rec.ChannelID, rec.ThreadTS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionPersistLimit)
	defer cancel()
	_, err := store.PutJSON(ctx, s.b, key, rec, store.Condition{})
	return err
}

func (s *SessionStore) fetch(channelID, threadTS string) (*sessionRecord, string, error) {
	key := sessionObjectKey(channelID, threadTS)
	if key == "" {
		return nil, "", store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionPersistLimit)
	defer cancel()
	return store.GetJSON[sessionRecord](ctx, s.b, key)
}

func (s *SessionStore) cached(key string) *ThreadSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[key]
}

func (s *SessionStore) attach(sess *ThreadSession) {
	sess.store = s
	if sess.Router == nil && s.routers != nil {
		sess.Router = s.routers(sess.AgentID)
	}
}

// Open creates a session for the thread, or refreshes the existing one, and
// returns it.
func (s *SessionStore) Open(channelID, threadTS, userID, agentID string, router *Router) *ThreadSession {
	if sessionObjectKey(channelID, threadTS) == "" {
		return nil
	}
	key := sessionKey(channelID, threadTS)
	now := time.Now().UTC()
	if sess := s.Lookup(channelID, threadTS); sess != nil {
		log.Printf("[session] refreshed channel=%s thread=%s user=%s agent=%s ttl=%s", channelID, threadTS, userID, agentID, s.ttl)
		return sess
	}
	sess := &ThreadSession{ChannelID: channelID, ThreadTS: threadTS, UserID: userID, AgentID: agentID, Router: router,
		CreatedAt: now, LastSeen: now, expires: now.Add(s.ttl), checkedAt: now, touchedAt: now}
	s.attach(sess)
	if err := s.put(sess.record()); err != nil {
		log.Printf("[session] open failed channel=%s thread=%s: %v", channelID, threadTS, err)
	}
	s.mu.Lock()
	s.live[key] = sess
	delete(s.misses, key)
	s.mu.Unlock()
	s.bump(func(st *sessionStats) { st.Opened++ })
	log.Printf("[session] opened channel=%s thread=%s user=%s agent=%s ttl=%s", channelID, threadTS, userID, agentID, s.ttl)
	return sess
}

// Lookup returns the live session for a thread, or nil. A hit extends the
// session.
func (s *SessionStore) Lookup(channelID, threadTS string) *ThreadSession {
	key := sessionKey(channelID, threadTS)
	now := time.Now().UTC()
	sess := s.cached(key)
	if sess != nil {
		sess.mu.Lock()
		fresh := now.Sub(sess.checkedAt) < sessionCacheFresh
		expired := now.After(sess.expires)
		sess.mu.Unlock()
		if fresh && !expired {
			s.touch(sess)
			return sess
		}
	}
	s.mu.Lock()
	missedAt, missed := s.misses[key]
	s.mu.Unlock()
	if missed && now.Sub(missedAt) < sessionMissTTL {
		return nil
	}
	rec, _, err := s.fetch(channelID, threadTS)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[session] lookup failed channel=%s thread=%s: %v", channelID, threadTS, err)
		}
		s.mu.Lock()
		s.misses[key] = now
		delete(s.live, key)
		s.mu.Unlock()
		return nil
	}
	if now.After(rec.Expires) {
		s.mu.Lock()
		s.misses[key] = now
		delete(s.live, key)
		s.mu.Unlock()
		return nil
	}
	if sess == nil {
		sess = &ThreadSession{ChannelID: channelID, ThreadTS: threadTS}
	}
	sess.mu.Lock()
	sess.applyRecord(rec)
	sess.mu.Unlock()
	s.attach(sess)
	s.mu.Lock()
	s.live[key] = sess
	delete(s.misses, key)
	s.mu.Unlock()
	s.touch(sess)
	return sess
}

func (s *SessionStore) keepAlive(sess *ThreadSession, stop <-chan struct{}) {
	t := time.NewTicker(sessionKeepAlive)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.touch(sess)
		}
	}
}

// slackResumeWindow is how long after it was asked a Slack request may still
// be answered by a resumed turn. Past it the person has moved on, and an
// answer arriving unbidden in an old thread is worse than none.
const slackResumeWindow = 15 * time.Minute

// maxSlackTurnAttempts bounds how often one Slack turn is started across
// restarts.
const maxSlackTurnAttempts = 2

// UseJournal recovers Slack turns that a restart cut short. The journal holds
// the thread and the message; the session holds which agent owns the thread,
// so a resumed turn is answered by the same agent that was asked. A turn that
// already reached a mutating tool is reported rather than replayed.
func (s *SessionStore) UseJournal(j *journal.Journal) {
	if s == nil || j == nil {
		return
	}
	j.Register(SlackJournalKind, journal.Handler{
		MaxAttempts: maxSlackTurnAttempts,
		MaxAge:      slackResumeWindow,
		Resume: func(_ context.Context, e journal.Entry) error {
			channelID, threadTS, userID, text, ok := parseTurnEntry(e)
			if !ok {
				return fmt.Errorf("malformed slack turn entry %q", e.Target)
			}
			sess := s.Lookup(channelID, threadTS)
			if sess == nil || sess.Router == nil {
				return fmt.Errorf("session %s/%s is gone", channelID, threadTS)
			}
			safego.Go("session: resume "+channelID+"/"+threadTS, func() {
				sess.Router.ResumeTurn(channelID, threadTS, userID, text)
			})
			return nil
		},
		Abandon: func(_ context.Context, e journal.Entry, reason string) {
			channelID, threadTS, _, _, ok := parseTurnEntry(e)
			if !ok || s.slack == nil {
				return
			}
			if err := s.slack.PostThreadReply(channelID, threadTS,
				":warning: I couldn't finish this request — "+reason+". Nothing further will happen automatically; send it again when you're ready."); err != nil {
				log.Printf("[session] interruption notice channel=%s thread=%s: %v", channelID, threadTS, err)
			}
		},
	})
}

// parseTurnEntry splits a slack-turn entry back into the thread it belongs to
// and the message that was being answered.
func parseTurnEntry(e journal.Entry) (channelID, threadTS, userID, text string, ok bool) {
	channelID, threadTS, ok = strings.Cut(e.Target, "/")
	if !ok {
		return "", "", "", "", false
	}
	userID, text, _ = strings.Cut(e.Detail, " ")
	return channelID, threadTS, userID, text, strings.TrimSpace(text) != ""
}

// touch extends the session and writes it back, at most every few seconds.
func (s *SessionStore) touch(sess *ThreadSession) {
	now := time.Now().UTC()
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	sess.LastSeen = now
	sess.expires = now.Add(s.ttl)
	sess.checkedAt = now
	if now.Sub(sess.touchedAt) < sessionTouchEvery {
		sess.mu.Unlock()
		return
	}
	sess.touchedAt = now
	rec := sess.record()
	sess.mu.Unlock()
	if err := s.put(rec); err != nil {
		log.Printf("[session] touch failed channel=%s thread=%s: %v", sess.ChannelID, sess.ThreadTS, err)
	}
}

// Close removes a session explicitly, for example when its anchor message
// is gone.
func (s *SessionStore) Close(channelID, threadTS, reason string) {
	key := sessionKey(channelID, threadTS)
	s.mu.Lock()
	sess := s.live[key]
	delete(s.live, key)
	s.misses[key] = time.Now()
	s.mu.Unlock()
	if sess != nil {
		sess.mu.Lock()
		sess.closed = true
		sess.mu.Unlock()
	}
	if objKey := sessionObjectKey(channelID, threadTS); objKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sessionPersistLimit)
		defer cancel()
		if err := s.b.Delete(ctx, objKey, ""); err != nil {
			log.Printf("[session] close failed channel=%s thread=%s: %v", channelID, threadTS, err)
			return
		}
	}
	s.bump(func(st *sessionStats) { st.Closed++ })
	duration := ""
	if sess != nil {
		duration = " duration=" + time.Since(sess.CreatedAt).Round(time.Millisecond).String()
	}
	log.Printf("[session] closed channel=%s thread=%s reason=%q%s", channelID, threadTS, reason, duration)
}

// bump applies fn to the shared counters in the background.
func (s *SessionStore) bump(fn func(*sessionStats)) {
	safego.Go("sessions: stats", func() {
		ctx, cancel := context.WithTimeout(context.Background(), sessionPersistLimit)
		defer cancel()
		for attempt := 0; attempt < 4; attempt++ {
			st, tag, err := store.GetJSON[sessionStats](ctx, s.b, sessionStatsKey)
			cond := store.Condition{IfMatch: tag}
			switch {
			case errors.Is(err, store.ErrNotFound):
				st = &sessionStats{}
				cond = store.Condition{IfNoneMatch: true}
			case err != nil:
				log.Printf("[session] stats read failed: %v", err)
				return
			}
			fn(st)
			_, err = store.PutJSON(ctx, s.b, sessionStatsKey, st, cond)
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			if err != nil {
				log.Printf("[session] stats write failed: %v", err)
			}
			return
		}
	})
}

// Stats returns the number of live sessions across replicas and the shared
// counters. Results are cached briefly.
func (s *SessionStore) Stats(ctx context.Context) (active int, opened, expired, closed int64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if time.Since(s.statsAt) < sessionStatsFresh {
		return s.statsActive, s.statsTotals.Opened, s.statsTotals.Expired, s.statsTotals.Closed
	}
	objs, err := s.b.List(ctx, sessionPrefix)
	if err != nil {
		log.Printf("[session] stats list failed: %v", err)
		return s.statsActive, s.statsTotals.Opened, s.statsTotals.Expired, s.statsTotals.Closed
	}
	cutoff := time.Now().Add(-s.ttl)
	active = 0
	for _, o := range objs {
		if o.Key == sessionStatsKey || !strings.HasSuffix(o.Key, ".json") || o.LastModified.Before(cutoff) {
			continue
		}
		active++
	}
	totals := sessionStats{}
	if st, _, err := store.GetJSON[sessionStats](ctx, s.b, sessionStatsKey); err == nil {
		totals = *st
	}
	s.statsAt, s.statsActive, s.statsTotals = time.Now(), active, totals
	return active, totals.Opened, totals.Expired, totals.Closed
}

// StartSweeper expires sessions nobody touched for the TTL, every interval,
// until ctx ends. One replica should run it.
func (s *SessionStore) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	safego.Go("sessions: sweep loop", func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				safego.Run("sessions: sweep", func() { s.sweep(ctx) })
			}
		}
	})
}

func (s *SessionStore) sweep(ctx context.Context) {
	objs, err := s.b.List(ctx, sessionPrefix)
	if err != nil {
		log.Printf("[session] sweep list failed: %v", err)
		return
	}
	cutoff := time.Now().Add(-s.ttl)
	for _, o := range objs {
		if o.Key == sessionStatsKey || !strings.HasSuffix(o.Key, ".json") || o.LastModified.After(cutoff) {
			continue
		}
		rec, tag, err := store.GetJSON[sessionRecord](ctx, s.b, o.Key)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("[session] sweep read %s: %v", o.Key, err)
			}
			continue
		}
		if time.Now().Before(rec.Expires) || s.processing(ctx, rec) {
			continue
		}
		if err := s.b.Delete(ctx, o.Key, tag); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				log.Printf("[session] sweep delete %s: %v", o.Key, err)
			}
			continue
		}
		s.mu.Lock()
		delete(s.live, sessionKey(rec.ChannelID, rec.ThreadTS))
		s.mu.Unlock()
		s.bump(func(st *sessionStats) { st.Expired++ })
		log.Printf("[session] expired channel=%s thread=%s user=%s agent=%s duration=%s",
			rec.ChannelID, rec.ThreadTS, rec.UserID, rec.AgentID, time.Since(rec.CreatedAt).Round(time.Millisecond))
		s.notifyExpired(rec)
	}
}

// processing reports whether a replica still holds the thread's lease, in
// which case expiry waits for the reply to land.
func (s *SessionStore) processing(ctx context.Context, rec *sessionRecord) bool {
	rec2, _, err := store.GetJSON[struct {
		Expires time.Time `json:"expires"`
	}](ctx, s.b, sessionLockPrefix+rec.ChannelID+"/"+rec.ThreadTS)
	return err == nil && time.Now().Before(rec2.Expires)
}

func (s *SessionStore) notifyExpired(rec *sessionRecord) {
	if s.slack == nil {
		return
	}
	if exists, err := s.slack.MessageExists(rec.ChannelID, rec.ThreadTS); err == nil && !exists {
		return
	}
	msg := fmt.Sprintf(
		"_:hourglass: Thread session expired after %d min of inactivity._\n"+
			"To continue this conversation, copy the link to this thread and paste it in a new `/%s` message — the bot will pick up the context automatically.\n"+
			"_(Right-click the thread timestamp → Copy link)_",
		int(s.ttl.Minutes()), rec.AgentID,
	)
	if err := s.slack.PostThreadReply(rec.ChannelID, rec.ThreadTS, msg); err != nil {
		log.Printf("[session] failed to post expiry notice channel=%s thread=%s: %v", rec.ChannelID, rec.ThreadTS, err)
	}
}
