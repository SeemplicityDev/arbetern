// Package metrics records how the platform performs: how long an agent turn
// takes, how much of that is the model and how much is tools, how many rounds
// and tool calls a turn needs, how often a turn ends without an answer, and how
// the model provider responds under load.
//
// Every sample is deliberately abstract. A sample is keyed by agent, entry
// path, model, backend and tool name only — never by the person, channel or
// organisation behind the turn — so the series can be retained and monitored
// indefinitely without carrying anyone's identity. Who asked for what is
// already covered by the usage ledger.
//
// Storage mirrors the usage ledger: one rolling object per calendar month
// (perf-YYYY-MM.json) plus a short feed of recent turns. Samples are buffered
// in memory and merged into the stored aggregates with conditional writes, so
// several replicas record concurrently without losing each other's work.
// Latency is kept as a histogram, so percentiles survive both the merge and the
// month rollup.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
)

// Prefix is the object prefix the performance series is stored under.
const Prefix = "metrics/"

const (
	recentKey        = Prefix + "recent.json"
	maxRecentTurns   = 200
	maxSlowestTurns  = 20
	maxSummaryRecent = 50
	flushInterval    = 20 * time.Second
	maxMergeAttempts = 4
)

// Outcomes a turn can end with. Everything other than OutcomeCompleted means
// the requester did not get an answer.
const (
	OutcomeCompleted = "completed"
	OutcomeEmpty     = "empty"
	OutcomeMaxRounds = "max_rounds"
	OutcomeTruncated = "truncated"
	OutcomeNoChoices = "no_choices"
	OutcomeBlocked   = "blocked_ack"
	OutcomeError     = "error"
)

// Turn is one finished agent turn.
type Turn struct {
	At      time.Time `json:"at"`
	Agent   string    `json:"agent"`
	Source  string    `json:"source"`
	Model   string    `json:"model"`
	Outcome string    `json:"outcome"`
	// DurationMS is the wall clock of the whole turn; FirstResponseMS is how
	// long the first model round took, which is what a waiting requester feels
	// before anything at all happens.
	DurationMS      int64 `json:"duration_ms"`
	FirstResponseMS int64 `json:"first_response_ms,omitempty"`
	ModelMS         int64 `json:"model_ms,omitempty"`
	ToolMS          int64 `json:"tool_ms,omitempty"`
	Rounds          int   `json:"rounds,omitempty"`
	ToolCalls       int   `json:"tool_calls,omitempty"`
	OutputTokens    int   `json:"output_tokens,omitempty"`
	Failed          bool  `json:"failed,omitempty"`
}

// Call is one provider round-trip, retries included.
type Call struct {
	At          time.Time `json:"at"`
	Model       string    `json:"model"`
	Backend     string    `json:"backend"`
	LatencyMS   int64     `json:"latency_ms"`
	Attempts    int       `json:"attempts"`
	RateLimited bool      `json:"rate_limited,omitempty"`
	Failed      bool      `json:"failed,omitempty"`
}

// ToolRun is one tool invocation inside a turn.
type ToolRun struct {
	At        time.Time `json:"at"`
	Agent     string    `json:"agent"`
	Name      string    `json:"name"`
	LatencyMS int64     `json:"latency_ms"`
	Failed    bool      `json:"failed,omitempty"`
}

// TurnStat is the rollup of many turns along one dimension.
type TurnStat struct {
	Latency       Stat             `json:"latency"`
	FirstResponse Stat             `json:"first_response"`
	Rounds        int64            `json:"rounds"`
	ToolCalls     int64            `json:"tool_calls"`
	ModelMS       int64            `json:"model_ms"`
	ToolMS        int64            `json:"tool_ms"`
	OutputTokens  int64            `json:"output_tokens"`
	Outcomes      map[string]int64 `json:"outcomes,omitempty"`
}

func (t *TurnStat) add(e Turn) {
	t.Latency.observe(e.DurationMS, e.Failed)
	if e.FirstResponseMS > 0 {
		t.FirstResponse.observe(e.FirstResponseMS, false)
	}
	t.Rounds += int64(e.Rounds)
	t.ToolCalls += int64(e.ToolCalls)
	t.ModelMS += e.ModelMS
	t.ToolMS += e.ToolMS
	t.OutputTokens += int64(e.OutputTokens)
	if t.Outcomes == nil {
		t.Outcomes = map[string]int64{}
	}
	t.Outcomes[e.Outcome]++
}

func (t *TurnStat) merge(o *TurnStat) {
	if o == nil {
		return
	}
	t.Latency.merge(&o.Latency)
	t.FirstResponse.merge(&o.FirstResponse)
	t.Rounds += o.Rounds
	t.ToolCalls += o.ToolCalls
	t.ModelMS += o.ModelMS
	t.ToolMS += o.ToolMS
	t.OutputTokens += o.OutputTokens
	if len(o.Outcomes) == 0 {
		return
	}
	if t.Outcomes == nil {
		t.Outcomes = map[string]int64{}
	}
	for k, v := range o.Outcomes {
		t.Outcomes[k] += v
	}
}

// CallStat is the rollup of provider round-trips along one dimension.
type CallStat struct {
	Latency     Stat  `json:"latency"`
	Attempts    int64 `json:"attempts"`
	Retried     int64 `json:"retried"`
	RateLimited int64 `json:"rate_limited"`
}

func (c *CallStat) add(e Call) {
	c.Latency.observe(e.LatencyMS, e.Failed)
	attempts := e.Attempts
	if attempts < 1 {
		attempts = 1
	}
	c.Attempts += int64(attempts)
	if attempts > 1 {
		c.Retried++
	}
	if e.RateLimited {
		c.RateLimited++
	}
}

func (c *CallStat) merge(o *CallStat) {
	if o == nil {
		return
	}
	c.Latency.merge(&o.Latency)
	c.Attempts += o.Attempts
	c.Retried += o.Retried
	c.RateLimited += o.RateLimited
}

// monthData is the persisted shape of one calendar month.
type monthData struct {
	Month   string               `json:"month"`
	Totals  TurnStat             `json:"totals"`
	Days    map[string]*TurnStat `json:"days"`
	Agents  map[string]*TurnStat `json:"agents"`
	Sources map[string]*TurnStat `json:"sources"`
	Models  map[string]*TurnStat `json:"models"`
	Calls   map[string]*CallStat `json:"calls"`
	Tools   map[string]*Stat     `json:"tools"`
}

func newMonth(key string) *monthData {
	m := &monthData{Month: key}
	m.ensureMaps()
	return m
}

func (m *monthData) ensureMaps() {
	if m.Days == nil {
		m.Days = map[string]*TurnStat{}
	}
	if m.Agents == nil {
		m.Agents = map[string]*TurnStat{}
	}
	if m.Sources == nil {
		m.Sources = map[string]*TurnStat{}
	}
	if m.Models == nil {
		m.Models = map[string]*TurnStat{}
	}
	if m.Calls == nil {
		m.Calls = map[string]*CallStat{}
	}
	if m.Tools == nil {
		m.Tools = map[string]*Stat{}
	}
}

func (m *monthData) addTurn(e Turn) {
	m.Totals.add(e)
	turnBucket(m.Days, dayKey(e.At)).add(e)
	turnBucket(m.Agents, e.Agent).add(e)
	turnBucket(m.Sources, e.Source).add(e)
	turnBucket(m.Models, e.Model).add(e)
}

func (m *monthData) addCall(e Call) {
	key := e.Model
	if key == "" {
		key = e.Backend
	}
	c := m.Calls[key]
	if c == nil {
		c = &CallStat{}
		m.Calls[key] = c
	}
	c.add(e)
}

func (m *monthData) addTool(e ToolRun) {
	s := m.Tools[e.Name]
	if s == nil {
		s = &Stat{}
		m.Tools[e.Name] = s
	}
	s.observe(e.LatencyMS, e.Failed)
}

func turnBucket(m map[string]*TurnStat, k string) *TurnStat {
	if k == "" {
		k = "(unknown)"
	}
	t := m[k]
	if t == nil {
		t = &TurnStat{}
		m[k] = t
	}
	return t
}

func monthObjectKey(month string) string { return Prefix + "perf-" + month + ".json" }

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }
func dayKey(t time.Time) string   { return t.UTC().Format("2006-01-02") }

// monthRe validates a month key read back from the bucket, since the key
// becomes the variable part of the object name on the next flush.
var monthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)

type pending struct {
	turns []Turn
	calls []Call
	tools []ToolRun
}

func (p *pending) empty() bool { return len(p.turns)+len(p.calls)+len(p.tools) == 0 }

// Store aggregates performance samples in memory and merges them into the
// bucket. Safe for concurrent use.
type Store struct {
	b       *store.Backend
	startAt time.Time

	mu      sync.RWMutex
	months  map[string]*monthData
	recent  []Turn
	pending map[string]*pending

	queueStats   func() any
	dependencies func() any
}

// New constructs a Store over b, loading the stored aggregates.
func New(ctx context.Context, b *store.Backend) (*Store, error) {
	s := &Store{
		b:       b,
		startAt: time.Now().UTC(),
		months:  map[string]*monthData{},
		pending: map[string]*pending{},
	}
	if err := s.load(ctx); err != nil {
		return nil, fmt.Errorf("load metrics: %w", err)
	}
	if n := s.Months(); n > 0 {
		log.Printf("[metrics] loaded %d month(s) of performance data from %s", n, b)
	}
	return s, nil
}

// Months is the number of calendar months with recorded samples.
func (s *Store) Months() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.months)
}

// SetQueueStats installs a lookup for the deferred-work backlog, surfaced
// alongside the latency series so a slow queue is visible next to slow turns.
func (s *Store) SetQueueStats(fn func() any) {
	s.mu.Lock()
	s.queueStats = fn
	s.mu.Unlock()
}

// SetDependencies installs a lookup for the optional endpoints the platform
// stops calling when they misbehave, so an operator reading a latency spike can
// see that one of them is out of the path instead of inferring it.
func (s *Store) SetDependencies(fn func() any) {
	s.mu.Lock()
	s.dependencies = fn
	s.mu.Unlock()
}

func (s *Store) bufferFor(mk string) (*monthData, *pending) {
	m := s.months[mk]
	if m == nil {
		m = newMonth(mk)
		s.months[mk] = m
	}
	p := s.pending[mk]
	if p == nil {
		p = &pending{}
		s.pending[mk] = p
	}
	return m, p
}

// RecordTurn tallies one finished turn. Never returns an error: recording must
// never break an agent turn.
func (s *Store) RecordTurn(e Turn) {
	if s == nil || e.DurationMS < 0 {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Outcome == "" {
		e.Outcome = OutcomeCompleted
	}
	s.mu.Lock()
	m, p := s.bufferFor(monthKey(e.At))
	m.addTurn(e)
	p.turns = append(p.turns, e)
	s.recent = capRecent(append(s.recent, e))
	s.mu.Unlock()
}

// RecordCall tallies one provider round-trip.
func (s *Store) RecordCall(e Call) {
	if s == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.mu.Lock()
	m, p := s.bufferFor(monthKey(e.At))
	m.addCall(e)
	p.calls = append(p.calls, e)
	s.mu.Unlock()
}

// RecordTool tallies one tool invocation.
func (s *Store) RecordTool(e ToolRun) {
	if s == nil || e.Name == "" {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.mu.Lock()
	m, p := s.bufferFor(monthKey(e.At))
	m.addTool(e)
	p.tools = append(p.tools, e)
	s.mu.Unlock()
}

// StartFlusher merges buffered samples into the bucket on a timer until stop is
// closed, then once more. The returned channel closes after that final flush.
func (s *Store) StartFlusher(stop <-chan struct{}) <-chan struct{} {
	return safego.Every("metrics: flush", flushInterval, stop, func() { s.flush(context.Background()) })
}

func (s *Store) flush(ctx context.Context) {
	s.mu.Lock()
	batch := s.pending
	s.pending = map[string]*pending{}
	s.mu.Unlock()
	var flushed []Turn
	for mk, p := range batch {
		if p.empty() {
			continue
		}
		if err := s.mergeMonth(ctx, mk, p); err != nil {
			log.Printf("[metrics] persist %s failed: %v", mk, err)
			s.mu.Lock()
			_, cur := s.bufferFor(mk)
			cur.turns = append(p.turns, cur.turns...)
			cur.calls = append(p.calls, cur.calls...)
			cur.tools = append(p.tools, cur.tools...)
			s.mu.Unlock()
			continue
		}
		flushed = append(flushed, p.turns...)
	}
	if len(flushed) > 0 {
		if err := s.mergeRecent(ctx, flushed); err != nil {
			log.Printf("[metrics] persist recent failed: %v", err)
		}
	}
}

// mergeMonth folds a batch into the stored month and adopts the merged result
// locally, so samples recorded by other replicas show up here too.
func (s *Store) mergeMonth(ctx context.Context, mk string, p *pending) error {
	merged, err := store.Merge(ctx, s.b, monthObjectKey(mk), maxMergeAttempts, func(m *monthData) {
		m.Month = mk
		m.ensureMaps()
		apply(m, p)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	if cur := s.pending[mk]; cur != nil {
		apply(merged, cur)
	}
	s.months[mk] = merged
	s.mu.Unlock()
	return nil
}

func apply(m *monthData, p *pending) {
	for _, e := range p.turns {
		m.addTurn(e)
	}
	for _, e := range p.calls {
		m.addCall(e)
	}
	for _, e := range p.tools {
		m.addTool(e)
	}
}

// mergeRecent appends to the stored turn feed and adopts the merged feed.
func (s *Store) mergeRecent(ctx context.Context, added []Turn) error {
	merged, err := store.Merge(ctx, s.b, recentKey, maxMergeAttempts, func(list *[]Turn) {
		*list = capRecent(append(*list, added...))
	})
	if err != nil {
		return err
	}
	list := *merged
	s.mu.Lock()
	for _, p := range s.pending {
		list = append(list, p.turns...)
	}
	s.recent = capRecent(list)
	s.mu.Unlock()
	return nil
}

func capRecent(list []Turn) []Turn {
	sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	if len(list) > maxRecentTurns {
		list = list[len(list)-maxRecentTurns:]
	}
	return list
}

// activeMonths are the aggregates a refresh re-reads: the current month and the
// one before it. A sample is stamped when it is recorded and flushed within
// seconds, so an older aggregate can no longer change — re-reading the whole
// history every interval would be waste that grows with the deployment's age.
func activeMonths(now time.Time) []string {
	now = now.UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return []string{monthKey(now), monthKey(first.AddDate(0, 0, -1))}
}

// Refresh picks up what other replicas wrote, then re-applies whatever this one
// has buffered but not yet flushed.
//
// Without it a replica only ever saw the samples it recorded itself: the merge
// on flush adopts the shared document, but a replica serving nothing but the
// console never flushes, so it would report zero while its siblings reported
// the truth — and which one answered the request decided what you saw.
func (s *Store) Refresh(ctx context.Context) error {
	fresh := map[string]*monthData{}
	for _, mk := range activeMonths(time.Now()) {
		m, _, err := store.GetJSON[monthData](ctx, s.b, monthObjectKey(mk))
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return err
		}
		if !monthRe.MatchString(m.Month) {
			continue
		}
		m.ensureMaps()
		fresh[m.Month] = m
	}
	recent, err := s.loadRecent(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Only a month that was just replaced by the stored copy needs its buffered
	// samples put back: every other cached month still carries them from when
	// they were recorded, and applying them again would count them twice.
	for mk, m := range fresh {
		s.months[mk] = m
		if p := s.pending[mk]; p != nil {
			apply(m, p)
		}
	}
	// The feed, by contrast, is replaced wholesale, so everything buffered here
	// goes back on top of it.
	for _, p := range s.pending {
		recent = append(recent, p.turns...)
	}
	s.recent = capRecent(recent)
	return nil
}

func (s *Store) loadRecent(ctx context.Context) ([]Turn, error) {
	rec, _, err := store.GetJSON[[]Turn](ctx, s.b, recentKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return *rec, nil
}

// load reads every stored month at boot, when the set of them is not yet known.
func (s *Store) load(ctx context.Context) error {
	err := store.LoadMatching(ctx, s.b, Prefix, "perf-", func(key string, m *monthData) {
		if !monthRe.MatchString(m.Month) {
			log.Printf("[metrics] skipping %s: invalid month %q", key, m.Month)
			return
		}
		m.ensureMaps()
		s.months[m.Month] = m
	})
	if err != nil {
		return err
	}
	recent, err := s.loadRecent(ctx)
	if err != nil {
		return err
	}
	s.recent = capRecent(recent)
	return nil
}

// StartRefresh reconciles with the bucket every interval until ctx ends.
func (s *Store) StartRefresh(ctx context.Context, interval time.Duration) {
	safego.Tick(ctx, "metrics: refresh", interval, func() {
		if err := s.Refresh(ctx); err != nil {
			log.Printf("[metrics] refresh: %v", err)
		}
	})
}

// Runtime is the live process picture that no stored series can give.
type Runtime struct {
	UptimeSeconds int64  `json:"uptime_seconds"`
	Uptime        string `json:"uptime"`
	Goroutines    int    `json:"goroutines"`
	HeapMB        int64  `json:"heap_mb"`
	GCCycles      uint32 `json:"gc_cycles"`
}

func snapshotRuntime(since time.Time) Runtime {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	up := time.Since(since)
	return Runtime{
		UptimeSeconds: int64(up.Seconds()),
		Uptime:        up.Round(time.Second).String(),
		Goroutines:    runtime.NumGoroutine(),
		HeapMB:        int64(ms.HeapAlloc / (1 << 20)),
		GCCycles:      ms.NumGC,
	}
}
