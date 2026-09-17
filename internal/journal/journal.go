// Package journal records work that is in flight so a restart can find what
// never finished and resume it.
//
// Leases say who may run something; they say nothing about what was running
// when a pod died. An entry under journal/<kind>/<target>.json is written the
// moment work starts and removed when it ends, and its holder heartbeats it
// while it runs. An entry whose heartbeat has stopped is work that was
// interrupted — a rollout, an OOM kill, a node failure — and the sweep hands
// it back to the kind that registered it.
//
// Resuming is bounded on four axes, because the work behind an entry calls
// models, opens pull requests and posts to Slack:
//
//   - Side effects. The first mutating tool call marks the entry before it
//     runs, so a partly-applied run is reported rather than replayed.
//   - Attempts. Each resume increments the count; past the kind's budget the
//     entry is abandoned.
//   - Age. Work older than the kind's window is abandoned rather than
//     replayed into a context that has moved on.
//   - Volume. One sweep resumes a bounded number of entries, so a bucket full
//     of stale entries cannot stampede the fleet.
package journal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
)

// Prefix is the object prefix in-flight entries live under.
const Prefix = "journal/"

const (
	heartbeatEvery = 20 * time.Second
	// staleAfter is how long a heartbeat must have been silent before the work
	// counts as interrupted. Six missed beats, so a slow bucket or a paused
	// process is not mistaken for a dead one.
	staleAfter = 2 * time.Minute
	// carryWindow bounds how far back an entry's attempt count is inherited by
	// a fresh start on the same target, so a stale entry nobody swept cannot
	// spend a later run's retry budget.
	carryWindow = time.Hour

	// activeTTL bounds how stale the fleet-wide in-flight view served to
	// readers may be. One listing serves every reader in the window, so a
	// console polling a running workflow every few seconds costs at most one
	// listing per window however many people are watching.
	activeTTL = 5 * time.Second

	defaultMaxAttempts = 2
	defaultMaxAge      = time.Hour
	maxPerSweep        = 8
	opTimeout          = 30 * time.Second
)

var (
	kindRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	targetRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}/[A-Za-z0-9._-]{1,120}$`)
)

// Entry is one unit of work as it is stored while it runs.
type Entry struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// Detail is what the kind needs to start the work again.
	Detail    string    `json:"detail,omitempty"`
	Owner     string    `json:"owner,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	StartedAt time.Time `json:"started_at"`
	Heartbeat time.Time `json:"heartbeat"`
	Attempt   int       `json:"attempt"`
	// SideEffects marks work that reached a mutating tool call. Such an entry
	// is never replayed.
	SideEffects bool `json:"side_effects,omitempty"`
}

// Age is how long the work has been outstanding across all of its attempts.
func (e Entry) Age(now time.Time) time.Duration { return now.Sub(e.FirstSeen) }

// Handler is how one kind of work is recovered. Both functions may be nil.
type Handler struct {
	// Resume restarts interrupted work. It should return as soon as the work
	// is durably scheduled rather than waiting for it to finish.
	Resume func(ctx context.Context, e Entry) error
	// Abandon reports work that a safeguard stopped from being resumed.
	Abandon func(ctx context.Context, e Entry, reason string)
	// MaxAttempts bounds how often one piece of work is started. Default 2.
	MaxAttempts int
	// MaxAge bounds how long after it first started work may still be
	// resumed. Default one hour.
	MaxAge time.Duration
}

func (h Handler) withDefaults() Handler {
	if h.MaxAttempts <= 0 {
		h.MaxAttempts = defaultMaxAttempts
	}
	if h.MaxAge <= 0 {
		h.MaxAge = defaultMaxAge
	}
	return h
}

// Journal is the in-flight record kept in the state bucket.
type Journal struct {
	b      *store.Backend
	holder string

	mu    sync.RWMutex
	kinds map[string]Handler

	amu    sync.Mutex
	active map[string]activeView
}

// activeView is one listing of a kind's live entries, shared by every reader
// for activeTTL.
type activeView struct {
	at      time.Time
	targets map[string]bool
}

// New returns a journal over b. Register each kind before starting recovery.
func New(b *store.Backend) *Journal {
	return &Journal{b: b, holder: store.InstanceID(), kinds: map[string]Handler{}, active: map[string]activeView{}}
}

// Active reports which targets of kind are in flight anywhere in the fleet,
// as of a listing at most activeTTL old. An entry is written when work starts
// and heartbeated by whichever replica runs it, so this is the only view of
// "running" that holds across replicas: a runner's own in-memory flag says
// nothing about a run another pod picked up, and deferred work is claimed by
// any replica, not just the scheduling leader. An entry whose heartbeat has
// stopped is interrupted work waiting for the sweep, not a live run, and is
// left out. On a listing error the last good view is served, because reporting
// nothing in flight would read as "finished". The returned map is shared and
// must not be modified.
func (j *Journal) Active(ctx context.Context, kind string) map[string]bool {
	if j == nil || j.b == nil || !kindRe.MatchString(kind) {
		return nil
	}
	j.amu.Lock()
	defer j.amu.Unlock()
	if v, ok := j.active[kind]; ok && time.Since(v.at) < activeTTL {
		return v.targets
	}
	prefix := Prefix + kind + "/"
	objs, err := j.b.List(ctx, prefix)
	if err != nil {
		log.Printf("[journal] active %s: %v", kind, err)
		return j.active[kind].targets
	}
	now := time.Now()
	targets := make(map[string]bool, len(objs))
	for _, o := range objs {
		if now.Sub(o.LastModified) >= staleAfter {
			continue
		}
		targets[strings.TrimSuffix(strings.TrimPrefix(o.Key, prefix), ".json")] = true
	}
	j.active[kind] = activeView{at: now, targets: targets}
	return targets
}

// Register installs the recovery handler for a kind.
func (j *Journal) Register(kind string, h Handler) {
	if j == nil {
		return
	}
	if !kindRe.MatchString(kind) {
		log.Printf("[journal] refusing to register kind %q: not a valid key segment", kind)
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.kinds[kind] = h.withDefaults()
}

// Kinds lists the registered kind names.
func (j *Journal) Kinds() []string {
	if j == nil {
		return nil
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]string, 0, len(j.kinds))
	for k := range j.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (j *Journal) handler(kind string) (Handler, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	h, ok := j.kinds[kind]
	return h, ok
}

func key(kind, target string) string { return Prefix + kind + "/" + target + ".json" }

type ctxKey struct{}

// Begin records that work on target has started and returns a context the
// work must run under, so anything it reaches can mark it. The returned handle
// is always usable: a journal that cannot write simply records nothing.
func (j *Journal) Begin(ctx context.Context, kind, target, detail string) (context.Context, *Handle) {
	if j == nil || j.b == nil || !kindRe.MatchString(kind) || !targetRe.MatchString(target) {
		return ctx, nil
	}
	now := time.Now().UTC()
	e := Entry{Kind: kind, Target: target, Detail: detail, Owner: j.holder,
		FirstSeen: now, StartedAt: now, Heartbeat: now, Attempt: 1}
	k := key(kind, target)
	if prev, _, err := store.GetJSON[Entry](ctx, j.b, k); err == nil {
		if prev.Attempt > 0 && now.Sub(prev.FirstSeen) <= carryWindow {
			e.Attempt = prev.Attempt
			e.FirstSeen = prev.FirstSeen
		}
		if prev.Owner != "" && prev.Owner != j.holder && now.Sub(prev.Heartbeat) < staleAfter {
			log.Printf("[journal] %s/%s taken over from %s, which was still beating", kind, target, prev.Owner)
		}
	}
	tag, err := store.PutJSON(ctx, j.b, k, e, store.Condition{})
	if err != nil {
		log.Printf("[journal] begin %s/%s: %v", kind, target, err)
		return ctx, nil
	}
	h := &Handle{j: j, key: k, entry: e, tag: tag, stop: make(chan struct{})}
	safego.Go("journal: heartbeat "+kind+"/"+target, h.beat)
	return context.WithValue(ctx, ctxKey{}, h), h
}

// Handle is the live record of one piece of work. Its mutex is held across the
// store call of every write, so the heartbeat, a side-effect mark and the final
// delete cannot race each other onto the same ETag — a lost race there would
// stop the heartbeat, or leave a finished entry behind to be resumed.
type Handle struct {
	j   *Journal
	key string

	stop chan struct{}
	once sync.Once

	mu     sync.Mutex
	entry  Entry
	tag    string
	closed bool
}

// NoteSideEffect records, durably and before the fact, that the work is about
// to change something outside this process. Only the first call writes.
func (h *Handle) NoteSideEffect() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.entry.SideEffects {
		return
	}
	h.entry.SideEffects = true
	h.write(time.Now().UTC())
}

// NoteSideEffect marks the work running under ctx as having made a change. It
// is a no-op when ctx carries no handle.
func NoteSideEffect(ctx context.Context) {
	if h, ok := ctx.Value(ctxKey{}).(*Handle); ok {
		h.NoteSideEffect()
	}
}

// Done removes the entry: the work finished, whether it succeeded or failed on
// its own terms, and must not be resumed.
func (h *Handle) Done() {
	if h == nil {
		return
	}
	h.finish(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := h.j.b.Delete(ctx, h.key, h.tag); err != nil && !errors.Is(err, store.ErrConflict) {
			log.Printf("[journal] clear %s: %v", h.key, err)
		}
	})
}

// Interrupted stops the heartbeat and leaves the entry behind, so the sweep
// picks the work up. Use it when the work was cut short rather than finished.
func (h *Handle) Interrupted() {
	if h == nil {
		return
	}
	h.finish(nil)
}

func (h *Handle) finish(fn func()) {
	h.once.Do(func() {
		close(h.stop)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.closed = true
		if fn != nil {
			fn()
		}
	})
}

func (h *Handle) beat() {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		live := !h.closed && h.write(time.Now().UTC())
		h.mu.Unlock()
		if !live {
			return
		}
	}
}

// write stores the entry under the tag it was last read at, and reports
// whether the entry is still ours. A lost race means the sweep has taken it
// over, so the holder stops writing. Callers hold h.mu.
func (h *Handle) write(now time.Time) bool {
	e := h.entry
	e.Heartbeat = now
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	tag, err := store.PutJSON(ctx, h.j.b, h.key, e, store.Condition{IfMatch: h.tag})
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		log.Printf("[journal] %s no longer ours; stopping heartbeat", h.key)
		return false
	case err != nil:
		log.Printf("[journal] heartbeat %s: %v", h.key, err)
		return true
	}
	h.entry, h.tag = e, tag
	return true
}

// StartRecovery sweeps for interrupted work every interval until ctx ends,
// starting with one sweep right away. Run it where scheduling runs: the sweep
// claims each entry with a conditional write, so a second sweeper is safe but
// buys nothing.
func (j *Journal) StartRecovery(ctx context.Context, interval time.Duration) {
	if j == nil || j.b == nil || len(j.Kinds()) == 0 {
		return
	}
	safego.Go("journal: recovery", func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			safego.Run("journal: sweep", func() { j.Recover(ctx) })
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// Recover resumes or abandons every interrupted entry it finds, up to the
// per-sweep cap.
func (j *Journal) Recover(ctx context.Context) {
	if j == nil || j.b == nil {
		return
	}
	objs, err := j.b.List(ctx, Prefix)
	if err != nil {
		log.Printf("[journal] list: %v", err)
		return
	}
	sort.Slice(objs, func(a, b int) bool { return objs[a].LastModified.Before(objs[b].LastModified) })
	acted := 0
	for _, o := range objs {
		if acted >= maxPerSweep || ctx.Err() != nil {
			return
		}
		kind, _, ok := strings.Cut(strings.TrimPrefix(o.Key, Prefix), "/")
		if !ok {
			continue
		}
		// An entry of a kind this binary does not know belongs to another
		// release; during a rolling deploy the replica that knows it sweeps it.
		h, known := j.handler(kind)
		if !known {
			continue
		}
		if time.Since(o.LastModified) < staleAfter {
			continue
		}
		if j.recoverOne(ctx, o.Key, h) {
			acted++
		}
	}
}

func (j *Journal) recoverOne(ctx context.Context, key string, h Handler) bool {
	e, tag, err := store.GetJSON[Entry](ctx, j.b, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		log.Printf("[journal] read %s: %v", key, err)
		return false
	}
	now := time.Now().UTC()
	if now.Sub(e.Heartbeat) < staleAfter {
		return false
	}
	if reason := refuse(*e, now, h); reason != "" {
		if err := j.b.Delete(ctx, key, tag); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				log.Printf("[journal] retire %s: %v", key, err)
			}
			return false
		}
		log.Printf("[journal] abandoning %s/%s: %s", e.Kind, e.Target, reason)
		if h.Abandon != nil {
			safego.Run("journal: abandon "+e.Kind, func() { h.Abandon(ctx, *e, reason) })
		}
		return true
	}
	// Claiming is the conditional write: the new attempt count and heartbeat
	// are stored before the work is scheduled, so a second sweeper finds an
	// entry that is neither stale nor at its old attempt.
	next := *e
	next.Attempt++
	next.Owner = ""
	next.StartedAt = now
	next.Heartbeat = now
	if _, err := store.PutJSON(ctx, j.b, key, next, store.Condition{IfMatch: tag}); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			log.Printf("[journal] claim %s: %v", key, err)
		}
		return false
	}
	log.Printf("[journal] resuming %s/%s (attempt %d/%d, interrupted after %s)",
		next.Kind, next.Target, next.Attempt, h.MaxAttempts, next.Age(now).Round(time.Second))
	if h.Resume == nil {
		return true
	}
	if err := h.Resume(ctx, next); err != nil {
		log.Printf("[journal] resume %s/%s: %v", next.Kind, next.Target, err)
	}
	return true
}

// refuse names the safeguard that stops an entry from being resumed, or "".
func refuse(e Entry, now time.Time, h Handler) string {
	switch {
	case e.SideEffects:
		return "it was interrupted after it had already made changes"
	case e.Attempt >= h.MaxAttempts:
		return fmt.Sprintf("it was interrupted on every one of its %d attempts", h.MaxAttempts)
	case e.Age(now) > h.MaxAge:
		return fmt.Sprintf("it has been outstanding for %s, past the %s resume window", e.Age(now).Round(time.Minute), h.MaxAge)
	}
	return ""
}
