// Package dashboards provides a lightweight dashboard engine for arbetern.
//
// Each dashboard is a small JSON descriptor stored at
//
//	<DASHBOARDS_DIR>/<agent>/<dashboard-id>.json
//
// It owns a list of "data sources" (pre-defined read-only integration queries)
// and a sync interval. A goroutine per dashboard periodically re-executes the
// sources and writes the latest results back to the same JSON file, so the
// serving layer can read the file and render a fresh view without touching
// the integrations directly.
//
// Dashboards can be created, listed, and deleted by LLM tools; they can also
// be viewed through a generated HTML page at /<agent>/dashboard/<id>.
package dashboards

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDir is used when DASHBOARDS_DIR is unset.
	DefaultDir = "./data/dashboards"

	// MinSyncInterval is the shortest allowed sync interval.
	MinSyncInterval = 30 * time.Second
	// MaxSyncInterval caps runaway intervals at one day.
	MaxSyncInterval = 24 * time.Hour
	// DefaultSyncInterval is used when the caller omits or provides an invalid value.
	DefaultSyncInterval = 5 * time.Minute
)

// DataSource is a single read-only query executed on each sync.
// Type identifies which integration is queried; Args carries the parameters.
type DataSource struct {
	Type string         `json:"type"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// SourceResult is the outcome of executing a DataSource on one sync cycle.
type SourceResult struct {
	FetchedAt  string `json:"fetched_at"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	// Content is free-form — either structured data (parsed JSON-compatible)
	// or a plain string from text-returning integration helpers.
	Content any `json:"content,omitempty"`
}

// Dashboard is the on-disk descriptor.
type Dashboard struct {
	ID          string `json:"id"`
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	ShortName   string `json:"short_name"`
	Description string `json:"description,omitempty"`
	// Kind distinguishes a plain sources dashboard (empty / "sources") from an
	// account-health dashboard ("account"). The HTML viewer switches layout based
	// on this field.
	Kind         string                  `json:"kind,omitempty"`
	SyncInterval string                  `json:"sync_interval"`
	Sources      []DataSource            `json:"sources"`
	CreatedBy    string                  `json:"created_by,omitempty"`
	CreatedAt    string                  `json:"created_at"`
	LastSync     string                  `json:"last_sync,omitempty"`
	LastError    string                  `json:"last_error,omitempty"`
	Data         map[string]SourceResult `json:"data,omitempty"`
	// Account is populated for Kind=="account" dashboards and drives the health
	// summary panel (score badge, risks, actions, signal bars).
	Account *AccountSummary `json:"account,omitempty"`
}

// AccountSummary captures the health-score output for an account dashboard.
type AccountSummary struct {
	AccountName string   `json:"account_name"`
	AccountID   string   `json:"account_id,omitempty"`
	Score       int      `json:"score"`
	Band        string   `json:"band"` // green | yellow | orange | red
	Risks       []string `json:"risks,omitempty"`
	Actions     []string `json:"actions,omitempty"`
	Signals     []Signal `json:"signals,omitempty"`
	GeneratedAt string   `json:"generated_at"`
}

// Signal is one row of the health breakdown.
type Signal struct {
	Name    string   `json:"name"`
	Weight  int      `json:"weight"`
	Score   int      `json:"score"`
	Reasons []string `json:"reasons,omitempty"`
}

// ViewURL returns the path to the HTML view for this dashboard.
func (d *Dashboard) ViewURL() string {
	return fmt.Sprintf("/%s/dashboard/%s", d.Agent, d.ID)
}

// interval parses SyncInterval, returning DefaultSyncInterval on failure.
func (d *Dashboard) interval() time.Duration {
	dur, err := time.ParseDuration(d.SyncInterval)
	if err != nil || dur < MinSyncInterval {
		return DefaultSyncInterval
	}
	if dur > MaxSyncInterval {
		return MaxSyncInterval
	}
	return dur
}

// Executor runs a single DataSource and returns its content (or an error).
// Implementations live in executor.go and talk to integration clients directly.
type Executor interface {
	Execute(ctx context.Context, src DataSource) (any, error)
}

// AccountRebuilder is an optional Executor extension for Kind="account"
// dashboards. Account dashboards are built via a dedicated fan-out
// (see BuildAccountDashboard) and their Sources carry no Args — the generic
// per-source Execute path would fail with "requires args.X" on every sync.
// When the Executor implements this interface, syncOne routes account
// dashboards through RebuildAccount instead of iterating Sources.
type AccountRebuilder interface {
	RebuildAccount(ctx context.Context, d *Dashboard) (*Dashboard, error)
}

// runner owns the goroutine driving one dashboard's sync loop.
type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Registry is the thread-safe in-memory index of dashboards plus their goroutines.
type Registry struct {
	mu       sync.RWMutex
	dir      string
	executor Executor
	items    map[string]*Dashboard // key: agent/id
	runners  map[string]*runner    // key: agent/id
}

// New creates a Registry rooted at dir. It creates the directory if missing.
func New(dir string, exec Executor) (*Registry, error) {
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create dashboards dir: %w", err)
	}
	r := &Registry{
		dir:      dir,
		executor: exec,
		items:    make(map[string]*Dashboard),
		runners:  make(map[string]*runner),
	}
	return r, nil
}

// Dir returns the root directory managed by the registry.
func (r *Registry) Dir() string { return r.dir }

func key(agent, id string) string { return agent + "/" + id }

// agentValidRe allows only lowercase/letters/digits/dashes for agent IDs.
var agentValidRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// idValidRe constrains dashboard IDs to a safe URL-friendly alphabet.
var idValidRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// LoadAll scans the dashboards directory and kicks off sync goroutines.
// Invalid files are logged and skipped; they do not prevent startup.
func (r *Registry) LoadAll(ctx context.Context) error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("read dashboards dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		agent := e.Name()
		if !agentValidRe.MatchString(agent) {
			continue
		}
		agentDir := filepath.Join(r.dir, agent)
		files, err := os.ReadDir(agentDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			path := filepath.Join(agentDir, f.Name())
			d, err := readFile(path)
			if err != nil {
				log.Printf("[dashboards] skipping %s: %v", path, err)
				continue
			}
			r.mu.Lock()
			r.items[key(d.Agent, d.ID)] = d
			r.mu.Unlock()
			r.startRunner(ctx, d)
			log.Printf("[dashboards] loaded %s/%s (%q, every %s)", d.Agent, d.ID, d.Name, d.interval())
		}
	}
	return nil
}

// Create validates, persists, and starts a new dashboard.
// It returns the stored dashboard (with generated ID and timestamps).
func (r *Registry) Create(ctx context.Context, agent, createdBy string, name, shortName, description, syncInterval string, sources []DataSource) (*Dashboard, error) {
	if !agentValidRe.MatchString(agent) {
		return nil, fmt.Errorf("invalid agent id %q", agent)
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("dashboard name is required")
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("at least one source is required")
	}
	if _, err := time.ParseDuration(syncInterval); err != nil {
		syncInterval = DefaultSyncInterval.String()
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if shortName == "" {
		shortName = slugify(name)
	}
	d := &Dashboard{
		ID:           id,
		Agent:        agent,
		Name:         name,
		ShortName:    shortName,
		Description:  description,
		SyncInterval: syncInterval,
		Sources:      sources,
		CreatedBy:    createdBy,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := r.persist(d); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.items[key(agent, id)] = d
	r.mu.Unlock()
	r.startRunner(ctx, d)
	log.Printf("[dashboards] created %s/%s (%q, every %s)", agent, id, name, d.interval())
	return d, nil
}

// Upsert stores a fully-formed Dashboard under its own ID without starting a
// background sync goroutine. Intended for on-demand dashboards (e.g. account
// health snapshots) where the caller, not a cron ticker, drives refreshes.
//
// Any existing runner for the same (agent, id) is stopped so the new snapshot
// becomes the sole source of truth. The dashboard is written to disk atomically.
func (r *Registry) Upsert(d *Dashboard) error {
	if d == nil {
		return fmt.Errorf("dashboard is nil")
	}
	if !agentValidRe.MatchString(d.Agent) {
		return fmt.Errorf("invalid agent id %q", d.Agent)
	}
	if !idValidRe.MatchString(d.ID) {
		return fmt.Errorf("invalid dashboard id %q", d.ID)
	}
	if d.CreatedAt == "" {
		d.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if d.SyncInterval == "" {
		d.SyncInterval = DefaultSyncInterval.String()
	}

	r.mu.Lock()
	if run, ok := r.runners[key(d.Agent, d.ID)]; ok {
		run.cancel()
		delete(r.runners, key(d.Agent, d.ID))
	}
	r.items[key(d.Agent, d.ID)] = d
	r.mu.Unlock()

	if err := r.persist(d); err != nil {
		return err
	}
	log.Printf("[dashboards] upsert %s/%s (%q)", d.Agent, d.ID, d.Name)
	return nil
}

// Get returns a copy of the stored dashboard by agent+id, or (nil,false) if missing.
func (r *Registry) Get(agent, id string) (*Dashboard, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.items[key(agent, id)]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// List returns all dashboards, optionally filtered by agent (empty = all),
// sorted by creation time ascending.
func (r *Registry) List(agent string) []*Dashboard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Dashboard, 0, len(r.items))
	for _, d := range r.items {
		if agent != "" && d.Agent != agent {
			continue
		}
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Delete stops the sync goroutine, removes the file, and deletes from memory.
func (r *Registry) Delete(agent, id string) error {
	r.mu.Lock()
	d, ok := r.items[key(agent, id)]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("dashboard %s/%s not found", agent, id)
	}
	delete(r.items, key(agent, id))
	run := r.runners[key(agent, id)]
	delete(r.runners, key(agent, id))
	r.mu.Unlock()

	if run != nil {
		run.cancel()
		// Best-effort wait so a concurrent sync finishes before we remove the file.
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
		}
	}
	path := r.pathFor(d)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove dashboard file: %w", err)
	}
	log.Printf("[dashboards] deleted %s/%s", agent, id)
	return nil
}

// StopAll cancels every sync goroutine. Safe to call at shutdown.
func (r *Registry) StopAll() {
	r.mu.Lock()
	runs := r.runners
	r.runners = make(map[string]*runner)
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
}

// startRunner launches the sync goroutine for d. Replaces any existing runner.
func (r *Registry) startRunner(parent context.Context, d *Dashboard) {
	r.mu.Lock()
	if old, ok := r.runners[key(d.Agent, d.ID)]; ok {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &runner{cancel: cancel, done: make(chan struct{})}
	r.runners[key(d.Agent, d.ID)] = run
	r.mu.Unlock()

	agent := d.Agent
	id := d.ID
	interval := d.interval()

	go func() {
		defer close(run.done)
		// Kick off an immediate sync so the dashboard has data on first view.
		r.syncOne(ctx, agent, id)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.syncOne(ctx, agent, id)
			}
		}
	}()
}

// syncOne re-executes all sources for a dashboard and persists the updated file.
func (r *Registry) syncOne(ctx context.Context, agent, id string) {
	r.mu.RLock()
	orig, ok := r.items[key(agent, id)]
	r.mu.RUnlock()
	if !ok {
		return
	}
	// Work on a copy so concurrent readers see a stable snapshot.
	d := *orig

	// Account dashboards have their own fan-out + health scoring — their
	// Sources carry no Args, so running them through the generic executor
	// would just yield "requires args.X" on every sync. Delegate to the
	// rebuilder which re-resolves the account and recomputes the score.
	if d.Kind == "account" {
		if rb, ok := r.executor.(AccountRebuilder); ok {
			refreshed, err := rb.RebuildAccount(ctx, &d)
			if err != nil {
				d.LastError = err.Error()
				d.LastSync = time.Now().UTC().Format(time.RFC3339)
				r.mu.Lock()
				r.items[key(agent, id)] = &d
				r.mu.Unlock()
				if perr := r.persist(&d); perr != nil {
					log.Printf("[dashboards] persist %s/%s failed: %v", agent, id, perr)
				}
				return
			}
			// Preserve stable identity fields across rebuilds.
			refreshed.ID = d.ID
			refreshed.Agent = d.Agent
			refreshed.ShortName = d.ShortName
			refreshed.CreatedBy = d.CreatedBy
			refreshed.CreatedAt = d.CreatedAt
			refreshed.SyncInterval = d.SyncInterval
			refreshed.LastError = ""
			r.mu.Lock()
			r.items[key(agent, id)] = refreshed
			r.mu.Unlock()
			if err := r.persist(refreshed); err != nil {
				log.Printf("[dashboards] persist %s/%s failed: %v", agent, id, err)
			}
			return
		}
	}

	data := make(map[string]SourceResult, len(d.Sources))
	for i, src := range d.Sources {
		start := time.Now()
		label := src.Name
		if label == "" {
			label = fmt.Sprintf("%s_%d", src.Type, i)
		}
		content, err := r.executor.Execute(ctx, src)
		res := SourceResult{
			FetchedAt:  time.Now().UTC().Format(time.RFC3339),
			DurationMS: time.Since(start).Milliseconds(),
			Content:    content,
		}
		if err != nil {
			res.Error = err.Error()
			res.Content = nil
		}
		data[label] = res
		// Respect cancellation between sources.
		if ctx.Err() != nil {
			return
		}
	}
	d.Data = data
	d.LastSync = time.Now().UTC().Format(time.RFC3339)
	d.LastError = ""

	r.mu.Lock()
	r.items[key(agent, id)] = &d
	r.mu.Unlock()

	if err := r.persist(&d); err != nil {
		log.Printf("[dashboards] persist %s/%s failed: %v", agent, id, err)
	}
}

// persist atomically writes a dashboard to disk.
func (r *Registry) persist(d *Dashboard) error {
	dir := filepath.Join(r.dir, d.Agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := r.pathFor(d)
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *Registry) pathFor(d *Dashboard) string {
	return filepath.Join(r.dir, d.Agent, d.ID+".json")
}

func readFile(path string) (*Dashboard, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.ID == "" || d.Agent == "" || !idValidRe.MatchString(d.ID) || !agentValidRe.MatchString(d.Agent) {
		return nil, fmt.Errorf("invalid dashboard descriptor")
	}
	return &d, nil
}

// newID generates an 8-byte URL-safe hex identifier.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "dashboard"
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}
