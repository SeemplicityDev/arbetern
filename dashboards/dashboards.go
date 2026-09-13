// Package dashboards provides a lightweight dashboard engine for arbetern.
//
// Each dashboard is a small JSON descriptor stored in the state bucket at
//
//	<Prefix><agent>/<dashboard-id>.json
//
// It owns a list of "data sources" (pre-defined read-only integration queries)
// and a sync interval. A goroutine per dashboard periodically re-executes the
// sources and writes the latest results back to the same document, so the
// serving layer can render a fresh view without touching the integrations
// directly.
//
// Dashboards can be created, listed, and deleted by LLM tools; they can also
// be viewed in the management console at /ui/<agent>/dashboard/<id>.
//
// Sync goroutines only run on the replica holding the scheduling lease; every
// replica keeps its cache in step with the bucket, and each sync takes a
// per-dashboard lease so two replicas never refresh the same dashboard at once.
package dashboards

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/crud"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
)

const (
	// Prefix is the object prefix dashboard descriptors are stored under.
	Prefix = "dashboards/"
	// legacyCachePrefix held per-account lookups before state moved to the bucket.
	legacyCachePrefix = Prefix + "_cache/"

	// syncLeaseTTL bounds how long a crashed replica blocks a dashboard's next sync.
	syncLeaseTTL = 5 * time.Minute
	// persistTimeout bounds the write of a finished sync's results.
	persistTimeout = 30 * time.Second

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

// Dashboard is the stored descriptor.
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

// ViewURL returns the management-console page for this dashboard.
func (d *Dashboard) ViewURL() string {
	return crud.ViewPath("dashboard", d.Agent, d.ID)
}

// Summary returns a copy without the fetched data and rendered report, for lists.
func (d *Dashboard) Summary() *Dashboard {
	cp := *d
	cp.Data = nil
	cp.Markdown = ""
	return &cp
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

// Registry is the cached view of the stored dashboards plus their goroutines.
type Registry struct {
	docs *store.Documents[Dashboard]
	b    *store.Backend

	mu       sync.RWMutex
	executor Executor
	renderer PromptRenderer
	runners  map[string]*runner // key: agent/id
	// baseCtx is the long-lived registry context, set by StartAll. Runner
	// goroutines derive their context from it rather than from the request
	// that created the dashboard.
	baseCtx context.Context
	// active is true between StartAll and StopAll, i.e. while this replica
	// holds the scheduling lease. Runners are only started while active.
	active bool
}

// New creates a Registry over b. Call LoadAll before serving.
func New(b *store.Backend, exec Executor) *Registry {
	return &Registry{
		docs: store.NewDocuments(b, Prefix, store.AgentKeyRe, func(d *Dashboard) error {
			if d.ID == "" || d.Agent == "" || !idValidRe.MatchString(d.ID) || !agentValidRe.MatchString(d.Agent) {
				return fmt.Errorf("invalid dashboard descriptor")
			}
			return nil
		}),
		b:        b,
		executor: exec,
		runners:  make(map[string]*runner),
	}
}

// Count is the number of stored dashboards.
func (r *Registry) Count() int { return r.docs.Len() }

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

// LoadAll loads every stored dashboard into the cache. It does NOT start
// sync goroutines — call StartAll afterwards to launch them. Invalid
// documents are logged and skipped; they do not prevent startup.
func (r *Registry) LoadAll(ctx context.Context) error {
	if err := r.docs.Load(ctx); err != nil {
		return fmt.Errorf("load dashboards: %w", err)
	}
	r.docs.Range(func(_ string, d *Dashboard) {
		log.Printf("[dashboards] loaded %s/%s (%q, every %s)", d.Agent, d.ID, d.Name, d.interval())
	})
	return nil
}

// StartRefresh keeps the cache in step with dashboards written by other
// replicas every interval, and reconciles runners on the replica holding the
// scheduling lease.
func (r *Registry) StartRefresh(ctx context.Context, interval time.Duration) {
	r.docs.StartRefresh(ctx, interval, r.applyChanges)
}

func (r *Registry) applyChanges(changes []store.Change[Dashboard]) {
	r.mu.RLock()
	active := r.active
	r.mu.RUnlock()
	for _, c := range changes {
		switch {
		case c.New == nil:
			if c.Old != nil {
				r.stopRunner(c.Old.Agent, c.Old.ID)
			}
		case !active:
		case c.Old == nil:
			r.startRunner(c.New, c.New.Source != "gitops")
		case c.Old.SyncInterval != c.New.SyncInterval || c.Old.Kind != c.New.Kind:
			r.startRunner(c.New, false)
		}
	}
}

// StartAll launches a sync goroutine for every loaded dashboard WITHOUT
// firing an immediate sync, and marks this replica as the one that syncs.
// Intended as the server-boot (and lease-acquired) entry point: we just want
// the scheduled ticker to begin, not to blast every upstream (Jira, Datadog,
// GitHub, Chorus, …) the moment the process comes up. Dashboards that are
// explicitly refreshed via "Refresh now" or via Create will still sync
// immediately — only the boot path is lazy.
func (r *Registry) StartAll(ctx context.Context) {
	r.mu.Lock()
	r.baseCtx = ctx
	r.active = true
	r.mu.Unlock()
	r.docs.Range(func(_ string, d *Dashboard) {
		r.startRunner(d, false)
	})
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
	if err := r.docs.Create(ctx, store.Key(agent, id), d); err != nil {
		return nil, err
	}
	r.startRunner(d, true)
	log.Printf("[dashboards] created %s/%s (%q, every %s)", agent, id, name, d.interval())
	return d, nil
}

// Get returns a copy of the stored dashboard by agent+id, or (nil,false) if missing.
func (r *Registry) Get(agent, id string) (*Dashboard, bool) {
	return r.docs.Get(store.Key(agent, id))
}

// List returns all dashboards, optionally filtered by agent (empty = all),
// sorted by creation time ascending.
func (r *Registry) List(agent string) []*Dashboard {
	out := make([]*Dashboard, 0)
	r.docs.Range(func(_ string, d *Dashboard) {
		if agent != "" && d.Agent != agent {
			return
		}
		out = append(out, d)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// ListSummaries is List without fetched data and rendered reports.
func (r *Registry) ListSummaries(agent string) []*Dashboard {
	list := r.List(agent)
	for i, d := range list {
		list[i] = d.Summary()
	}
	return list
}

// PurgeLegacyCache deletes the objects a previous release kept under the
// dashboards prefix that are not descriptors. It returns how many were removed.
func (r *Registry) PurgeLegacyCache(ctx context.Context) (int, error) {
	objs, err := r.b.List(ctx, legacyCachePrefix)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, o := range objs {
		if err := r.b.Delete(ctx, o.Key, ""); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Delete stops the sync goroutine and removes the stored descriptor.
func (r *Registry) Delete(agent, id string) error {
	k := store.Key(agent, id)
	if _, ok := r.docs.Get(k); !ok {
		return fmt.Errorf("dashboard %s/%s not found", agent, id)
	}
	r.mu.Lock()
	run := r.runners[key(agent, id)]
	delete(r.runners, key(agent, id))
	r.mu.Unlock()

	if run != nil {
		run.cancel()
		// Best-effort wait so a concurrent sync finishes before we remove the document.
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
		}
	}
	if err := r.docs.Delete(context.Background(), k); err != nil {
		return fmt.Errorf("remove dashboard: %w", err)
	}
	log.Printf("[dashboards] deleted %s/%s", agent, id)
	return nil
}

// StopAll cancels every sync goroutine and stops new ones from starting until
// StartAll runs again. Safe to call at shutdown.
func (r *Registry) StopAll() {
	r.mu.Lock()
	r.active = false
	runs := r.runners
	r.runners = make(map[string]*runner)
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
}

func (r *Registry) stopRunner(agent, id string) {
	r.mu.Lock()
	run, ok := r.runners[key(agent, id)]
	if ok {
		delete(r.runners, key(agent, id))
	}
	r.mu.Unlock()
	if ok {
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

	k := store.Key(spec.Agent, spec.ID)
	if _, exists := r.docs.Get(k); !exists {
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
		err := r.docs.Create(ctx, k, nd)
		if err == nil {
			r.startRunner(nd, false)
			log.Printf("[dashboards] upsert created %s/%s (%q, source=%s)", spec.Agent, spec.ID, spec.Name, spec.Source)
			return nd, true, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, false, err
		}
	}

	specHash := upsertFingerprint(spec, interval, shortName)
	var orig Dashboard
	updated, err := r.docs.Update(ctx, k, func(d *Dashboard) error {
		orig = *d
		if upsertFingerprint(specFromDashboard(d), d.SyncInterval, d.ShortName) == specHash {
			return errUnchanged
		}
		d.Name = spec.Name
		d.ShortName = shortName
		d.Description = spec.Description
		d.Kind = spec.Kind
		d.SyncInterval = interval
		d.Sources = append([]DataSource(nil), spec.Sources...)
		d.Prompt = spec.Prompt
		d.Source = spec.Source
		d.SourceRef = spec.SourceRef
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return &orig, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if orig.SyncInterval != updated.SyncInterval {
		r.startRunner(updated, false)
	}
	log.Printf("[dashboards] upsert updated %s/%s (%q, source=%s)", spec.Agent, spec.ID, spec.Name, spec.Source)
	return updated, true, nil
}

var errUnchanged = errors.New("unchanged")

// ListBySource returns copies of dashboards whose Source equals src.
func (r *Registry) ListBySource(src string) []*Dashboard {
	out := make([]*Dashboard, 0)
	r.docs.Range(func(_ string, d *Dashboard) {
		if d.Source == src {
			out = append(out, d)
		}
	})
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
// It does nothing on a replica that does not hold the scheduling lease.
func (r *Registry) startRunner(d *Dashboard, runInitial bool) {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	if old, ok := r.runners[key(d.Agent, d.ID)]; ok {
		old.cancel()
	}
	parent := r.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &runner{cancel: cancel, done: make(chan struct{})}
	r.runners[key(d.Agent, d.ID)] = run
	r.mu.Unlock()

	agent := d.Agent
	id := d.ID
	interval := d.interval()

	// Guarded per sync, not per goroutine: a panicking refresh must not end the
	// ticker and leave this dashboard permanently stale.
	sync := func() {
		safego.Run("dashboards: sync "+agent+"/"+id, func() { r.syncOne(ctx, agent, id) })
	}

	go func() {
		defer close(run.done)
		defer safego.Recover("dashboards: runner " + agent + "/" + id)
		// runInitial=true is the create / explicit-refresh path and should
		// populate the dashboard so the first view has data. runInitial=false
		// is the server-boot path — we trust whatever is already stored and
		// wait for the next tick.
		if runInitial {
			sync()
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sync()
			}
		}
	}()
}

// syncOne re-executes all sources for a dashboard and stores the results
// against the latest descriptor. A per-dashboard lease keeps two replicas
// from refreshing the same dashboard at once.
func (r *Registry) syncOne(ctx context.Context, agent, id string) {
	d, ok := r.docs.Get(store.Key(agent, id))
	if !ok {
		return
	}
	lease := store.NewLease(r.b, "locks/dashboards/"+agent+"/"+id, store.InstanceID(), syncLeaseTTL)
	held, release, acquired, err := lease.Acquire(ctx)
	switch {
	case err != nil:
		log.Printf("[dashboards] %s/%s sync lease unavailable, continuing without it: %v", agent, id, err)
	case !acquired:
		log.Printf("[dashboards] skip %s/%s: syncing on another replica", agent, id)
		return
	default:
		defer release()
		ctx = held
	}

	// Prompt-driven dashboards render through the LLM tool-loop, not the
	// source executor.
	if d.Kind == KindPrompt {
		r.syncPrompt(ctx, agent, id, d)
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
	syncedAt := time.Now().UTC().Format(time.RFC3339)
	r.record(agent, id, func(d *Dashboard) {
		d.Data = data
		d.LastSync = syncedAt
		d.LastError = ""
	})
}

// record applies fn to the latest stored descriptor. It uses its own context
// so a sync whose context was cancelled after the work finished still lands.
func (r *Registry) record(agent, id string, fn func(*Dashboard)) {
	pctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	_, err := r.docs.Update(pctx, store.Key(agent, id), func(d *Dashboard) error {
		fn(d)
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[dashboards] persist %s/%s failed: %v", agent, id, err)
	}
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

// instanceID composes a rendered instance's id from its template id and input
// slug. The slug is derived from request-supplied values, so the result is
// capped at store.MaxSegment: an over-long id is rejected by the store on
// write, and any that slipped through would be dropped as an invalid
// descriptor on the next load. Truncated ids are disambiguated with a hash of
// the full slug so distinct inputs keep distinct — and stable — instances.
func instanceID(templateID, slug string) string {
	id := templateID + "-" + slug
	if len(id) <= store.MaxSegment {
		return id
	}
	sum := sha256.Sum256([]byte(slug))
	suffix := "-" + hex.EncodeToString(sum[:4])
	head := templateID + "-"
	if len(head)+len(suffix) > store.MaxSegment {
		head = head[:store.MaxSegment-len(suffix)]
	}
	keep := store.MaxSegment - len(head) - len(suffix)
	return strings.TrimRight(head+slug[:keep], "-") + suffix
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
		ID:           instanceID(tmpl.ID, InstanceSlug(inputs)),
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
	k := store.Key(agent, inst.ID)
	err := r.docs.Create(ctx, k, inst)
	if errors.Is(err, store.ErrConflict) {
		_, err = r.docs.Update(ctx, k, func(d *Dashboard) error {
			*d = *inst
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	// runInitial=true renders immediately in the runner goroutine.
	r.startRunner(inst, true)
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
	syncedAt := time.Now().UTC().Format(time.RFC3339)
	r.record(agent, id, func(d *Dashboard) {
		d.LastSync = syncedAt
		if err != nil {
			d.LastError = err.Error()
			return
		}
		d.Markdown = strings.TrimSpace(md)
		d.LastError = ""
	})
}
