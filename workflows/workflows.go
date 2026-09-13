// Package workflows provides a lightweight scheduled-agent engine for arbetern.
//
// Each workflow is a small JSON descriptor stored in the state bucket at
//
//	<Prefix><agent>/<workflow-id>.json
//
// It owns a natural-language prompt (or an ordered list of task prompts) and
// a trigger (schedule, on_success of another workflow, on_failure of another
// workflow, or manual-only). A goroutine per scheduled workflow periodically
// re-runs the prompt through the owning agent's LLM tool-loop (so the
// workflow has access to the same tools as a regular /agent command — Jira
// search, GitHub PR creation, Slack posting, etc.).
//
// Four execution patterns are supported (loosely modelled on Prefect flows):
//
//   - Monoflow: a single Prompt on a schedule. Tight coupling, one LLM call
//     per tick, simplest to reason about.
//   - Flow of subflows (Tasks): an ordered list of sub-prompts; each task's
//     output is fed into the next task's context. Keeps individual LLM calls
//     small and bounded.
//   - Flow of deployments (call_workflow): a task invokes another workflow
//     by id via the `call_workflow` LLM tool, decoupling concerns across
//     agents.
//   - Event-triggered: a workflow with Trigger.Type = "on_success" or
//     "on_failure" and Trigger.Ref = "<agent>/<id>" runs reactively whenever
//     the referenced workflow finishes.
//
// Each run's outcome is appended to the descriptor so the serving layer can
// render an HTML history without touching the integrations directly.
//
// Runners only tick on the replica holding the scheduling lease; every
// replica keeps its cache in step with the bucket and reconciles runners
// from the changes it sees, and each run takes a per-workflow lease so two
// replicas never execute the same workflow at once.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justmike1/arbetern/internal/store"
	"github.com/robfig/cron/v3"

	"github.com/justmike1/arbetern/internal/crud"
	"github.com/justmike1/arbetern/internal/safego"
)

const (
	// Prefix is the object prefix workflow descriptors are stored under.
	Prefix = "workflows/"

	// runLeaseTTL bounds how long a crashed replica blocks a workflow's next run.
	runLeaseTTL = 5 * time.Minute
	// persistTimeout bounds the write of a finished run's outcome.
	persistTimeout = 30 * time.Second

	// DefaultCron is the cron expression used when a scheduled workflow is
	// created without an explicit one (every 5 minutes from runner start).
	DefaultCron = "@every 5m"

	// defaultMaxRunHistory is the fallback used when WORKFLOW_RUN_HISTORY
	// is unset or invalid. See MaxRunHistory.
	defaultMaxRunHistory = 20

	// MaxResultChars bounds a single stored run's result text.
	MaxResultChars = 8000
	// MaxTaskContextChars bounds the prior-task summary fed into the next task.
	MaxTaskContextChars = 2000
	// MaxConsecutiveFailures is the number of back-to-back failed ticks (or
	// ticks that completed with at least one mutating-tool error) before a
	// scheduled workflow is automatically disabled. A human must then
	// inspect the last_error / disabled_reason and re-enable the workflow
	// via update_workflow. This prevents a broken workflow (bad prompt,
	// misconfigured branch name, revoked credentials, etc.) from spamming
	// PRs / Slack / 429s indefinitely.
	MaxConsecutiveFailures = 3

	// TriggerSchedule runs the workflow on its Cron expression.
	TriggerSchedule = "schedule"
	// TriggerOnSuccess runs the workflow right after Trigger.Ref succeeds.
	TriggerOnSuccess = "on_success"
	// TriggerOnFailure runs the workflow right after Trigger.Ref fails.
	TriggerOnFailure = "on_failure"
	// TriggerManual disables automatic ticks; only the manual /run endpoint
	// or a call_workflow tool call can fire the workflow.
	TriggerManual = "manual"
)

// MaxRunHistory bounds the per-workflow on-disk run log. Default is 20;
// override via the WORKFLOW_RUN_HISTORY environment variable (positive
// integer). The bound is applied on every persist, so shrinking it takes
// effect on the next tick — older entries are dropped, never resurrected.
// The cap exists so a busy hourly workflow doesn't grow data.json
// unbounded. It's a package-level var (rather than a const) so the env
// override can take effect at process start.
var MaxRunHistory = resolveMaxRunHistory()

func resolveMaxRunHistory() int {
	if s := strings.TrimSpace(os.Getenv("WORKFLOW_RUN_HISTORY")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
		log.Printf("[workflows] invalid WORKFLOW_RUN_HISTORY %q; falling back to default %d", s, defaultMaxRunHistory)
	}
	return defaultMaxRunHistory
}

// Task is one step in a multi-step workflow. Each task is an independent
// prompt; the registry serialises tasks, threading each result forward as
// context for the next.
type Task struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// Trigger describes how a workflow is fired.
type Trigger struct {
	// Type is one of TriggerSchedule, TriggerOnSuccess, TriggerOnFailure,
	// TriggerManual. Empty == TriggerSchedule.
	Type string `json:"type,omitempty"`
	// Ref is "<agent>/<id>" for on_success / on_failure triggers.
	Ref string `json:"ref,omitempty"`
}

// TaskResult records the outcome of a single task within a workflow tick.
type TaskResult struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
}

// RunLog records the outcome of a single workflow tick.
type RunLog struct {
	StartedAt   string       `json:"started_at"`
	DurationMS  int64        `json:"duration_ms"`
	Result      string       `json:"result,omitempty"`
	Error       string       `json:"error,omitempty"`
	Tasks       []TaskResult `json:"tasks,omitempty"`
	TriggeredBy string       `json:"triggered_by,omitempty"` // empty, "manual", "schedule", "on_success:<ref>", etc.
}

// Workflow is the on-disk descriptor.
type Workflow struct {
	ID          string `json:"id"`
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	ShortName   string `json:"short_name"`
	Description string `json:"description,omitempty"`
	// Cron is the standard 5-field UTC cron expression that schedules this
	// workflow (e.g. "0 5 * * *" for daily at 05:00 UTC, "*/15 * * * *"
	// for every 15 minutes, "0 18 * * 4" for Thursday 18:00,
	// "0 5 * * 0-4" for Sunday–Thursday at 05:00). Descriptors like
	// "@every 1h", "@daily", "@hourly" are also accepted. The next fire
	// time is always the next match of the expression after now (in UTC).
	// On server restart, if the most recent expected fire time is after
	// LastRun, the runner fires a single catch-up tick immediately so a
	// missed daily report is delivered after a redeploy. Required for
	// schedule-triggered workflows; ignored for manual / event-triggered.
	Cron   string `json:"cron,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Tasks  []Task `json:"tasks,omitempty"`
	// Model overrides the agent's default CODE_MODEL for this workflow's ticks.
	// It is a backend deployment/model name (e.g. a cheaper model for simple
	// reporting workflows). Empty == use the configured code model.
	Model      string  `json:"model,omitempty"`
	Trigger    Trigger `json:"trigger,omitempty"`
	CreatedBy  string  `json:"created_by,omitempty"`
	CreatedAt  string  `json:"created_at"`
	Enabled    bool    `json:"enabled"`
	LastRun    string  `json:"last_run,omitempty"`
	LastError  string  `json:"last_error,omitempty"`
	LastResult string  `json:"last_result,omitempty"`
	// ConsecutiveFailures is incremented on every failed or partially-failed
	// tick and reset to 0 on a clean run. When it reaches
	// MaxConsecutiveFailures the workflow is auto-disabled.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// DisabledReason is set when the registry auto-disables a workflow
	// (empty when the user manually toggled enabled=false). The UI surfaces
	// this to explain why a workflow stopped ticking.
	DisabledReason string `json:"disabled_reason,omitempty"`
	// Source identifies how the workflow was created. "" / "ui" =
	// interactive; "gitops" = managed by the GitOps poller.
	Source string `json:"source,omitempty"`
	// SourceRef is an informational pointer to the upstream definition
	// for non-ui sources (e.g. "<owner>/<repo>@<branch>:<path>").
	SourceRef string   `json:"source_ref,omitempty"`
	Runs      []RunLog `json:"runs,omitempty"`
	// Running is a transient, never-persisted flag populated by Get/List
	// from the live runner state. True while a tick is in flight.
	Running bool `json:"running,omitempty"`
}

// ViewURL returns the management-console page for this workflow.
func (w *Workflow) ViewURL() string {
	return crud.ViewPath("workflow", w.Agent, w.ID)
}

// Pattern returns a human-readable label describing the workflow's
// execution pattern, used for UI labels and logs.
func (w *Workflow) Pattern() string {
	switch w.triggerType() {
	case TriggerOnSuccess, TriggerOnFailure:
		return "event-triggered"
	case TriggerManual:
		return "manual"
	}
	if len(w.Tasks) > 0 {
		return "subflows"
	}
	return "monoflow"
}

func (w *Workflow) triggerType() string {
	if w.Trigger.Type == "" {
		return TriggerSchedule
	}
	return w.Trigger.Type
}

// Executor runs a single prompt (a monoflow or a single task from a
// multi-task workflow) and returns a human-readable result summary.
// Implementations wire in the agent LLM tool-loop.
//
// The executor is called once per task. For a monoflow, the registry passes
// Workflow.Prompt. For a Flow-of-subflows, the registry passes each task's
// prompt in turn, with prior-task outputs already merged into the prompt
// text as a "Previous task results" prelude.
type Executor interface {
	Run(ctx context.Context, w *Workflow, prompt string) (string, error)
}

// runner owns the goroutine driving one workflow's tick loop.
type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
	// busy is set to 1 while this workflow has a tick in flight. It is used
	// as a non-blocking try-lock so overlapping triggers (e.g. a manual
	// /run firing while a scheduled tick is mid-execution, or a very slow
	// tick whose runtime exceeds the inter-fire delta before it finishes)
	// do not spawn a concurrent second run of the same workflow, which
	// would double-post to Slack, double-create PRs, and race on
	// persistence.
	busy atomic.Int32
}

// Registry is the cached view of the stored workflows plus their goroutines.
type Registry struct {
	docs *store.Documents[Workflow]
	b    *store.Backend

	mu       sync.RWMutex
	executor Executor
	runners  map[string]*runner // key: agent/id
	// baseCtx is the long-lived registry context, set by StartAllEnabled.
	// Runner goroutines derive their context from this, NOT from per-request
	// contexts passed to Create/Update — otherwise the HTTP handler returning
	// would cancel the runner and any in-flight tick.
	baseCtx context.Context
	// active is true between StartAllEnabled and StopAll, i.e. while this
	// replica holds the scheduling lease. Runners are only started while active.
	active bool
}

// New creates a Registry over b. Call LoadAll before serving.
func New(b *store.Backend, exec Executor) *Registry {
	return &Registry{
		docs: store.NewDocuments(b, Prefix, store.AgentKeyRe, func(w *Workflow) error {
			if w.ID == "" || w.Agent == "" || !idValidRe.MatchString(w.ID) || !agentValidRe.MatchString(w.Agent) {
				return fmt.Errorf("invalid workflow descriptor")
			}
			return nil
		}),
		b:        b,
		executor: exec,
		runners:  make(map[string]*runner),
	}
}

// Count is the number of stored workflows.
func (r *Registry) Count() int { return r.docs.Len() }

// SetExecutor installs the executor post-construction. Useful when the
// executor depends on components (e.g. the router map) that are built after
// the registry.
func (r *Registry) SetExecutor(exec Executor) {
	r.mu.Lock()
	r.executor = exec
	r.mu.Unlock()
}

func key(agent, id string) string { return agent + "/" + id }

// Re-exported so the http layer (in this same package) can keep using the
// short names without importing internal/store directly.
var (
	agentValidRe = store.AgentRe
	idValidRe    = store.IDRe
)

// LoadAll loads every stored workflow into the cache. It does NOT start tick
// goroutines — StartAllEnabled handles that once the executor has been wired
// up. Invalid documents are logged and skipped; they do not prevent startup.
func (r *Registry) LoadAll(ctx context.Context) error {
	if err := r.docs.Load(ctx); err != nil {
		return fmt.Errorf("load workflows: %w", err)
	}
	r.docs.Range(func(_ string, w *Workflow) {
		log.Printf("[workflows] loaded %s/%s (%q, pattern=%s, enabled=%t)", w.Agent, w.ID, w.Name, w.Pattern(), w.Enabled)
	})
	return nil
}

// StartRefresh keeps the cache in step with workflows written by other
// replicas every interval, and reconciles runners on the replica holding the
// scheduling lease.
func (r *Registry) StartRefresh(ctx context.Context, interval time.Duration) {
	r.docs.StartRefresh(ctx, interval, r.applyChanges)
}

func (r *Registry) applyChanges(changes []store.Change[Workflow]) {
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
			if c.New.Enabled {
				r.startRunner(c.New, c.New.Source != "gitops")
			}
		default:
			r.reconcileRunner(c.Old, c.New)
		}
	}
}

// reconcileRunner restarts or stops the runner of a workflow whose stored
// descriptor changed from orig to updated. Non-scheduling edits (name,
// description, prompt, tasks) are picked up on the next tick because runOnce
// re-reads the stored workflow, so the ticker is left alone — cancelling it
// would also kill any in-flight run.
func (r *Registry) reconcileRunner(orig, updated *Workflow) {
	scheduleChanged := orig.Cron != updated.Cron ||
		orig.Trigger.Type != updated.Trigger.Type ||
		orig.Trigger.Ref != updated.Trigger.Ref
	enabledChanged := orig.Enabled != updated.Enabled
	switch {
	case !updated.Enabled:
		if enabledChanged {
			r.stopRunner(updated.Agent, updated.ID)
		}
	case scheduleChanged || enabledChanged:
		r.startRunner(updated, false)
	}
}

// CreateOpts captures the full set of knobs for Create. Fields left empty
// fall back to sensible defaults (TriggerSchedule, DefaultCron, etc.).
type CreateOpts struct {
	Agent       string
	CreatedBy   string
	Name        string
	ShortName   string
	Description string
	Cron        string
	Prompt      string
	Tasks       []Task
	Trigger     Trigger
}

// Create validates, persists, and starts a new workflow.
func (r *Registry) Create(ctx context.Context, opts CreateOpts) (*Workflow, error) {
	if !agentValidRe.MatchString(opts.Agent) {
		return nil, fmt.Errorf("invalid agent id %q", opts.Agent)
	}
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("workflow name is required")
	}
	hasPrompt := strings.TrimSpace(opts.Prompt) != ""
	hasTasks := len(opts.Tasks) > 0
	if !hasPrompt && !hasTasks {
		return nil, fmt.Errorf("workflow requires either prompt or at least one task")
	}
	if hasTasks {
		for i, t := range opts.Tasks {
			if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Prompt) == "" {
				return nil, fmt.Errorf("task %d requires non-empty name and prompt", i+1)
			}
		}
	}
	cronExpr := strings.TrimSpace(opts.Cron)
	if cronExpr == "" {
		cronExpr = DefaultCron
	}
	if _, err := cron.ParseStandard(cronExpr); err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", cronExpr, err)
	}
	trig := opts.Trigger
	switch trig.Type {
	case "", TriggerSchedule:
		trig.Type = TriggerSchedule
		trig.Ref = ""
	case TriggerOnSuccess, TriggerOnFailure:
		if !strings.Contains(trig.Ref, "/") {
			return nil, fmt.Errorf("trigger ref must be '<agent>/<id>' for %s trigger", trig.Type)
		}
	case TriggerManual:
		trig.Ref = ""
	default:
		return nil, fmt.Errorf("unknown trigger type %q", trig.Type)
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	shortName := opts.ShortName
	if shortName == "" {
		shortName = slugify(opts.Name)
	}
	w := &Workflow{
		ID:          id,
		Agent:       opts.Agent,
		Name:        opts.Name,
		ShortName:   shortName,
		Description: opts.Description,
		Cron:        cronExpr,
		Prompt:      opts.Prompt,
		Tasks:       opts.Tasks,
		Trigger:     trig,
		CreatedBy:   opts.CreatedBy,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Enabled:     true,
	}
	if err := r.docs.Create(ctx, store.Key(opts.Agent, id), w); err != nil {
		return nil, err
	}
	r.startRunner(w, true)
	log.Printf("[workflows] created %s/%s (%q, pattern=%s, cron=%s)", opts.Agent, id, opts.Name, w.Pattern(), w.Cron)
	return w, nil
}

// Get returns a copy of the stored workflow, or (nil,false) if missing.
func (r *Registry) Get(agent, id string) (*Workflow, bool) {
	w, ok := r.docs.Get(store.Key(agent, id))
	if !ok {
		return nil, false
	}
	w.Running = r.isRunning(agent, id)
	return w, true
}

// List returns all workflows, optionally filtered by agent, sorted by creation time asc.
func (r *Registry) List(agent string) []*Workflow {
	out := make([]*Workflow, 0)
	r.docs.Range(func(_ string, w *Workflow) {
		if agent != "" && w.Agent != agent {
			return
		}
		w.Running = r.isRunning(w.Agent, w.ID)
		out = append(out, w)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// isRunning reports whether this replica has a tick of the workflow in flight.
func (r *Registry) isRunning(agent, id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run := r.runners[key(agent, id)]
	return run != nil && run.busy.Load() != 0
}

// UpdateOpts carries the editable fields of a workflow. Any field left at
// its zero value is preserved from the existing workflow, with the exception
// of Tasks (pass a non-nil slice — possibly empty — to replace tasks), Enabled
// (always applied), and the explicit Clear* flags for fields whose zero value
// is ambiguous with "don't change" (e.g. clearing a prompt when switching to
// tasks-only mode).
type UpdateOpts struct {
	Name        *string
	Description *string
	Cron        *string
	Prompt      *string
	Tasks       *[]Task  // nil = unchanged; non-nil (even empty) = replace
	Trigger     *Trigger // nil = unchanged
	Enabled     *bool
}

// Update applies a partial edit to an existing workflow. The tick goroutine
// is restarted so the new prompt / cron / trigger take effect on the next
// scheduled run. Run history, last-run timestamp, and created metadata are
// preserved. Fields left as nil pointers are untouched.
func (r *Registry) Update(ctx context.Context, agent, id string, opts UpdateOpts) (*Workflow, error) {
	var orig Workflow
	updated, err := r.docs.Update(ctx, store.Key(agent, id), func(w *Workflow) error {
		orig = *w
		return applyUpdate(w, opts)
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	if err != nil {
		return nil, err
	}
	// Pass runInitial=false so the edit does NOT fire an immediate tick
	// (previous behaviour caused every save to trigger a new run, which
	// also got killed by the HTTP request context).
	r.reconcileRunner(&orig, updated)
	log.Printf("[workflows] updated %s/%s (%q, pattern=%s, cron=%s, enabled=%t)",
		agent, id, updated.Name, updated.Pattern(), updated.Cron, updated.Enabled)
	return updated, nil
}

func applyUpdate(updated *Workflow, opts UpdateOpts) error {
	if opts.Name != nil {
		if strings.TrimSpace(*opts.Name) == "" {
			return fmt.Errorf("workflow name cannot be empty")
		}
		updated.Name = *opts.Name
	}
	if opts.Description != nil {
		updated.Description = *opts.Description
	}
	if opts.Cron != nil {
		v := strings.TrimSpace(*opts.Cron)
		if v == "" {
			return fmt.Errorf("cron expression cannot be empty")
		}
		if _, err := cron.ParseStandard(v); err != nil {
			return fmt.Errorf("invalid cron %q: %w", v, err)
		}
		updated.Cron = v
	}
	if opts.Prompt != nil {
		updated.Prompt = *opts.Prompt
	}
	if opts.Tasks != nil {
		for i, t := range *opts.Tasks {
			if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Prompt) == "" {
				return fmt.Errorf("task %d requires non-empty name and prompt", i+1)
			}
		}
		updated.Tasks = *opts.Tasks
	}
	if strings.TrimSpace(updated.Prompt) == "" && len(updated.Tasks) == 0 {
		return fmt.Errorf("workflow requires either prompt or at least one task after update")
	}
	if opts.Trigger != nil {
		trig := *opts.Trigger
		switch trig.Type {
		case "", TriggerSchedule:
			trig.Type = TriggerSchedule
			trig.Ref = ""
		case TriggerOnSuccess, TriggerOnFailure:
			if !strings.Contains(trig.Ref, "/") {
				return fmt.Errorf("trigger ref must be '<agent>/<id>' for %s trigger", trig.Type)
			}
		case TriggerManual:
			trig.Ref = ""
		default:
			return fmt.Errorf("unknown trigger type %q", trig.Type)
		}
		updated.Trigger = trig
	}
	if opts.Enabled != nil {
		updated.Enabled = *opts.Enabled
		// Manually re-enabling a workflow clears the auto-disable state so
		// the runner gets a fresh failure budget and the UI banner goes away.
		if *opts.Enabled {
			updated.ConsecutiveFailures = 0
			updated.DisabledReason = ""
		}
	}
	return nil
}

// UpsertSpec is the declarative shape used by id-stable callers (e.g.
// the GitOps syncer). All fields replace the stored value; runtime
// bookkeeping (run history, LastRun, ConsecutiveFailures) is preserved.
type UpsertSpec struct {
	ID          string
	Agent       string
	Name        string
	ShortName   string // slugified from Name when empty
	Description string
	Cron        string
	Prompt      string
	Tasks       []Task
	Model       string
	Trigger     Trigger
	CreatedBy   string // applied only on first create
	Enabled     bool
	Source      string
	SourceRef   string
}

// Upsert creates a workflow at spec.ID or replaces the existing one in
// place. Returns changed=false when the spec matches the stored copy.
func (r *Registry) Upsert(ctx context.Context, spec UpsertSpec) (w *Workflow, changed bool, err error) {
	if !agentValidRe.MatchString(spec.Agent) {
		return nil, false, fmt.Errorf("invalid agent id %q", spec.Agent)
	}
	if !idValidRe.MatchString(spec.ID) {
		return nil, false, fmt.Errorf("invalid workflow id %q", spec.ID)
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, false, fmt.Errorf("workflow name is required")
	}
	hasPrompt := strings.TrimSpace(spec.Prompt) != ""
	hasTasks := len(spec.Tasks) > 0
	if !hasPrompt && !hasTasks {
		return nil, false, fmt.Errorf("workflow requires either prompt or at least one task")
	}
	if hasTasks {
		for i, t := range spec.Tasks {
			if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Prompt) == "" {
				return nil, false, fmt.Errorf("task %d requires non-empty name and prompt", i+1)
			}
		}
	}
	cronExpr := strings.TrimSpace(spec.Cron)
	if cronExpr == "" {
		cronExpr = DefaultCron
	}
	if _, err := cron.ParseStandard(cronExpr); err != nil {
		return nil, false, fmt.Errorf("invalid cron %q: %w", cronExpr, err)
	}
	trig := spec.Trigger
	switch trig.Type {
	case "", TriggerSchedule:
		trig.Type = TriggerSchedule
		trig.Ref = ""
	case TriggerOnSuccess, TriggerOnFailure:
		if !strings.Contains(trig.Ref, "/") {
			return nil, false, fmt.Errorf("trigger ref must be '<agent>/<id>' for %s trigger", trig.Type)
		}
	case TriggerManual:
		trig.Ref = ""
	default:
		return nil, false, fmt.Errorf("unknown trigger type %q", trig.Type)
	}
	shortName := spec.ShortName
	if shortName == "" {
		shortName = slugify(spec.Name)
	}

	k := store.Key(spec.Agent, spec.ID)
	if _, exists := r.docs.Get(k); !exists {
		w := &Workflow{
			ID:          spec.ID,
			Agent:       spec.Agent,
			Name:        spec.Name,
			ShortName:   shortName,
			Description: spec.Description,
			Cron:        cronExpr,
			Prompt:      spec.Prompt,
			Tasks:       spec.Tasks,
			Model:       spec.Model,
			Trigger:     trig,
			CreatedBy:   spec.CreatedBy,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
			Enabled:     spec.Enabled,
			Source:      spec.Source,
			SourceRef:   spec.SourceRef,
		}
		err := r.docs.Create(ctx, k, w)
		if err == nil {
			if w.Enabled {
				r.startRunner(w, false)
			}
			log.Printf("[workflows] upsert created %s/%s (%q, source=%s)", spec.Agent, spec.ID, spec.Name, spec.Source)
			return w, true, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, false, err
		}
	}

	// Distinguish no-op from actual change before writing.
	specHash := upsertFingerprint(spec, cronExpr, shortName, trig)
	var orig Workflow
	updated, err := r.docs.Update(ctx, k, func(w *Workflow) error {
		orig = *w
		if upsertFingerprint(specFromWorkflow(w), w.Cron, w.ShortName, w.Trigger) == specHash {
			return errUnchanged
		}
		w.Name = spec.Name
		w.ShortName = shortName
		w.Description = spec.Description
		w.Cron = cronExpr
		w.Prompt = spec.Prompt
		w.Tasks = append([]Task(nil), spec.Tasks...)
		w.Model = spec.Model
		w.Trigger = trig
		w.Enabled = spec.Enabled
		w.Source = spec.Source
		w.SourceRef = spec.SourceRef
		if spec.Enabled {
			// Re-enable clears any auto-disable banner.
			w.ConsecutiveFailures = 0
			w.DisabledReason = ""
		}
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return &orig, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	r.reconcileRunner(&orig, updated)
	log.Printf("[workflows] upsert updated %s/%s (%q, source=%s)", spec.Agent, spec.ID, spec.Name, spec.Source)
	return updated, true, nil
}

var errUnchanged = errors.New("unchanged")

// ListBySource returns copies of workflows whose Source equals src.
func (r *Registry) ListBySource(src string) []*Workflow {
	out := make([]*Workflow, 0)
	r.docs.Range(func(_ string, w *Workflow) {
		if w.Source != src {
			return
		}
		w.Running = r.isRunning(w.Agent, w.ID)
		out = append(out, w)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// upsertFingerprint hashes the fields that should trigger a write so
// no-op reconciles do not bounce the runner or rewrite the descriptor.
func upsertFingerprint(spec UpsertSpec, cron, shortName string, trig Trigger) string {
	type fp struct {
		Name, ShortName, Description, Cron, Prompt, Model, Source, SourceRef string
		TriggerType, TriggerRef                                              string
		Tasks                                                                []Task
		Enabled                                                              bool
	}
	body, _ := json.Marshal(fp{
		Name:        spec.Name,
		ShortName:   shortName,
		Description: spec.Description,
		Cron:        cron,
		Prompt:      spec.Prompt,
		Model:       spec.Model,
		Source:      spec.Source,
		SourceRef:   spec.SourceRef,
		TriggerType: trig.Type,
		TriggerRef:  trig.Ref,
		Tasks:       spec.Tasks,
		Enabled:     spec.Enabled,
	})
	return string(body)
}

func specFromWorkflow(w *Workflow) UpsertSpec {
	return UpsertSpec{
		ID:          w.ID,
		Agent:       w.Agent,
		Name:        w.Name,
		ShortName:   w.ShortName,
		Description: w.Description,
		Cron:        w.Cron,
		Prompt:      w.Prompt,
		Tasks:       w.Tasks,
		Model:       w.Model,
		Trigger:     w.Trigger,
		CreatedBy:   w.CreatedBy,
		Enabled:     w.Enabled,
		Source:      w.Source,
		SourceRef:   w.SourceRef,
	}
}

// Delete stops the tick goroutine and removes the stored descriptor.
func (r *Registry) Delete(agent, id string) error {
	k := store.Key(agent, id)
	if _, ok := r.docs.Get(k); !ok {
		return fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	r.mu.Lock()
	run := r.runners[key(agent, id)]
	delete(r.runners, key(agent, id))
	r.mu.Unlock()

	if run != nil {
		run.cancel()
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
		}
	}
	if err := r.docs.Delete(context.Background(), k); err != nil {
		return fmt.Errorf("remove workflow: %w", err)
	}
	log.Printf("[workflows] deleted %s/%s", agent, id)
	return nil
}

// StopAll cancels every tick goroutine and stops new ones from starting until
// StartAllEnabled runs again. Safe to call at shutdown.
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

// StartAllEnabled launches runners for every enabled workflow already loaded
// and marks this replica as the one that ticks. This is the server-boot (and
// lease-acquired) entry point, so we pass runInitial=false — loading
// workflows on startup should NOT fire an immediate tick. Ticks will fire on
// their normal schedule (or via manual "run now" from the UI).
func (r *Registry) StartAllEnabled(ctx context.Context) {
	r.mu.Lock()
	r.baseCtx = ctx
	r.active = true
	r.mu.Unlock()
	r.docs.Range(func(_ string, w *Workflow) {
		if w.Enabled {
			r.startRunner(w, false)
		}
	})
}

func (r *Registry) startRunner(w *Workflow, runInitial bool) {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	if old, ok := r.runners[key(w.Agent, w.ID)]; ok {
		old.cancel()
	}
	parent := r.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &runner{cancel: cancel, done: make(chan struct{})}
	r.runners[key(w.Agent, w.ID)] = run
	r.mu.Unlock()

	agent := w.Agent
	id := w.ID
	tType := w.triggerType()

	// Only TriggerSchedule drives a ticker goroutine. Manual + event-triggered
	// workflows sit idle until RunOnce is invoked by the API / event bus.
	if tType != TriggerSchedule {
		// Close done so StopAll / Delete don't wait.
		close(run.done)
		return
	}

	cronExpr := strings.TrimSpace(w.Cron)
	if cronExpr == "" {
		cronExpr = DefaultCron
	}
	schedule, err := cron.ParseStandard(cronExpr)
	if err != nil {
		// Persisted descriptor was already validated on Create/Update,
		// but guard against a tampered file or a future incompatible
		// expression. Log and bail so a bad expression cannot crash the
		// runner; the workflow simply won't tick until the user fixes it.
		log.Printf("[workflows] %s/%s invalid cron %q (%v); runner will not start", agent, id, cronExpr, err)
		close(run.done)
		return
	}

	go func() {
		defer close(run.done)
		defer safego.Recover("workflows: runner " + agent + "/" + id)

		// Catch-up on fresh create or server boot: if there's no LastRun
		// (fresh create) OR the most recent expected fire has been missed
		// since LastRun, fire one tick immediately. Then loop on
		// schedule.Next.
		if runInitial {
			nowUTC := time.Now().UTC()
			var lastRun time.Time
			if w.LastRun != "" {
				lastRun, _ = time.Parse(time.RFC3339, w.LastRun)
			}
			shouldCatchup := false
			reason := ""
			if lastRun.IsZero() {
				shouldCatchup = true
				reason = "fresh create"
			} else if !schedule.Next(lastRun).After(nowUTC) {
				shouldCatchup = true
				reason = fmt.Sprintf("missed expected fire (last_run=%s)", lastRun.UTC().Format(time.RFC3339))
			}
			if shouldCatchup {
				log.Printf("[workflows] %s/%s cron catchup: %s", agent, id, reason)
				// Guarded separately from the runner: this call is inline, so a
				// panic here would abandon the schedule loop below.
				safego.Run("workflows: catchup "+agent+"/"+id, func() {
					_, _ = r.runOnce(ctx, agent, id, "schedule:catchup")
				})
			}
		}

		for {
			next := schedule.Next(time.Now().UTC())
			delay := time.Until(next)
			if delay < 0 {
				delay = 0
			}
			log.Printf("[workflows] %s/%s next cron tick at %s UTC (in %s)", agent, id, next.Format(time.RFC3339), delay.Round(time.Second))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			// Each scheduled tick runs in its own goroutine so a tick
			// that takes longer than the inter-fire delta does not block
			// later ticks of THIS workflow from starting on time, and
			// does not block any other workflow's runner (each workflow
			// has its own startRunner goroutine). runOnce uses the per-
			// workflow `busy` try-lock so an overlapping tick is skipped
			// rather than run concurrently.
			safego.Go("workflows: tick "+agent+"/"+id, func() {
				_, _ = r.runOnce(ctx, agent, id, "schedule")
			})
		}
	}()
}

// RunOnce executes a workflow a single time outside its normal schedule.
// Returns the run's result (or an error). Used by manual API invocations
// and by event-triggered listeners.
func (r *Registry) RunOnce(ctx context.Context, agent, id, trigger string) (string, error) {
	if _, ok := r.docs.Get(store.Key(agent, id)); !ok {
		return "", fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	return r.runOnce(ctx, agent, id, trigger)
}

// runOnce performs the actual execution. For multi-task workflows each task
// is executed in order with prior outputs threaded into the prompt.
//
// A per-workflow `busy` try-lock on this replica plus a per-workflow lease in
// the bucket prevent the same workflow from running concurrently with
// itself, here or on another replica. Overlapping triggers are skipped with
// a logged warning — this protects against double-posts, double-PRs, and
// write races that would otherwise happen if a slow tick (or manual /run)
// overlapped another trigger.
func (r *Registry) runOnce(ctx context.Context, agent, id, triggeredBy string) (string, error) {
	orig, ok := r.docs.Get(store.Key(agent, id))
	if !ok {
		return "", fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	r.mu.RLock()
	exec := r.executor
	run := r.runners[key(agent, id)]
	r.mu.RUnlock()
	if exec == nil {
		return "", fmt.Errorf("workflow executor not configured")
	}
	if run != nil {
		if !run.busy.CompareAndSwap(0, 1) {
			log.Printf("[workflows] skip %s/%s (%s): already running", agent, id, triggeredBy)
			return "", fmt.Errorf("workflow %s/%s is already running", agent, id)
		}
		defer run.busy.Store(0)
	}
	lease := store.NewLease(r.b, "locks/workflows/"+agent+"/"+id, store.InstanceID(), runLeaseTTL)
	held, release, acquired, err := lease.Acquire(ctx)
	switch {
	case err != nil:
		log.Printf("[workflows] %s/%s run lease unavailable, continuing without it: %v", agent, id, err)
	case !acquired:
		log.Printf("[workflows] skip %s/%s (%s): running on another replica", agent, id, triggeredBy)
		return "", fmt.Errorf("workflow %s/%s is already running", agent, id)
	default:
		defer release()
		ctx = held
	}
	w := *orig
	if triggeredBy == "" {
		triggeredBy = w.triggerType()
	}

	start := time.Now()
	log.Printf("[workflows] run %s/%s (%q, pattern=%s, trigger=%s)", agent, id, w.Name, w.Pattern(), triggeredBy)

	entry := RunLog{
		StartedAt:   start.UTC().Format(time.RFC3339),
		TriggeredBy: triggeredBy,
	}

	var (
		finalResult string
		firstError  error
	)

	if len(w.Tasks) > 0 {
		// Flow of subflows: sequentially execute each task, feeding prior
		// outputs forward as a compact "Previous task results" prelude.
		var prior []string
		for _, task := range w.Tasks {
			tStart := time.Now()
			composed := withClock(composeTaskPrompt(task, prior))
			out, err := exec.Run(ctx, &w, composed)
			tr := TaskResult{
				Name:       task.Name,
				DurationMS: time.Since(tStart).Milliseconds(),
				Result:     out,
			}
			if err != nil {
				tr.Error = err.Error()
				if firstError == nil {
					firstError = err
				}
				entry.Tasks = append(entry.Tasks, tr)
				// Stop the chain on the first task failure to avoid cascading
				// nonsense into dependent tasks.
				break
			}
			entry.Tasks = append(entry.Tasks, tr)
			prior = append(prior, fmt.Sprintf("[%s]\n%s", task.Name, out))
			finalResult = out
		}
	} else {
		out, err := exec.Run(ctx, &w, withClock(w.Prompt))
		finalResult = out
		firstError = err
	}

	duration := time.Since(start)
	if len(finalResult) > MaxResultChars {
		finalResult = finalResult[:MaxResultChars] + "\n…(truncated)"
	}
	entry.DurationMS = duration.Milliseconds()
	entry.Result = finalResult
	if firstError != nil {
		entry.Error = firstError.Error()
		log.Printf("[workflows] run %s/%s failed after %s: %v", agent, id, duration.Round(time.Millisecond), firstError)
	} else {
		log.Printf("[workflows] run %s/%s completed in %s (result: %d chars)",
			agent, id, duration.Round(time.Millisecond), len(finalResult))
	}

	// Record the outcome against the latest stored descriptor so an edit made
	// while the tick was running (here or on another replica) is kept. The
	// write uses its own context: the run's may already be cancelled.
	pctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	autoDisabled := false
	_, perr := r.docs.Update(pctx, store.Key(agent, id), func(w *Workflow) error {
		w.LastRun = entry.StartedAt
		w.LastResult = finalResult
		if firstError != nil {
			w.LastError = firstError.Error()
			w.ConsecutiveFailures++
		} else {
			w.LastError = ""
			w.ConsecutiveFailures = 0
			// A successful run clears any prior auto-disable reason. (It is
			// only actually set when Enabled=false, so a clean success after a
			// user re-enables the workflow will wipe the stale banner.)
			w.DisabledReason = ""
		}
		// Auto-disable after too many consecutive failures. Only applies to
		// workflows that are currently enabled — we don't want to clobber a
		// user's manual pause with an auto-disable reason.
		autoDisabled = false
		if w.Enabled && w.ConsecutiveFailures >= MaxConsecutiveFailures {
			w.Enabled = false
			w.DisabledReason = fmt.Sprintf(
				"auto-disabled after %d consecutive failed ticks. Last error: %s. "+
					"Re-enable via update_workflow once the underlying issue is fixed.",
				w.ConsecutiveFailures, w.LastError)
			autoDisabled = true
		}
		w.Runs = append([]RunLog{entry}, w.Runs...)
		if len(w.Runs) > MaxRunHistory {
			w.Runs = w.Runs[:MaxRunHistory]
		}
		return nil
	})
	if perr != nil {
		log.Printf("[workflows] persist %s/%s failed: %v", agent, id, perr)
	} else if autoDisabled {
		log.Printf("[workflows] AUTO-DISABLED %s/%s after %d consecutive failures: %v",
			agent, id, MaxConsecutiveFailures, firstError)
		// The runner goroutine exits on ctx.Done(); our defer on
		// run.busy.Store(0) above already keeps the try-lock consistent.
		r.stopRunner(agent, id)
	}

	// Emit event to any listener workflows.
	r.fireListeners(ctx, agent, id, firstError == nil)

	return finalResult, firstError
}

// fireListeners runs any workflow whose Trigger matches the just-finished
// workflow. Listeners execute in their own goroutines so a slow listener
// cannot back up the source workflow's tick loop.
func (r *Registry) fireListeners(parent context.Context, srcAgent, srcID string, ok bool) {
	wantType := TriggerOnSuccess
	if !ok {
		wantType = TriggerOnFailure
	}
	ref := srcAgent + "/" + srcID
	var listeners []struct{ agent, id string }
	r.docs.Range(func(_ string, w *Workflow) {
		if w.Enabled && w.Trigger.Type == wantType && w.Trigger.Ref == ref {
			listeners = append(listeners, struct{ agent, id string }{w.Agent, w.ID})
		}
	})
	for _, l := range listeners {
		agent, id := l.agent, l.id
		safego.Go("workflows: listener "+agent+"/"+id, func() {
			// Decouple from the parent tick context so listener cancellation
			// does not cascade; use a fresh timeout-bounded context.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_, _ = r.runOnce(ctx, agent, id, wantType+":"+ref)
		})
	}
	_ = parent
}

func composeTaskPrompt(task Task, prior []string) string {
	if len(prior) == 0 {
		return task.Prompt
	}
	joined := strings.Join(prior, "\n\n")
	if len(joined) > MaxTaskContextChars {
		joined = joined[:MaxTaskContextChars] + "\n…(truncated)"
	}
	return fmt.Sprintf("Previous task results:\n%s\n\n---\nCurrent task: %s\n%s", joined, task.Name, task.Prompt)
}

// withClock prepends the current wall-clock time to a prompt. LLMs trained
// months or years ago otherwise default to their training cutoff when asked
// to compute "yesterday" or "2 days ago" — which is the wrong baseline for
// any workflow that queries a live API for date-bounded data (AWS cost
// reports, Slack windows, commit digests, etc.). Adding the real timestamp
// to every tick is an ~80-byte prepend that fixes this for all workflows.
func withClock(prompt string) string {
	now := time.Now().UTC()
	return fmt.Sprintf(
		"Current UTC time: %s (date: %s, weekday: %s). Use THIS as today's date — do not rely on your training cutoff.\n\n---\n%s",
		now.Format("2006-01-02 15:04:05"),
		now.Format("2006-01-02"),
		now.Weekday(),
		prompt,
	)
}

func newID() (string, error) { return store.NewID() }

func slugify(s string) string { return store.Slugify(s, "workflow") }
