// Package workflows provides a lightweight scheduled-agent engine for arbetern.
//
// Each workflow is a small JSON descriptor stored at
//
//	<WORKFLOWS_DIR>/<agent>/<workflow-id>.json
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
package workflows

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
	"sync/atomic"
	"time"
)

const (
	// DefaultDir is used when WORKFLOWS_DIR is unset.
	DefaultDir = "./data/workflows"

	// MinInterval is the shortest allowed trigger interval.
	MinInterval = 1 * time.Minute
	// MaxInterval caps runaway intervals at one week.
	MaxInterval = 7 * 24 * time.Hour
	// DefaultInterval is used when the caller omits or provides an invalid value.
	DefaultInterval = 5 * time.Minute

	// MaxRunHistory bounds the per-workflow on-disk run log.
	MaxRunHistory = 20
	// MaxResultChars bounds a single stored run's result text.
	MaxResultChars = 8000
	// MaxTaskContextChars bounds the prior-task summary fed into the next task.
	MaxTaskContextChars = 2000

	// TriggerSchedule runs the workflow on its Interval.
	TriggerSchedule = "schedule"
	// TriggerOnSuccess runs the workflow right after Trigger.Ref succeeds.
	TriggerOnSuccess = "on_success"
	// TriggerOnFailure runs the workflow right after Trigger.Ref fails.
	TriggerOnFailure = "on_failure"
	// TriggerManual disables automatic ticks; only the manual /run endpoint
	// or a call_workflow tool call can fire the workflow.
	TriggerManual = "manual"
)

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
	ID          string   `json:"id"`
	Agent       string   `json:"agent"`
	Name        string   `json:"name"`
	ShortName   string   `json:"short_name"`
	Description string   `json:"description,omitempty"`
	Interval    string   `json:"interval"`
	Prompt      string   `json:"prompt,omitempty"`
	Tasks       []Task   `json:"tasks,omitempty"`
	Trigger     Trigger  `json:"trigger,omitempty"`
	CreatedBy   string   `json:"created_by,omitempty"`
	CreatedAt   string   `json:"created_at"`
	Enabled     bool     `json:"enabled"`
	LastRun     string   `json:"last_run,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	LastResult  string   `json:"last_result,omitempty"`
	Runs        []RunLog `json:"runs,omitempty"`
}

// ViewURL returns the path to the HTML view for this workflow.
func (w *Workflow) ViewURL() string {
	return fmt.Sprintf("/%s/workflow/%s", w.Agent, w.ID)
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

// intervalDur parses Interval, returning DefaultInterval on failure.
func (w *Workflow) intervalDur() time.Duration {
	dur, err := time.ParseDuration(w.Interval)
	if err != nil || dur < MinInterval {
		return DefaultInterval
	}
	if dur > MaxInterval {
		return MaxInterval
	}
	return dur
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
	// tick whose interval elapses before it finishes) do not spawn a
	// concurrent second run of the same workflow, which would double-post
	// to Slack, double-create PRs, and race on persistence.
	busy atomic.Int32
}

// Registry is the thread-safe in-memory index of workflows plus their goroutines.
type Registry struct {
	mu       sync.RWMutex
	dir      string
	executor Executor
	items    map[string]*Workflow // key: agent/id
	runners  map[string]*runner   // key: agent/id
}

// New creates a Registry rooted at dir. It creates the directory if missing.
func New(dir string, exec Executor) (*Registry, error) {
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workflows dir: %w", err)
	}
	r := &Registry{
		dir:      dir,
		executor: exec,
		items:    make(map[string]*Workflow),
		runners:  make(map[string]*runner),
	}
	return r, nil
}

// Dir returns the root directory managed by the registry.
func (r *Registry) Dir() string { return r.dir }

// SetExecutor installs the executor post-construction. Useful when the
// executor depends on components (e.g. the router map) that are built after
// the registry.
func (r *Registry) SetExecutor(exec Executor) {
	r.mu.Lock()
	r.executor = exec
	r.mu.Unlock()
}

func key(agent, id string) string { return agent + "/" + id }

var agentValidRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var idValidRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// LoadAll scans the workflows directory and kicks off tick goroutines.
// Invalid files are logged and skipped; they do not prevent startup.
func (r *Registry) LoadAll(ctx context.Context) error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("read workflows dir: %w", err)
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
			w, err := readFile(path)
			if err != nil {
				log.Printf("[workflows] skipping %s: %v", path, err)
				continue
			}
			r.mu.Lock()
			r.items[key(w.Agent, w.ID)] = w
			r.mu.Unlock()
			log.Printf("[workflows] loaded %s/%s (%q, pattern=%s, enabled=%t)", w.Agent, w.ID, w.Name, w.Pattern(), w.Enabled)
		}
	}
	return nil
}

// CreateOpts captures the full set of knobs for Create. Fields left empty
// fall back to sensible defaults (TriggerSchedule, DefaultInterval, etc.).
type CreateOpts struct {
	Agent       string
	CreatedBy   string
	Name        string
	ShortName   string
	Description string
	Interval    string
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
	interval := opts.Interval
	if _, err := time.ParseDuration(interval); err != nil {
		interval = DefaultInterval.String()
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
		Interval:    interval,
		Prompt:      opts.Prompt,
		Tasks:       opts.Tasks,
		Trigger:     trig,
		CreatedBy:   opts.CreatedBy,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Enabled:     true,
	}
	if err := r.persist(w); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.items[key(opts.Agent, id)] = w
	r.mu.Unlock()
	r.startRunner(ctx, w)
	log.Printf("[workflows] created %s/%s (%q, pattern=%s, every %s)", opts.Agent, id, opts.Name, w.Pattern(), w.intervalDur())
	return w, nil
}

// Get returns a copy of the stored workflow, or (nil,false) if missing.
func (r *Registry) Get(agent, id string) (*Workflow, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.items[key(agent, id)]
	if !ok {
		return nil, false
	}
	cp := *w
	return &cp, true
}

// List returns all workflows, optionally filtered by agent, sorted by creation time asc.
func (r *Registry) List(agent string) []*Workflow {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Workflow, 0, len(r.items))
	for _, w := range r.items {
		if agent != "" && w.Agent != agent {
			continue
		}
		cp := *w
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
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
	Interval    *string
	Prompt      *string
	Tasks       *[]Task  // nil = unchanged; non-nil (even empty) = replace
	Trigger     *Trigger // nil = unchanged
	Enabled     *bool
}

// Update applies a partial edit to an existing workflow. The tick goroutine
// is restarted so the new prompt / interval / trigger take effect on the next
// scheduled run. Run history, last-run timestamp, and created metadata are
// preserved. Fields left as nil pointers are untouched.
func (r *Registry) Update(ctx context.Context, agent, id string, opts UpdateOpts) (*Workflow, error) {
	r.mu.Lock()
	orig, ok := r.items[key(agent, id)]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	updated := *orig // copy, then mutate, then swap in under the lock
	r.mu.Unlock()

	if opts.Name != nil {
		if strings.TrimSpace(*opts.Name) == "" {
			return nil, fmt.Errorf("workflow name cannot be empty")
		}
		updated.Name = *opts.Name
	}
	if opts.Description != nil {
		updated.Description = *opts.Description
	}
	if opts.Interval != nil {
		if _, err := time.ParseDuration(*opts.Interval); err != nil {
			return nil, fmt.Errorf("invalid interval %q: %w", *opts.Interval, err)
		}
		updated.Interval = *opts.Interval
	}
	if opts.Prompt != nil {
		updated.Prompt = *opts.Prompt
	}
	if opts.Tasks != nil {
		for i, t := range *opts.Tasks {
			if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Prompt) == "" {
				return nil, fmt.Errorf("task %d requires non-empty name and prompt", i+1)
			}
		}
		updated.Tasks = *opts.Tasks
	}
	if strings.TrimSpace(updated.Prompt) == "" && len(updated.Tasks) == 0 {
		return nil, fmt.Errorf("workflow requires either prompt or at least one task after update")
	}
	if opts.Trigger != nil {
		trig := *opts.Trigger
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
		updated.Trigger = trig
	}
	if opts.Enabled != nil {
		updated.Enabled = *opts.Enabled
	}

	if err := r.persist(&updated); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.items[key(agent, id)] = &updated
	r.mu.Unlock()

	// Restart the tick goroutine so schedule/trigger/prompt changes take
	// effect immediately. startRunner already cancels any prior runner for
	// this key before starting a new one.
	if updated.Enabled {
		r.startRunner(ctx, &updated)
	} else {
		// Disabled: stop any running ticker and leave the entry in place.
		r.mu.Lock()
		if run, ok := r.runners[key(agent, id)]; ok {
			run.cancel()
			delete(r.runners, key(agent, id))
		}
		r.mu.Unlock()
	}
	log.Printf("[workflows] updated %s/%s (%q, pattern=%s, every %s, enabled=%t)",
		agent, id, updated.Name, updated.Pattern(), updated.intervalDur(), updated.Enabled)
	cp := updated
	return &cp, nil
}

// Delete stops the tick goroutine, removes the file, and deletes from memory.
func (r *Registry) Delete(agent, id string) error {
	r.mu.Lock()
	w, ok := r.items[key(agent, id)]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	delete(r.items, key(agent, id))
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
	path := r.pathFor(w)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove workflow file: %w", err)
	}
	log.Printf("[workflows] deleted %s/%s", agent, id)
	return nil
}

// StopAll cancels every tick goroutine. Safe to call at shutdown.
func (r *Registry) StopAll() {
	r.mu.Lock()
	runs := r.runners
	r.runners = make(map[string]*runner)
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
}

// StartAllEnabled launches runners for every enabled workflow already loaded.
// Used when the executor is installed after LoadAll.
func (r *Registry) StartAllEnabled(ctx context.Context) {
	r.mu.RLock()
	list := make([]*Workflow, 0, len(r.items))
	for _, w := range r.items {
		if w.Enabled {
			cp := *w
			list = append(list, &cp)
		}
	}
	r.mu.RUnlock()
	for _, w := range list {
		r.startRunner(ctx, w)
	}
}

func (r *Registry) startRunner(parent context.Context, w *Workflow) {
	r.mu.Lock()
	if old, ok := r.runners[key(w.Agent, w.ID)]; ok {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &runner{cancel: cancel, done: make(chan struct{})}
	r.runners[key(w.Agent, w.ID)] = run
	r.mu.Unlock()

	agent := w.Agent
	id := w.ID
	tType := w.triggerType()
	interval := w.intervalDur()

	// Only TriggerSchedule drives a ticker goroutine. Manual + event-triggered
	// workflows sit idle until RunOnce is invoked by the API / event bus.
	if tType != TriggerSchedule {
		// Close done so StopAll / Delete don't wait.
		close(run.done)
		return
	}

	go func() {
		defer close(run.done)
		// Fire an initial tick so the user sees activity without waiting a
		// full interval for the first execution. The initial tick is run in
		// this goroutine so the ticker only starts after it completes — the
		// ticker then represents the cadence of scheduled ticks relative to
		// that baseline.
		_, _ = r.runOnce(ctx, agent, id, "schedule:initial")

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Each scheduled tick runs in its own goroutine so a tick
				// that takes longer than the interval does not block later
				// ticks of THIS workflow from starting on time, and does
				// not block any other workflow's ticker (each workflow has
				// its own startRunner goroutine). runOnce uses the per-
				// workflow `busy` try-lock so an overlapping tick is
				// skipped rather than run concurrently.
				go func() {
					_, _ = r.runOnce(ctx, agent, id, "schedule")
				}()
			}
		}
	}()
}

// RunOnce executes a workflow a single time outside its normal schedule.
// Returns the run's result (or an error). Used by manual API invocations
// and by event-triggered listeners.
func (r *Registry) RunOnce(ctx context.Context, agent, id, trigger string) (string, error) {
	r.mu.RLock()
	_, ok := r.items[key(agent, id)]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("workflow %s/%s not found", agent, id)
	}
	return r.runOnce(ctx, agent, id, trigger)
}

// runOnce performs the actual execution. For multi-task workflows each task
// is executed in order with prior outputs threaded into the prompt.
//
// A per-workflow `busy` try-lock prevents the same workflow from running
// concurrently with itself. Overlapping triggers are skipped with a logged
// warning — this protects against double-posts, double-PRs, and persistence
// races that would otherwise happen if a slow tick (or manual /run)
// overlapped another trigger.
func (r *Registry) runOnce(ctx context.Context, agent, id, triggeredBy string) (string, error) {
	r.mu.RLock()
	orig, ok := r.items[key(agent, id)]
	exec := r.executor
	run := r.runners[key(agent, id)]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("workflow %s/%s not found", agent, id)
	}
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
			composed := composeTaskPrompt(task, prior)
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
		out, err := exec.Run(ctx, &w, w.Prompt)
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

	w.LastRun = entry.StartedAt
	w.LastResult = finalResult
	if firstError != nil {
		w.LastError = firstError.Error()
	} else {
		w.LastError = ""
	}

	w.Runs = append([]RunLog{entry}, w.Runs...)
	if len(w.Runs) > MaxRunHistory {
		w.Runs = w.Runs[:MaxRunHistory]
	}

	r.mu.Lock()
	r.items[key(agent, id)] = &w
	r.mu.Unlock()

	if perr := r.persist(&w); perr != nil {
		log.Printf("[workflows] persist %s/%s failed: %v", agent, id, perr)
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
	r.mu.RLock()
	for _, w := range r.items {
		if !w.Enabled {
			continue
		}
		if w.Trigger.Type == wantType && w.Trigger.Ref == ref {
			listeners = append(listeners, struct{ agent, id string }{w.Agent, w.ID})
		}
	}
	r.mu.RUnlock()
	for _, l := range listeners {
		go func(agent, id string) {
			// Decouple from the parent tick context so listener cancellation
			// does not cascade; use a fresh timeout-bounded context.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_, _ = r.runOnce(ctx, agent, id, wantType+":"+ref)
		}(l.agent, l.id)
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

func (r *Registry) persist(w *Workflow) error {
	dir := filepath.Join(r.dir, w.Agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := r.pathFor(w)
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *Registry) pathFor(w *Workflow) string {
	return filepath.Join(r.dir, w.Agent, w.ID+".json")
}

func readFile(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var w Workflow
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	if w.ID == "" || w.Agent == "" || !idValidRe.MatchString(w.ID) || !agentValidRe.MatchString(w.Agent) {
		return nil, fmt.Errorf("invalid workflow descriptor")
	}
	return &w, nil
}

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
		s = "workflow"
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}
