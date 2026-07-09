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

	"github.com/justmike1/arbetern/internal/store"
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

	// KindPrompt marks a prompt-driven dashboard. Instead of a fixed list of
	// deterministic Sources, a prompt dashboard owns a natural-language Prompt
	// that is run through the owning agent's LLM tool-loop — like a workflow —
	// and renders the model's Markdown output.
	//
	// A prompt dashboard descriptor synced from GitOps is a TEMPLATE. When its
	// Prompt contains {{VAR}} placeholders, the template is not rendered itself;
	// the UI shows a form (one field per detected input) and each submission
	// renders a per-input INSTANCE (TemplateID set, Inputs populated). A prompt
	// dashboard with NO placeholders is self-contained and renders in place. Both
	// instances and placeholder-free templates auto-refresh on their sync ticker.
	KindPrompt = "prompt"
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
	// Kind distinguishes a plain sources dashboard (empty / "sources") from a
	// prompt-driven dashboard (KindPrompt). The HTML viewer switches layout
	// based on this field.
	Kind         string                  `json:"kind,omitempty"`
	SyncInterval string                  `json:"sync_interval"`
	Sources      []DataSource            `json:"sources"`
	CreatedBy    string                  `json:"created_by,omitempty"`
	CreatedAt    string                  `json:"created_at"`
	LastSync     string                  `json:"last_sync,omitempty"`
	LastError    string                  `json:"last_error,omitempty"`
	Data         map[string]SourceResult `json:"data,omitempty"`
	// Prompt is the natural-language instruction for Kind==KindPrompt
	// dashboards. It may contain {{VAR}} placeholders (declared inputs)
	// substituted at render time. Ignored for sources dashboards.
	Prompt string `json:"prompt,omitempty"`
	// TemplateID is set on a rendered INSTANCE of a prompt template and points
	// back to the template's ID. Empty on the template itself.
	TemplateID string `json:"template_id,omitempty"`
	// Inputs holds the resolved {{VAR}} values for a rendered instance
	// (var name -> value). Empty on a template.
	Inputs map[string]string `json:"inputs,omitempty"`
	// Markdown holds the model's rendered report for a prompt-dashboard
	// instance (or a placeholder-free prompt dashboard). The HTML viewer
	// renders it when Kind==KindPrompt.
	Markdown string `json:"markdown,omitempty"`
	// Source identifies how the dashboard was created. "" / "ui" =
	// interactive; "gitops" = managed by the GitOps poller.
	Source string `json:"source,omitempty"`
	// SourceRef is an informational pointer to the upstream definition
	// for non-ui sources (e.g. "<owner>/<repo>@<branch>:<path>").
	SourceRef string `json:"source_ref,omitempty"`
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

// PromptRenderer runs a fully-substituted prompt through the owning agent's
// LLM tool-loop and returns the Markdown report. It is implemented outside
// this package (in main, over the router map) because rendering needs the
// full agent tool-loop, and installed post-construction via SetPromptRenderer
// (mirrors how the workflows registry receives its Executor).
type PromptRenderer interface {
	RenderPrompt(ctx context.Context, agent, dashboardID, dashboardName, prompt string) (string, error)
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
	renderer PromptRenderer
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

// SetPromptRenderer installs the prompt renderer post-construction. It is
// wired after the router map is built (which depends on the registry), so a
// prompt dashboard loaded at boot only renders once this is set.
func (r *Registry) SetPromptRenderer(pr PromptRenderer) {
	r.mu.Lock()
	r.renderer = pr
	r.mu.Unlock()
}

func key(agent, id string) string { return agent + "/" + id }

// Re-exported so the http layer (in this same package) can keep the short
// names. The actual rules live in internal/store.
var (
	agentValidRe = store.AgentRe
	idValidRe    = store.IDRe
)

// LoadAll scans the dashboards directory and loads each dashboard into
// memory. It does NOT start sync goroutines — call StartAll afterwards to
// launch them. Invalid files are logged and skipped; they do not prevent
// startup.
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
			log.Printf("[dashboards] loaded %s/%s (%q, every %s)", d.Agent, d.ID, d.Name, d.interval())
		}
	}
	return nil
}

// StartAll launches a sync goroutine for every loaded dashboard WITHOUT
// firing an immediate sync. Intended as the server-boot entry point: on
// startup we just want the scheduled ticker to begin, not to blast every
// upstream (Jira, Datadog, GitHub, Chorus, …) the moment the process comes
// up. Dashboards that are explicitly refreshed via "Refresh now" or via
// Create will still sync immediately — only the boot path is lazy.
//
// Account-kind dashboards, which have no ticker by design, are started the
// same way startRunner handles them (no-op runner) so StopAll still works.
func (r *Registry) StartAll(ctx context.Context) {
	r.mu.RLock()
	list := make([]*Dashboard, 0, len(r.items))
	for _, d := range r.items {
		cp := *d
		list = append(list, &cp)
	}
	r.mu.RUnlock()
	for _, d := range list {
		r.startRunner(ctx, d, false)
	}
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
	r.startRunner(ctx, d, true)
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

// UpsertSpec is the declarative shape used by id-stable callers (e.g. the
// GitOps syncer). All fields replace the stored value; runtime bookkeeping
// (Data, LastSync, LastError, Account) is preserved across updates.
type UpsertSpec struct {
	ID           string
	Agent        string
	Name         string
	ShortName    string // slugified from Name when empty
	Description  string
	Kind         string
	SyncInterval string
	Sources      []DataSource
	Prompt       string // required when Kind==KindPrompt
	CreatedBy    string // applied only on first create
	Source       string
	SourceRef    string
}

// UpsertFromSpec creates a dashboard at spec.ID or replaces the existing
// one in place. Returns changed=false when the spec matches the stored copy.
// The sync runner is restarted only when the schedule or sources actually
// change so a no-op tick does not bounce live syncs.
func (r *Registry) UpsertFromSpec(ctx context.Context, spec UpsertSpec) (d *Dashboard, changed bool, err error) {
	if !agentValidRe.MatchString(spec.Agent) {
		return nil, false, fmt.Errorf("invalid agent id %q", spec.Agent)
	}
	if !idValidRe.MatchString(spec.ID) {
		return nil, false, fmt.Errorf("invalid dashboard id %q", spec.ID)
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, false, fmt.Errorf("dashboard name is required")
	}
	if spec.Kind == KindPrompt {
		if strings.TrimSpace(spec.Prompt) == "" {
			return nil, false, fmt.Errorf("prompt dashboard requires a prompt")
		}
	} else if len(spec.Sources) == 0 {
		return nil, false, fmt.Errorf("at least one source is required")
	}
	interval := strings.TrimSpace(spec.SyncInterval)
	if interval == "" {
		interval = DefaultSyncInterval.String()
	} else if _, e := time.ParseDuration(interval); e != nil {
		interval = DefaultSyncInterval.String()
	}
	shortName := spec.ShortName
	if shortName == "" {
		shortName = slugify(spec.Name)
	}

	r.mu.Lock()
	orig, exists := r.items[key(spec.Agent, spec.ID)]
	r.mu.Unlock()

	if !exists {
		nd := &Dashboard{
			ID:           spec.ID,
			Agent:        spec.Agent,
			Name:         spec.Name,
			ShortName:    shortName,
			Description:  spec.Description,
			Kind:         spec.Kind,
			SyncInterval: interval,
			Sources:      spec.Sources,
			Prompt:       spec.Prompt,
			CreatedBy:    spec.CreatedBy,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
			Source:       spec.Source,
			SourceRef:    spec.SourceRef,
		}
		if err := r.persist(nd); err != nil {
			return nil, false, err
		}
		r.mu.Lock()
		r.items[key(spec.Agent, spec.ID)] = nd
		r.mu.Unlock()
		r.startRunner(ctx, nd, false)
		log.Printf("[dashboards] upsert created %s/%s (%q, source=%s)", spec.Agent, spec.ID, spec.Name, spec.Source)
		return nd, true, nil
	}

	specHash := upsertFingerprint(spec, interval, shortName)
	currentHash := upsertFingerprint(specFromDashboard(orig), orig.SyncInterval, orig.ShortName)
	if specHash == currentHash {
		return orig, false, nil
	}

	updated := *orig
	scheduleChanged := updated.SyncInterval != interval
	updated.Name = spec.Name
	updated.ShortName = shortName
	updated.Description = spec.Description
	updated.Kind = spec.Kind
	updated.SyncInterval = interval
	updated.Sources = append([]DataSource(nil), spec.Sources...)
	updated.Prompt = spec.Prompt
	updated.Source = spec.Source
	updated.SourceRef = spec.SourceRef

	if err := r.persist(&updated); err != nil {
		return nil, false, err
	}
	r.mu.Lock()
	r.items[key(spec.Agent, spec.ID)] = &updated
	r.mu.Unlock()
	if scheduleChanged {
		r.startRunner(ctx, &updated, false)
	}
	log.Printf("[dashboards] upsert updated %s/%s (%q, source=%s)", spec.Agent, spec.ID, spec.Name, spec.Source)
	cp := updated
	return &cp, true, nil
}

// ListBySource returns shallow copies of dashboards whose Source equals src.
func (r *Registry) ListBySource(src string) []*Dashboard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Dashboard, 0)
	for _, d := range r.items {
		if d.Source != src {
			continue
		}
		cp := *d
		out = append(out, &cp)
	}
	return out
}

// upsertFingerprint hashes the fields that should trigger a write so
// no-op reconciles do not bounce the runner or rewrite the descriptor.
func upsertFingerprint(spec UpsertSpec, interval, shortName string) string {
	type fp struct {
		Name, ShortName, Description, Kind, SyncInterval, Source, SourceRef, Prompt string
		Sources                                                                     []DataSource
	}
	body, _ := json.Marshal(fp{
		Name:         spec.Name,
		ShortName:    shortName,
		Description:  spec.Description,
		Kind:         spec.Kind,
		SyncInterval: interval,
		Source:       spec.Source,
		SourceRef:    spec.SourceRef,
		Prompt:       spec.Prompt,
		Sources:      spec.Sources,
	})
	return string(body)
}

func specFromDashboard(d *Dashboard) UpsertSpec {
	return UpsertSpec{
		ID:           d.ID,
		Agent:        d.Agent,
		Name:         d.Name,
		ShortName:    d.ShortName,
		Description:  d.Description,
		Kind:         d.Kind,
		SyncInterval: d.SyncInterval,
		Sources:      d.Sources,
		Prompt:       d.Prompt,
		CreatedBy:    d.CreatedBy,
		Source:       d.Source,
		SourceRef:    d.SourceRef,
	}
}

// startRunner launches the sync goroutine for d. Replaces any existing runner.
func (r *Registry) startRunner(parent context.Context, d *Dashboard, runInitial bool) {
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
		// runInitial=true is the create / explicit-refresh path and should
		// populate the dashboard so the first view has data. runInitial=false
		// is the server-boot path — we trust whatever is already on disk and
		// wait for the next tick.
		if runInitial {
			r.syncOne(ctx, agent, id)
		}

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

	// Prompt-driven dashboards render through the LLM tool-loop, not the
	// source executor.
	if d.Kind == KindPrompt {
		r.syncPrompt(ctx, agent, id, &d)
		return
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
	return store.WriteJSON(r.dir, d.Agent, d.ID, d)
}

func (r *Registry) pathFor(d *Dashboard) string {
	return store.PathFor(r.dir, d.Agent, d.ID)
}

func readFile(path string) (*Dashboard, error) {
	return store.ReadJSON[Dashboard](path, func(d *Dashboard) error {
		if d.ID == "" || d.Agent == "" || !idValidRe.MatchString(d.ID) || !agentValidRe.MatchString(d.Agent) {
			return fmt.Errorf("invalid dashboard descriptor")
		}
		return nil
	})
}

// newID generates an 8-byte URL-safe hex identifier.
func newID() (string, error) { return store.NewID() }

func slugify(s string) string { return store.Slugify(s, "dashboard") }

// promptInputRe matches a {{VAR}} placeholder. Names are [A-Za-z0-9_].
var promptInputRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// PromptInputs returns the distinct {{VAR}} placeholder names in a prompt, in
// first-seen order. Case-insensitive dedupe (TENANT and tenant are one input),
// preserving the first spelling seen.
func PromptInputs(prompt string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range promptInputRe.FindAllStringSubmatch(prompt, -1) {
		name := m[1]
		key := strings.ToUpper(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

// SubstitutePromptInputs replaces every {{VAR}} in prompt with the matching
// value from inputs (case-insensitive on the name). Unmatched placeholders are
// left untouched.
func SubstitutePromptInputs(prompt string, inputs map[string]string) string {
	if len(inputs) == 0 {
		return prompt
	}
	return promptInputRe.ReplaceAllStringFunc(prompt, func(tok string) string {
		name := promptInputRe.FindStringSubmatch(tok)[1]
		for k, v := range inputs {
			if strings.EqualFold(k, name) {
				return v
			}
		}
		return tok
	})
}

// InstanceSlug builds the id suffix for a rendered instance from its input
// values (sorted by key for determinism). Falls back to "default" when there
// are no inputs.
func InstanceSlug(inputs map[string]string) string {
	if len(inputs) == 0 {
		return "default"
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, store.Slugify(inputs[k], "v"))
	}
	return strings.Join(parts, "-")
}

// buildPromptInstance builds a rendered-instance shell for a prompt template
// with the given input values. Markdown is empty — it is populated by the
// first sync. The ID is slug-derived so re-rendering the same inputs
// overwrites the same instance.
func buildPromptInstance(tmpl *Dashboard, inputs map[string]string, createdBy string) *Dashboard {
	now := time.Now().UTC().Format(time.RFC3339)
	label := instanceLabel(inputs)
	name := tmpl.Name
	if label != "" {
		name = tmpl.Name + " — " + label
	}
	inst := &Dashboard{
		ID:           tmpl.ID + "-" + InstanceSlug(inputs),
		Agent:        tmpl.Agent,
		Name:         name,
		ShortName:    tmpl.ShortName,
		Description:  tmpl.Description,
		Kind:         KindPrompt,
		SyncInterval: tmpl.SyncInterval,
		Prompt:       tmpl.Prompt, // snapshot; live template prompt preferred at render
		TemplateID:   tmpl.ID,
		Inputs:       inputs,
		CreatedBy:    createdBy,
		CreatedAt:    now,
	}
	if inst.SyncInterval == "" {
		inst.SyncInterval = DefaultSyncInterval.String()
	}
	return inst
}

// instanceLabel renders input values as a human label (values joined by " · ").
func instanceLabel(inputs map[string]string) string {
	if len(inputs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, inputs[k])
	}
	return strings.Join(parts, " · ")
}

// RenderInstance creates (or overwrites) a rendered instance of the prompt
// template templateID with the given input values, starts its refresh ticker,
// and fires an immediate render. It returns the instance shell straight away
// (Markdown still empty); the caller polls the instance's data.json until the
// first render lands. This mirrors the workflow "run now" fire-and-forget flow.
//
// interval overrides the instance's auto-refresh cadence when non-empty and a
// valid Go duration; otherwise the template's sync_interval is inherited.
func (r *Registry) RenderInstance(ctx context.Context, agent, templateID string, inputs map[string]string, interval string) (*Dashboard, error) {
	tmpl, ok := r.Get(agent, templateID)
	if !ok {
		return nil, fmt.Errorf("prompt dashboard %s/%s not found", agent, templateID)
	}
	if tmpl.Kind != KindPrompt {
		return nil, fmt.Errorf("dashboard %s/%s is not a prompt dashboard", agent, templateID)
	}
	if tmpl.TemplateID != "" {
		return nil, fmt.Errorf("dashboard %s/%s is already an instance, not a template", agent, templateID)
	}
	required := PromptInputs(tmpl.Prompt)
	for _, name := range required {
		found := false
		for k, v := range inputs {
			if strings.EqualFold(k, name) && strings.TrimSpace(v) != "" {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("missing value for input %q", name)
		}
	}
	inst := buildPromptInstance(tmpl, inputs, tmpl.CreatedBy)
	if s := strings.TrimSpace(interval); s != "" {
		if _, err := time.ParseDuration(s); err == nil {
			inst.SyncInterval = s
		}
	}
	if err := r.persist(inst); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.items[key(agent, inst.ID)] = inst
	r.mu.Unlock()
	// runInitial=true renders immediately in the runner goroutine. Use a
	// background parent so the render survives the HTTP request returning.
	r.startRunner(context.Background(), inst, true)
	log.Printf("[dashboards] render instance %s/%s of template %s", agent, inst.ID, templateID)
	cp := *inst
	return &cp, nil
}

// syncPrompt renders a prompt dashboard through the LLM tool-loop and persists
// the Markdown. Templates that still have unresolved {{VAR}} placeholders are
// skipped (they are rendered per-instance, not in place). Instances resolve
// against the live template prompt when available, falling back to their own
// snapshot.
func (r *Registry) syncPrompt(ctx context.Context, agent, id string, d *Dashboard) {
	isInstance := d.TemplateID != ""
	prompt := d.Prompt
	if isInstance {
		if tmpl, ok := r.Get(agent, d.TemplateID); ok && strings.TrimSpace(tmpl.Prompt) != "" {
			prompt = tmpl.Prompt
		}
		prompt = SubstitutePromptInputs(prompt, d.Inputs)
	} else if len(PromptInputs(prompt)) > 0 {
		// Template with declared inputs — rendered per-instance, not in place.
		return
	}

	r.mu.RLock()
	renderer := r.renderer
	r.mu.RUnlock()
	if renderer == nil {
		return
	}

	md, err := renderer.RenderPrompt(ctx, agent, id, d.Name, prompt)
	d.LastSync = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		d.LastError = err.Error()
	} else {
		d.Markdown = strings.TrimSpace(md)
		d.LastError = ""
	}
	r.mu.Lock()
	r.items[key(agent, id)] = d
	r.mu.Unlock()
	if perr := r.persist(d); perr != nil {
		log.Printf("[dashboards] persist %s/%s failed: %v", agent, id, perr)
	}
}
