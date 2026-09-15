// Package queue is a durable work queue kept in the state bucket. One task is
// one object under queue/<topic>/; workers on every replica claim tasks with
// conditional writes, so a task is owned by exactly one replica at a time
// without a leader, a broker or any shared disk.
//
// Two delivery modes cover what the platform defers. A topic with MaxAttempts
// above 1 is at-least-once: the task survives a failed handler and is retried
// with backoff, which suits idempotent bookkeeping. A topic with MaxAttempts 1
// is at-most-once: the task is removed the moment it is claimed, so a replica
// that dies mid-handler loses the work instead of replaying a side effect.
package queue

import (
	"context"
	"encoding/json"
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
	"github.com/justmike1/arbetern/internal/text"
)

// Prefix is the object prefix every task lives under.
const Prefix = "queue/"

const (
	defaultPoll        = 5 * time.Second
	maxPoll            = 30 * time.Second
	maxIdleSteps       = 3
	defaultVisibility  = 5 * time.Minute
	defaultTimeout     = 2 * time.Minute
	defaultMaxAttempts = 3
	retryBackoff       = 30 * time.Second
	maxRetryBackoff    = 15 * time.Minute
	maxBatch           = 64
	maxWorkers         = 4
	maxErrorChars      = 400
)

// ErrUnknownTopic is returned when enqueuing to a topic no worker handles.
var ErrUnknownTopic = errors.New("queue: unknown topic")

// topicRe constrains a topic to what is safe as one object-key segment. Topics
// are compile-time constants today, but the name becomes part of an S3 key, so
// it is checked at registration rather than trusted.
var topicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Task is one unit of deferred work as it is stored.
type Task struct {
	ID         string          `json:"id"`
	Topic      string          `json:"topic"`
	Payload    json.RawMessage `json:"payload"`
	EnqueuedAt time.Time       `json:"enqueued_at"`
	NotBefore  time.Time       `json:"not_before,omitempty"`
	Attempts   int             `json:"attempts,omitempty"`
	Owner      string          `json:"owner,omitempty"`
	LeaseUntil time.Time       `json:"lease_until,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
}

// Handler runs one task. Returning an error retries the task on topics that
// allow more than one attempt.
type Handler func(ctx context.Context, payload []byte) error

// Options tune one topic's delivery.
type Options struct {
	// Visibility is how long a claim holds a task before another worker may
	// take it over.
	Visibility time.Duration
	// Timeout bounds a single handler run.
	Timeout time.Duration
	// MaxAttempts bounds delivery. 1 makes the topic at-most-once: the task is
	// dropped at claim time so a handler with side effects never replays.
	MaxAttempts int
}

func (o Options) withDefaults() Options {
	if o.Visibility <= 0 {
		o.Visibility = defaultVisibility
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.Timeout > o.Visibility {
		o.Visibility = o.Timeout + time.Minute
	}
	return o
}

type registration struct {
	opts    Options
	handler Handler
}

// TopicStat reports one topic's backlog, for operational display.
type TopicStat struct {
	Topic     string `json:"topic"`
	Pending   int    `json:"pending"`
	OldestAge string `json:"oldest_age,omitempty"`
}

// Queue dispatches deferred work through the state bucket.
type Queue struct {
	b      *store.Backend
	holder string
	poll   time.Duration

	mu     sync.RWMutex
	topics map[string]*registration
	stats  []TopicStat

	wake chan struct{}
	sem  chan struct{}
}

// New returns a queue over b. Register topics before calling Start.
func New(b *store.Backend) *Queue {
	return &Queue{
		b:      b,
		holder: store.InstanceID(),
		poll:   defaultPoll,
		topics: map[string]*registration{},
		wake:   make(chan struct{}, 1),
		sem:    make(chan struct{}, maxWorkers),
	}
}

// Register installs the handler for a topic. It must be called before Start.
func (q *Queue) Register(topic string, opts Options, h Handler) {
	if q == nil || h == nil {
		return
	}
	if !topicRe.MatchString(topic) {
		log.Printf("[queue] refusing to register topic %q: not a valid key segment", topic)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.topics[topic] = &registration{opts: opts.withDefaults(), handler: h}
}

// Topics lists the registered topic names.
func (q *Queue) Topics() []string {
	if q == nil {
		return nil
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]string, 0, len(q.topics))
	for t := range q.topics {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Stats reports the backlog seen on the last poll.
func (q *Queue) Stats() []TopicStat {
	if q == nil {
		return nil
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]TopicStat, len(q.stats))
	copy(out, q.stats)
	return out
}

func (q *Queue) registration(topic string) *registration {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.topics[topic]
}

// taskKey names a task so a lexicographic listing is oldest-first.
func taskKey(topic string, at time.Time, id string) string {
	return Prefix + topic + "/" + fmt.Sprintf("%012x", at.UTC().UnixMilli()) + "-" + id + ".json"
}

// Enqueue stores payload as a task and nudges the local worker so a task
// enqueued on this replica is usually picked up before the next poll.
func (q *Queue) Enqueue(ctx context.Context, topic string, payload any) error {
	if q == nil {
		return ErrUnknownTopic
	}
	if q.registration(topic) == nil {
		return fmt.Errorf("%w: %s", ErrUnknownTopic, topic)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	id, err := store.NewID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	t := Task{ID: id, Topic: topic, Payload: body, EnqueuedAt: now}
	if _, err := store.PutJSON(ctx, q.b, taskKey(topic, now, id), t, store.Condition{IfNoneMatch: true}); err != nil {
		return err
	}
	q.nudge()
	return nil
}

func (q *Queue) nudge() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Start runs the worker until ctx ends. Every replica may run one: claims are
// conditional writes, so the same task is never handled twice at once.
//
// An idle queue backs its polling off to maxPoll, because the common state of
// this queue is empty and a fixed 5-second listing across a fleet costs far
// more in requests than it saves in latency. An enqueue on this replica wakes
// the loop immediately, and one that landed on another replica waits at most
// one poll.
func (q *Queue) Start(ctx context.Context) {
	if q == nil || len(q.Topics()) == 0 {
		return
	}
	safego.Go("queue: worker", func() {
		idle := 0
		for {
			found := 0
			safego.Run("queue: poll", func() { found = q.pollOnce(ctx) })
			switch {
			case found > 0:
				idle = 0
			case idle < maxIdleSteps:
				idle++
			}
			wait := q.poll << idle
			if wait > maxPoll {
				wait = maxPoll
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-q.wake:
				idle = 0
			case <-t.C:
			}
			t.Stop()
		}
	})
}

// pollOnce lists the whole queue prefix once — not once per topic, so adding a
// topic costs no extra requests — and hands every due task to a worker. It
// returns how many tasks it saw.
func (q *Queue) pollOnce(ctx context.Context) int {
	objs, err := q.b.List(ctx, Prefix)
	if err != nil {
		log.Printf("[queue] list: %v", err)
		return 0
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })

	stats := map[string]*TopicStat{}
	for _, topic := range q.Topics() {
		stats[topic] = &TopicStat{Topic: topic}
	}
	var wg sync.WaitGroup
	for _, o := range objs {
		topic, name, ok := strings.Cut(strings.TrimPrefix(o.Key, Prefix), "/")
		if !ok || !strings.HasSuffix(name, ".json") {
			continue
		}
		// A task for a topic this binary does not know is left alone: during a
		// rolling deploy the replica that does know it will take it.
		reg := q.registration(topic)
		st := stats[topic]
		if reg == nil || st == nil {
			continue
		}
		if st.Pending == 0 {
			st.OldestAge = time.Since(o.LastModified).Round(time.Second).String()
		}
		st.Pending++
		if st.Pending > maxBatch {
			continue
		}
		key := o.Key
		wg.Add(1)
		q.sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-q.sem }()
			safego.Run("queue: task "+topic, func() { q.claimAndRun(ctx, reg, key) })
		}()
	}
	wg.Wait()

	out := make([]TopicStat, 0, len(stats))
	total := 0
	for _, topic := range q.Topics() {
		if st := stats[topic]; st != nil {
			out = append(out, *st)
			total += st.Pending
		}
	}
	q.mu.Lock()
	q.stats = out
	q.mu.Unlock()
	return total
}

// claimAndRun takes ownership of one task with a conditional write and runs it.
// A lost race, a task another replica already owns and a task not yet due all
// return without touching the object.
func (q *Queue) claimAndRun(ctx context.Context, reg *registration, key string) {
	body, tag, err := q.b.Get(ctx, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		log.Printf("[queue] read %s: %v", key, err)
		return
	}
	t := &Task{}
	if err := json.Unmarshal(body, t); err != nil {
		// Nothing will ever run this. Left in place it would be re-read on
		// every poll forever, so it goes rather than becoming a poison pill.
		q.drop(ctx, key, tag, "undecodable task: "+err.Error())
		return
	}
	now := time.Now().UTC()
	if t.NotBefore.After(now) || (t.LeaseUntil.After(now) && t.Owner != q.holder) {
		return
	}
	if t.Attempts < 0 {
		t.Attempts = 0
	}
	t.Attempts++
	t.Owner = q.holder
	t.LeaseUntil = now.Add(reg.opts.Visibility)
	if t.Attempts > reg.opts.MaxAttempts {
		q.drop(ctx, key, tag, "attempts exhausted")
		return
	}
	tag, err = store.PutJSON(ctx, q.b, key, t, store.Condition{IfMatch: tag})
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		log.Printf("[queue] claim %s: %v", key, err)
		return
	}

	// At-most-once topics give the task up before running it, so a replica that
	// dies inside the handler cannot have its side effect replayed by another.
	atMostOnce := reg.opts.MaxAttempts == 1
	if atMostOnce {
		q.drop(ctx, key, tag, "")
	}

	// The handler and the bookkeeping that follows it outlive a cancelled poll
	// loop: a shutdown between finishing the work and recording that must not
	// leave the task to be delivered again.
	done := context.WithoutCancel(ctx)
	rctx, cancel := context.WithTimeout(done, reg.opts.Timeout)
	defer cancel()
	if err := reg.handler(rctx, t.Payload); err != nil {
		log.Printf("[queue] %s task %s failed (attempt %d/%d): %v", t.Topic, t.ID, t.Attempts, reg.opts.MaxAttempts, err)
		if !atMostOnce {
			q.retry(done, key, tag, t, reg, err)
		}
		return
	}
	if !atMostOnce {
		q.drop(done, key, tag, "")
	}
}

// retry hands the task back for a later attempt, or drops it when the topic's
// attempt budget is spent.
func (q *Queue) retry(ctx context.Context, key, tag string, t *Task, reg *registration, cause error) {
	if t.Attempts >= reg.opts.MaxAttempts {
		q.drop(ctx, key, tag, fmt.Sprintf("gave up after %d attempts: %v", t.Attempts, cause))
		return
	}
	backoff := retryBackoff << (t.Attempts - 1)
	if backoff > maxRetryBackoff {
		backoff = maxRetryBackoff
	}
	t.Owner = ""
	t.LeaseUntil = time.Time{}
	t.NotBefore = time.Now().UTC().Add(backoff)
	t.LastError = text.Truncate(cause.Error(), maxErrorChars)
	if _, err := store.PutJSON(ctx, q.b, key, t, store.Condition{IfMatch: tag}); err != nil && !errors.Is(err, store.ErrConflict) {
		log.Printf("[queue] reschedule %s: %v", key, err)
	}
}

func (q *Queue) drop(ctx context.Context, key, tag, reason string) {
	if reason != "" {
		log.Printf("[queue] dropping %s: %s", key, reason)
	}
	if err := q.b.Delete(ctx, key, tag); err != nil && !errors.Is(err, store.ErrConflict) {
		log.Printf("[queue] delete %s: %v", key, err)
	}
}
