// Package billing tracks the LLM token cost of every arbetern turn — Slack
// command, scheduled workflow tick, and web chat — and aggregates it so usage
// and spend can be reviewed per agent, model, source, and workflow.
//
// Data lives under Prefix in the state bucket: one rolling object per calendar
// month (usage-YYYY-MM.json) plus a recent-events feed (recent.json). Events
// are buffered in memory and merged into the stored aggregates with
// conditional writes, so several replicas can record concurrently without
// losing each other's turns. Pricing is synced daily from a single source of
// truth (PRICE_SOURCE_URL) and overridable via LLM_PRICE_OVERRIDES; price
// changes only affect future turns.
package billing

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

// Prefix is the object prefix the ledger is stored under.
const Prefix = "billing/"

// maxRecentEvents bounds the stored activity feed shown in the UI.
const maxRecentEvents = 500

// flushInterval is how often buffered events are merged into the bucket.
const flushInterval = 15 * time.Second

const (
	recentKey        = Prefix + "recent.json"
	maxMergeAttempts = 4
)

// Source labels the entry path that produced a turn.
const (
	SourceSlack     = "slack"
	SourceWorkflow  = "workflow"
	SourceChat      = "chat"
	SourceDashboard = "dashboard"
)

// Event is one billable LLM turn. Cost is derived from Model + token counts at
// record time, so callers only supply tokens and metadata. PromptTokens and
// CompletionTokens are the cumulative counts across all tool-loop rounds.
type Event struct {
	At                 time.Time `json:"at"`
	Agent              string    `json:"agent"`
	Source             string    `json:"source"`
	UserID             string    `json:"user_id,omitempty"`
	WorkflowID         string    `json:"workflow_id,omitempty"`
	WorkflowName       string    `json:"workflow_name,omitempty"`
	Model              string    `json:"model"`
	PromptTokens       int       `json:"prompt_tokens"`
	CachedPromptTokens int       `json:"cached_prompt_tokens,omitempty"`
	CacheWriteTokens   int       `json:"cache_write_tokens,omitempty"`
	CompletionTokens   int       `json:"completion_tokens"`
	TotalTokens        int       `json:"total_tokens"`
	// Compression{Input,Saved}Tokens track Headroom savings for this turn; the
	// saved-% is derived as CompressionSavedTokens / CompressionInputTokens.
	CompressionInputTokens int     `json:"compression_input_tokens,omitempty"`
	CompressionSavedTokens int     `json:"compression_saved_tokens,omitempty"`
	CostUSD                float64 `json:"cost_usd"`
	Unpriced               bool    `json:"unpriced,omitempty"`
}

// Counts is a rolled-up tally shared by every aggregation dimension.
type Counts struct {
	Requests           int   `json:"requests"`
	PromptTokens       int64 `json:"prompt_tokens"`
	CachedPromptTokens int64 `json:"cached_prompt_tokens"`
	CacheWriteTokens   int64 `json:"cache_write_tokens"`
	CompletionTokens   int64 `json:"completion_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	// Headroom compression rollup; saved-% = CompressionSavedTokens / CompressionInputTokens.
	CompressionInputTokens int64   `json:"compression_input_tokens"`
	CompressionSavedTokens int64   `json:"compression_saved_tokens"`
	CostUSD                float64 `json:"cost_usd"`
	Unpriced               int     `json:"unpriced"`
}

func (c *Counts) add(e Event) {
	c.Requests++
	c.PromptTokens += int64(e.PromptTokens)
	c.CachedPromptTokens += int64(e.CachedPromptTokens)
	c.CacheWriteTokens += int64(e.CacheWriteTokens)
	c.CompletionTokens += int64(e.CompletionTokens)
	c.TotalTokens += int64(e.TotalTokens)
	c.CompressionInputTokens += int64(e.CompressionInputTokens)
	c.CompressionSavedTokens += int64(e.CompressionSavedTokens)
	c.CostUSD += e.CostUSD
	if e.Unpriced {
		c.Unpriced++
	}
}

// wfCounts is a per-workflow tally carrying display metadata.
type wfCounts struct {
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Counts        // embedded; flattened in JSON
}

// monthData is the persisted shape of one calendar month.
type monthData struct {
	Month      string               `json:"month"`
	Totals     Counts               `json:"totals"`
	Days       map[string]*Counts   `json:"days"`
	Agents     map[string]*Counts   `json:"agents"`
	Models     map[string]*Counts   `json:"models"`
	Sources    map[string]*Counts   `json:"sources"`
	Users      map[string]*Counts   `json:"users"`
	UserAgents map[string]*Counts   `json:"user_agents"`
	Workflows  map[string]*wfCounts `json:"workflows"`
}

func newMonth(key string) *monthData {
	m := &monthData{Month: key}
	m.ensureMaps()
	return m
}

func (m *monthData) ensureMaps() {
	if m.Days == nil {
		m.Days = map[string]*Counts{}
	}
	if m.Agents == nil {
		m.Agents = map[string]*Counts{}
	}
	if m.Models == nil {
		m.Models = map[string]*Counts{}
	}
	if m.Sources == nil {
		m.Sources = map[string]*Counts{}
	}
	if m.Users == nil {
		m.Users = map[string]*Counts{}
	}
	if m.UserAgents == nil {
		m.UserAgents = map[string]*Counts{}
	}
	if m.Workflows == nil {
		m.Workflows = map[string]*wfCounts{}
	}
}

// add tallies one event into every dimension of the month.
func (m *monthData) add(e Event) {
	m.Totals.add(e)
	bucket(m.Days, dayKey(e.At)).add(e)
	bucket(m.Agents, e.Agent).add(e)
	bucket(m.Models, e.Model).add(e)
	bucket(m.Sources, e.Source).add(e)
	if e.UserID != "" {
		bucket(m.Users, e.UserID).add(e)
		bucket(m.UserAgents, e.UserID+userAgentSep+e.Agent).add(e)
	}
	if e.WorkflowID != "" {
		key := e.Agent + "/" + e.WorkflowID
		wf := m.Workflows[key]
		if wf == nil {
			wf = &wfCounts{Agent: e.Agent, Name: e.WorkflowName}
			m.Workflows[key] = wf
		}
		if e.WorkflowName != "" {
			wf.Name = e.WorkflowName
		}
		wf.add(e)
	}
}

func monthObjectKey(month string) string { return Prefix + "usage-" + month + ".json" }

// Store aggregates usage events in memory and merges them into the bucket.
// Safe for concurrent use.
type Store struct {
	b *store.Backend

	mu      sync.RWMutex
	months  map[string]*monthData
	recent  []Event
	pending map[string][]Event

	resolveName func(userID string) string
}

// SetUserNameResolver installs a best-effort user ID → display name lookup
// used to label users in summaries. Nil or "" results leave the raw ID.
func (s *Store) SetUserNameResolver(fn func(userID string) string) {
	s.mu.Lock()
	s.resolveName = fn
	s.mu.Unlock()
}

// New constructs a Store over b, loading the stored aggregates.
func New(ctx context.Context, b *store.Backend) (*Store, error) {
	s := &Store{
		b:       b,
		months:  map[string]*monthData{},
		pending: map[string][]Event{},
	}
	if err := s.load(ctx); err != nil {
		return nil, fmt.Errorf("load billing: %w", err)
	}
	return s, nil
}

// Months is the number of calendar months with recorded usage.
func (s *Store) Months() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.months)
}

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }
func dayKey(t time.Time) string   { return t.UTC().Format("2006-01-02") }

// monthRe validates a month key read back from the bucket. Keys become the
// variable part of the usage-<month>.json object name on the next flush, so a
// hand-edited document must not be able to steer that write elsewhere.
var monthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)

// userAgentSep joins a user ID and an agent ID into one UserAgents key. Agent
// IDs never contain it, so the key splits unambiguously at its last occurrence.
const userAgentSep = "|"

func bucket(m map[string]*Counts, k string) *Counts {
	c := m[k]
	if c == nil {
		c = &Counts{}
		m[k] = c
	}
	return c
}

// Record tallies one turn. Cost is computed from the model price table unless
// the caller already set CostUSD. Empty/zero-token events are ignored so the
// feed only reflects real spend. Never returns an error: billing must never
// break an agent turn.
func (s *Store) Record(e Event) {
	if e.TotalTokens == 0 && e.PromptTokens == 0 && e.CompletionTokens == 0 {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Source == "" {
		e.Source = SourceSlack
	}
	if e.CostUSD == 0 {
		cost, ok := Cost(e.Model, e.PromptTokens, e.CachedPromptTokens, e.CacheWriteTokens, e.CompletionTokens)
		e.CostUSD = cost
		e.Unpriced = !ok
	}

	mk := monthKey(e.At)
	s.mu.Lock()
	m := s.months[mk]
	if m == nil {
		m = newMonth(mk)
		s.months[mk] = m
	}
	m.add(e)
	s.recent = append(s.recent, e)
	if len(s.recent) > maxRecentEvents {
		s.recent = s.recent[len(s.recent)-maxRecentEvents:]
	}
	s.pending[mk] = append(s.pending[mk], e)
	s.mu.Unlock()
}

// StartFlusher merges buffered events into the bucket on a timer until stop is
// closed, then once more. The returned channel closes after that final flush.
func (s *Store) StartFlusher(stop <-chan struct{}) <-chan struct{} {
	return safego.Every("billing: flush", flushInterval, stop, func() { s.flush(context.Background()) })
}

func (s *Store) flush(ctx context.Context) {
	s.mu.Lock()
	pending := s.pending
	s.pending = map[string][]Event{}
	s.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	var flushed []Event
	for mk, evs := range pending {
		if err := s.mergeMonth(ctx, mk, evs); err != nil {
			log.Printf("[billing] persist %s failed: %v", mk, err)
			s.mu.Lock()
			s.pending[mk] = append(evs, s.pending[mk]...)
			s.mu.Unlock()
			continue
		}
		flushed = append(flushed, evs...)
	}
	if len(flushed) > 0 {
		if err := s.mergeRecent(ctx, flushed); err != nil {
			log.Printf("[billing] persist recent failed: %v", err)
		}
	}
}

// mergeMonth folds evs into the stored month and adopts the merged result as
// the local view, so turns recorded by other replicas show up here too.
func (s *Store) mergeMonth(ctx context.Context, mk string, evs []Event) error {
	merged, err := store.Merge(ctx, s.b, monthObjectKey(mk), maxMergeAttempts, func(m *monthData) {
		m.Month = mk
		m.ensureMaps()
		for _, e := range evs {
			m.add(e)
		}
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, e := range s.pending[mk] {
		merged.add(e)
	}
	s.months[mk] = merged
	s.mu.Unlock()
	return nil
}

// mergeRecent appends added to the stored activity feed and adopts the merged
// feed locally.
func (s *Store) mergeRecent(ctx context.Context, added []Event) error {
	merged, err := store.Merge(ctx, s.b, recentKey, maxMergeAttempts, func(list *[]Event) {
		*list = capRecent(append(*list, added...))
	})
	if err != nil {
		return err
	}
	list := *merged
	s.mu.Lock()
	for _, evs := range s.pending {
		list = append(list, evs...)
	}
	s.recent = capRecent(list)
	s.mu.Unlock()
	return nil
}

func capRecent(list []Event) []Event {
	sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	if len(list) > maxRecentEvents {
		list = list[len(list)-maxRecentEvents:]
	}
	return list
}

func (s *Store) load(ctx context.Context) error {
	err := store.LoadMatching(ctx, s.b, Prefix, "usage-", func(key string, m *monthData) {
		if !monthRe.MatchString(m.Month) {
			log.Printf("[billing] skipping %s: invalid month %q", key, m.Month)
			return
		}
		m.ensureMaps()
		s.months[m.Month] = m
	})
	if err != nil {
		return err
	}
	rec, _, err := store.GetJSON[[]Event](ctx, s.b, recentKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	default:
		s.recent = *rec
	}
	if n := len(s.months); n > 0 {
		log.Printf("[billing] loaded %d month(s) of usage from %s", n, s.b)
	}
	return nil
}

// Row pairs a key with its tally for sorted JSON output.
type Row struct {
	Key    string `json:"key"`
	Name   string `json:"name,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Counts Counts `json:"counts"`
}

// Summary is the aggregated view served to the UI for a trailing window.
type Summary struct {
	Days        int         `json:"days"`
	Since       string      `json:"since"`
	Totals      Counts      `json:"totals"`
	ByAgent     []Row       `json:"by_agent"`
	ByModel     []Row       `json:"by_model"`
	BySource    []Row       `json:"by_source"`
	ByUser      []Row       `json:"by_user"`
	ByUserAgent []Row       `json:"by_user_agent"`
	ByWorkflow  []Row       `json:"by_workflow"`
	Daily       []Row       `json:"daily"`
	Recent      []Event     `json:"recent"`
	Months      []string    `json:"months"`
	Pricing     PriceSource `json:"pricing"`
}

// Summarize aggregates the trailing `days` (UTC) into a single view. days<=0
// means the full retained history.
func (s *Store) Summarize(days int) Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var since time.Time
	if days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	}
	out := Summary{Days: days}
	if !since.IsZero() {
		out.Since = since.Format("2006-01-02")
	}
	agents := map[string]*Counts{}
	models := map[string]*Counts{}
	sources := map[string]*Counts{}
	users := map[string]*Counts{}
	userAgents := map[string]*Counts{}
	wfs := map[string]*wfCounts{}
	daily := map[string]*Counts{}

	for mk, m := range s.months {
		out.Months = append(out.Months, mk)
		for d, c := range m.Days {
			if !since.IsZero() && d < out.Since {
				continue
			}
			out.Totals.addCounts(c)
			bucket(daily, d).addCounts(c)
		}
		merge(agents, m.Agents)
		merge(models, m.Models)
		merge(sources, m.Sources)
		merge(users, m.Users)
		merge(userAgents, m.UserAgents)
		for k, wf := range m.Workflows {
			dst := wfs[k]
			if dst == nil {
				dst = &wfCounts{Agent: wf.Agent, Name: wf.Name}
				wfs[k] = dst
			}
			dst.addCounts(&wf.Counts)
		}
	}
	sort.Strings(out.Months)
	out.ByAgent = rows(agents)
	out.ByModel = rows(models)
	out.BySource = rows(sources)
	out.ByUser = rows(users)
	out.ByUserAgent = userAgentRows(userAgents)
	out.ByWorkflow = wfRows(wfs)
	s.labelUsers(out.ByUser)
	s.labelUsers(out.ByUserAgent)
	out.Daily = dailyRows(daily)
	out.Recent = recentSince(s.recent, since)
	out.Pricing = SourceInfo()
	return out
}

func (c *Counts) addCounts(o *Counts) {
	c.Requests += o.Requests
	c.PromptTokens += o.PromptTokens
	c.CachedPromptTokens += o.CachedPromptTokens
	c.CacheWriteTokens += o.CacheWriteTokens
	c.CompletionTokens += o.CompletionTokens
	c.TotalTokens += o.TotalTokens
	c.CompressionInputTokens += o.CompressionInputTokens
	c.CompressionSavedTokens += o.CompressionSavedTokens
	c.CostUSD += o.CostUSD
	c.Unpriced += o.Unpriced
}

// merge sums per-month dimension buckets. Day-level filtering applies to
// totals/daily; the agent/model/source rollups span every retained month that
// has activity in the window — accurate for the common "all" and full-month
// windows, generous for sub-month ones.
func merge(dst, src map[string]*Counts) {
	for k, c := range src {
		bucket(dst, k).addCounts(c)
	}
}

func rows(m map[string]*Counts) []Row {
	out := make([]Row, 0, len(m))
	for k, c := range m {
		if k == "" {
			k = "(unknown)"
		}
		out = append(out, Row{Key: k, Counts: *c})
	}
	sortRows(out)
	return out
}

func userAgentRows(m map[string]*Counts) []Row {
	out := make([]Row, 0, len(m))
	for k, c := range m {
		user, agent := k, ""
		if i := strings.LastIndex(k, userAgentSep); i >= 0 {
			user, agent = k[:i], k[i+len(userAgentSep):]
		}
		out = append(out, Row{Key: user, Agent: agent, Counts: *c})
	}
	sortRows(out)
	return out
}

// labelUsers fills Row.Name from the configured resolver. Called under the
// read lock; the resolver is expected to cache and never block for long.
func (s *Store) labelUsers(rows []Row) {
	if s.resolveName == nil {
		return
	}
	for i := range rows {
		if name := s.resolveName(rows[i].Key); name != "" && name != rows[i].Key {
			rows[i].Name = name
		}
	}
}

func wfRows(m map[string]*wfCounts) []Row {
	out := make([]Row, 0, len(m))
	for k, c := range m {
		out = append(out, Row{Key: k, Name: c.Name, Agent: c.Agent, Counts: c.Counts})
	}
	sortRows(out)
	return out
}

func dailyRows(m map[string]*Counts) []Row {
	out := make([]Row, 0, len(m))
	for k, c := range m {
		out = append(out, Row{Key: k, Counts: *c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func sortRows(out []Row) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Counts.CostUSD != out[j].Counts.CostUSD {
			return out[i].Counts.CostUSD > out[j].Counts.CostUSD
		}
		return out[i].Counts.TotalTokens > out[j].Counts.TotalTokens
	})
}

func recentSince(all []Event, since time.Time) []Event {
	out := make([]Event, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if !since.IsZero() && all[i].At.Before(since) {
			continue
		}
		out = append(out, all[i])
		if len(out) >= 100 {
			break
		}
	}
	return out
}
