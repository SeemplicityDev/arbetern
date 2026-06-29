// Package billing tracks the LLM token cost of every arbetern turn — Slack
// command, scheduled workflow tick, and web chat — and aggregates it on disk
// so usage and spend can be reviewed per agent, model, source, and workflow.
//
// Data lives under <BILLING_DIR>/ on the same persistent volume as workflows
// and dashboards: one rolling file per calendar month (usage-YYYY-MM.json)
// plus a recent-events feed (recent.json), written atomically. Pricing is
// synced daily from a single source of truth (PRICE_SOURCE_URL) and overridable
// via LLM_PRICE_OVERRIDES; price changes only affect future turns.
package billing

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/store"
)

// DefaultDir is used when BILLING_DIR is unset (dev only — not persisted).
const DefaultDir = "./data/billing"

// maxRecentEvents bounds the on-disk activity feed shown in the UI.
const maxRecentEvents = 500

// flushInterval is how often dirty aggregates are written back to disk.
const flushInterval = 15 * time.Second

// Source labels the entry path that produced a turn.
const (
	SourceSlack    = "slack"
	SourceWorkflow = "workflow"
	SourceChat     = "chat"
)

// Event is one billable LLM turn. Cost is derived from Model + token counts at
// record time, so callers only supply tokens and metadata. PromptTokens and
// CompletionTokens are the cumulative counts across all tool-loop rounds.
type Event struct {
	At               time.Time `json:"at"`
	Agent            string    `json:"agent"`
	Source           string    `json:"source"`
	WorkflowID       string    `json:"workflow_id,omitempty"`
	WorkflowName     string    `json:"workflow_name,omitempty"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Unpriced         bool      `json:"unpriced,omitempty"`
}

// Counts is a rolled-up tally shared by every aggregation dimension.
type Counts struct {
	Requests         int     `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Unpriced         int     `json:"unpriced"`
}

func (c *Counts) add(e Event) {
	c.Requests++
	c.PromptTokens += int64(e.PromptTokens)
	c.CompletionTokens += int64(e.CompletionTokens)
	c.TotalTokens += int64(e.TotalTokens)
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
	Month     string               `json:"month"`
	Totals    Counts               `json:"totals"`
	Days      map[string]*Counts   `json:"days"`
	Agents    map[string]*Counts   `json:"agents"`
	Models    map[string]*Counts   `json:"models"`
	Sources   map[string]*Counts   `json:"sources"`
	Workflows map[string]*wfCounts `json:"workflows"`
}

func newMonth(key string) *monthData {
	return &monthData{
		Month:     key,
		Days:      map[string]*Counts{},
		Agents:    map[string]*Counts{},
		Models:    map[string]*Counts{},
		Sources:   map[string]*Counts{},
		Workflows: map[string]*wfCounts{},
	}
}

// Store aggregates and persists usage events. Safe for concurrent use.
type Store struct {
	dir string

	mu     sync.RWMutex
	months map[string]*monthData
	recent []Event
	dirty  map[string]bool
}

// New constructs a Store rooted at dir, loading any existing aggregates.
func New(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create billing dir: %w", err)
	}
	s := &Store{
		dir:    dir,
		months: map[string]*monthData{},
		dirty:  map[string]bool{},
	}
	s.load()
	return s, nil
}

// Dir returns the root directory backing the store.
func (s *Store) Dir() string { return s.dir }

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }
func dayKey(t time.Time) string   { return t.UTC().Format("2006-01-02") }

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
		cost, ok := Cost(e.Model, e.PromptTokens, e.CompletionTokens)
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
	m.Totals.add(e)
	bucket(m.Days, dayKey(e.At)).add(e)
	bucket(m.Agents, e.Agent).add(e)
	bucket(m.Models, e.Model).add(e)
	bucket(m.Sources, e.Source).add(e)
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
	s.recent = append(s.recent, e)
	if len(s.recent) > maxRecentEvents {
		s.recent = s.recent[len(s.recent)-maxRecentEvents:]
	}
	s.dirty[mk] = true
	s.mu.Unlock()
}

// StartFlusher persists dirty aggregates on a timer until ctx is done. Call
// once at startup.
func (s *Store) StartFlusher(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				s.flush()
				return
			case <-t.C:
				s.flush()
			}
		}
	}()
}

func (s *Store) flush() {
	s.mu.Lock()
	pending := make([]string, 0, len(s.dirty))
	for k := range s.dirty {
		pending = append(pending, k)
	}
	snaps := make(map[string]*monthData, len(pending))
	for _, k := range pending {
		snaps[k] = s.months[k]
	}
	var recent []Event
	if len(pending) > 0 {
		recent = append(recent, s.recent...)
	}
	s.dirty = map[string]bool{}
	s.mu.Unlock()

	for k, m := range snaps {
		if err := store.WriteJSONAt(filepath.Join(s.dir, "usage-"+k+".json"), m); err != nil {
			log.Printf("[billing] persist %s failed: %v", k, err)
		}
	}
	if recent != nil {
		if err := store.WriteJSONAt(filepath.Join(s.dir, "recent.json"), recent); err != nil {
			log.Printf("[billing] persist recent failed: %v", err)
		}
	}
}

func (s *Store) load() {
	entries, _ := filepath.Glob(filepath.Join(s.dir, "usage-*.json"))
	for _, p := range entries {
		m, err := store.ReadJSON[monthData](p, nil)
		if err != nil || m == nil || m.Month == "" {
			continue
		}
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
		if m.Workflows == nil {
			m.Workflows = map[string]*wfCounts{}
		}
		s.months[m.Month] = m
	}
	if rec, err := store.ReadJSON[[]Event](filepath.Join(s.dir, "recent.json"), nil); err == nil && rec != nil {
		s.recent = *rec
	}
	if n := len(s.months); n > 0 {
		log.Printf("[billing] loaded %d month(s) of usage from %s", n, s.dir)
	}
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
	Days       int         `json:"days"`
	Since      string      `json:"since"`
	Totals     Counts      `json:"totals"`
	ByAgent    []Row       `json:"by_agent"`
	ByModel    []Row       `json:"by_model"`
	BySource   []Row       `json:"by_source"`
	ByWorkflow []Row       `json:"by_workflow"`
	Daily      []Row       `json:"daily"`
	Recent     []Event     `json:"recent"`
	Months     []string    `json:"months"`
	Pricing    PriceSource `json:"pricing"`
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
	out.ByWorkflow = wfRows(wfs)
	out.Daily = dailyRows(daily)
	out.Recent = recentSince(s.recent, since)
	out.Pricing = SourceInfo()
	return out
}

func (c *Counts) addCounts(o *Counts) {
	c.Requests += o.Requests
	c.PromptTokens += o.PromptTokens
	c.CompletionTokens += o.CompletionTokens
	c.TotalTokens += o.TotalTokens
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
