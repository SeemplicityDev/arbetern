package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/queue"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/sessiontoken"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/ttlcache"
)

var errNotFound = errors.New("project not found")

// Deps are the services a Registry works with.
type Deps struct {
	Backend      *store.Backend
	GitHub       *github.Client                          // nil → create/PR features answer a clear error
	Datadog      func(agent string) *datadog.MultiClient // agent-scoped clients (may return nil)
	Skills       func(agent string) []string             // skills.Registry.Instructions
	AgentAllowed func(agent string) bool                 // whether the agent may use Datadog
	KnownAgents  map[string]bool
	Fire         Dispatcher // nil → default routine client
}

type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type stateKey struct {
	agent, id string
	gen       uint64
}

type projectState struct {
	ledger *Ledger
	memory *Memory
}

// Registry caches the stored projects and runs their ticks.
type Registry struct {
	deps Deps
	b    *store.Backend
	gh   *github.Client
	fire Dispatcher
	docs *store.Documents[Project]

	mu      sync.RWMutex
	runners map[string]*runner
	busy    map[string]bool
	gens    map[string]uint64
	baseCtx context.Context
	active  bool
	tasks   *queue.Queue

	states *ttlcache.Keyed[stateKey, *projectState]
	login  *ttlcache.Cache[string]
}

type tickTask struct {
	Agent   string `json:"agent"`
	ID      string `json:"id"`
	Trigger string `json:"trigger"`
}

// New creates a Registry; call Load before serving.
func New(deps Deps) *Registry {
	r := &Registry{
		deps:    deps,
		b:       deps.Backend,
		gh:      deps.GitHub,
		fire:    deps.Fire,
		runners: map[string]*runner{},
		busy:    map[string]bool{},
		gens:    map[string]uint64{},
	}
	if r.fire == nil {
		r.fire = newRoutineClient()
	}
	r.docs = store.NewDocuments(deps.Backend, Prefix, store.AgentKeyRe, func(p *Project) error {
		if !store.AgentRe.MatchString(p.Agent) || !store.IDRe.MatchString(p.ID) {
			return errors.New("invalid project descriptor")
		}
		return nil
	})
	r.states = ttlcache.NewKeyed(viewCacheTTL, 256, r.fetchState)
	r.login = ttlcache.New(time.Hour, func(ctx context.Context) (string, error) {
		if r.gh == nil {
			return "", errors.New("GitHub is not configured")
		}
		return r.gh.GetAuthenticatedUser(ctx)
	})
	return r
}

func key(agent, id string) string { return agent + "/" + id }

// Load reads every stored project into the cache without starting runners.
func (r *Registry) Load(ctx context.Context) error {
	if err := r.docs.Load(ctx); err != nil {
		return fmt.Errorf("load projects: %w", err)
	}
	r.docs.Range(func(_ string, p *Project) {
		log.Printf("[projects] loaded %s/%s (%q, repo=%s/%s, enabled=%t)", p.Agent, p.ID, p.Name, p.Repo.Owner, p.Repo.Name, p.Enabled)
	})
	return nil
}

// Count is the number of stored projects.
func (r *Registry) Count() int { return r.docs.Len() }

// UseQueue routes manual runs through the durable queue so they land on any replica.
func (r *Registry) UseQueue(q *queue.Queue) {
	if r == nil || q == nil {
		return
	}
	r.mu.Lock()
	r.tasks = q
	r.mu.Unlock()
	q.Register(QueueTopic, queue.Options{Timeout: queueTimeout, MaxAttempts: 1}, r.runQueued)
}

func (r *Registry) runQueued(ctx context.Context, payload []byte) error {
	var t tickTask
	if err := json.Unmarshal(payload, &t); err != nil {
		return err
	}
	if !store.AgentRe.MatchString(t.Agent) || !store.IDRe.MatchString(t.ID) {
		return fmt.Errorf("invalid project reference %q/%q", t.Agent, t.ID)
	}
	r.tick(ctx, t.Agent, t.ID, t.Trigger)
	return nil
}

func (r *Registry) trigger(ctx context.Context, agent, id string) error {
	r.mu.RLock()
	q := r.tasks
	r.mu.RUnlock()
	if q != nil {
		err := q.Enqueue(ctx, QueueTopic, tickTask{Agent: agent, ID: id, Trigger: "manual"})
		if err == nil {
			return nil
		}
		log.Printf("[projects] queueing a tick of %s/%s failed, running locally: %v", agent, id, err)
	}
	safego.Go("projects: tick "+key(agent, id), func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueTimeout)
		defer cancel()
		r.tick(tctx, agent, id, "manual")
	})
	return nil
}

// StartRefresh keeps the cache in step with other replicas and reconciles runners on the scheduling leader.
func (r *Registry) StartRefresh(ctx context.Context, interval time.Duration) {
	r.docs.StartRefresh(ctx, interval, r.applyChanges)
}

func (r *Registry) applyChanges(changes []store.Change[Project]) {
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
				r.startRunner(c.New)
			}
		default:
			r.reconcileRunner(c.Old, c.New)
		}
	}
}

func (r *Registry) reconcileRunner(orig, updated *Project) {
	switch {
	case !updated.Enabled:
		if orig.Enabled {
			r.stopRunner(updated.Agent, updated.ID)
		}
	case !orig.Enabled || orig.Interval != updated.Interval:
		r.startRunner(updated)
	}
}

// StartAll starts a runner for every enabled project; call it while holding the scheduling lease.
func (r *Registry) StartAll(ctx context.Context) {
	r.mu.Lock()
	r.baseCtx = ctx
	r.active = true
	r.mu.Unlock()
	r.docs.Range(func(_ string, p *Project) {
		if p.Enabled {
			r.startRunner(p)
		}
	})
}

// StopAll cancels every runner until StartAll runs again.
func (r *Registry) StopAll() {
	r.mu.Lock()
	r.active = false
	runs := r.runners
	r.runners = map[string]*runner{}
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
}

func (r *Registry) startRunner(p *Project) {
	k := key(p.Agent, p.ID)
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	if old, ok := r.runners[k]; ok {
		old.cancel()
	}
	parent := r.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &runner{cancel: cancel, done: make(chan struct{})}
	r.runners[k] = run
	r.mu.Unlock()

	agent, id, every := p.Agent, p.ID, p.interval()
	go func() {
		defer close(run.done)
		defer safego.Recover("projects: runner " + k)
		wait := rand.N(maxStagger)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			safego.Run("projects: tick "+k, func() { r.tick(ctx, agent, id, "schedule") })
			wait = every
		}
	}()
}

func (r *Registry) stopRunner(agent, id string) *runner {
	r.mu.Lock()
	run, ok := r.runners[key(agent, id)]
	delete(r.runners, key(agent, id))
	r.mu.Unlock()
	if !ok {
		return nil
	}
	run.cancel()
	return run
}

func (r *Registry) tryBusy(k string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[k] {
		return false
	}
	r.busy[k] = true
	return true
}

func (r *Registry) releaseBusy(k string) {
	r.mu.Lock()
	delete(r.busy, k)
	r.mu.Unlock()
}

func (r *Registry) knownAgent(agent string) bool {
	return store.AgentRe.MatchString(agent) && r.deps.KnownAgents[agent]
}

func (r *Registry) checkAgent(agent string) error {
	if !r.knownAgent(agent) {
		return invalidf("unknown agent %q", agent)
	}
	if r.deps.AgentAllowed != nil && !r.deps.AgentAllowed(agent) {
		return invalidf("agent %s may not use Datadog, which projects need", agent)
	}
	return nil
}

func (r *Registry) datadogFor(agent, site string) *datadog.Client {
	if r.deps.Datadog == nil {
		return nil
	}
	return r.deps.Datadog(agent).ForSite(site)
}

func (r *Registry) checkSite(agent, site string) error {
	if r.datadogFor(agent, site) == nil {
		return invalidf("agent %s has no Datadog credentials for site %s", agent, site)
	}
	return nil
}

func (r *Registry) skillsFor(agent string) []string {
	if r.deps.Skills == nil {
		return nil
	}
	return r.deps.Skills(agent)
}

func (r *Registry) defaultBranch(ctx context.Context, repo Repo) (string, error) {
	if r.gh == nil {
		return "", invalidf("GitHub is not configured in this deployment, so repo.base_branch cannot be resolved")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	base, err := r.gh.GetDefaultBranch(ctx, repo.Owner, repo.Name)
	if err != nil {
		return "", invalidf("could not read the default branch of %s/%s: %v", repo.Owner, repo.Name, err)
	}
	if !validBranchName(base) {
		return "", invalidf("the default branch of %s/%s is not a usable branch name", repo.Owner, repo.Name)
	}
	return base, nil
}

func (r *Registry) tokenLogin(ctx context.Context) string {
	login, err := r.login.Get(ctx)
	if err != nil {
		log.Printf("[projects] reading the GitHub token login: %v", err)
		return ""
	}
	return login
}

func (r *Registry) create(ctx context.Context, draft Project, enabled bool, createdBy string) (*Project, error) {
	if err := r.checkAgent(draft.Agent); err != nil {
		return nil, err
	}
	if err := draft.normalize(); err != nil {
		return nil, err
	}
	if err := r.checkSite(draft.Agent, draft.Signal.Site); err != nil {
		return nil, err
	}
	if r.gh == nil {
		return nil, invalidf("GitHub is not configured in this deployment; projects need it to open pull requests")
	}
	if draft.Repo.BaseBranch == "" {
		base, err := r.defaultBranch(ctx, draft.Repo)
		if err != nil {
			return nil, err
		}
		draft.Repo.BaseBranch = base
	}
	id, err := store.NewID()
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID:           id,
		Agent:        draft.Agent,
		Name:         draft.Name,
		Description:  draft.Description,
		Goal:         draft.Goal,
		Instructions: draft.Instructions,
		Repo:         draft.Repo,
		Signal:       draft.Signal,
		Dispatch:     draft.Dispatch,
		Limits:       draft.Limits,
		Interval:     draft.Interval,
		Enabled:      enabled,
		CreatedBy:    createdBy,
		CreatedAt:    stamp(time.Now()),
	}
	if err := r.docs.Create(ctx, store.Key(p.Agent, p.ID), p); err != nil {
		return nil, err
	}
	if p.Enabled {
		r.startRunner(p)
	}
	log.Printf("[projects] created %s/%s (%q, repo=%s/%s, routine=%s)", p.Agent, p.ID, p.Name, p.Repo.Owner, p.Repo.Name, p.Dispatch.RoutineID)
	cp := *p
	return &cp, nil
}

type projectPatch struct {
	Name, Description, Goal, Instructions, Interval *string
	Repo                                            *Repo
	Signal                                          *Signal
	Dispatch                                        *Dispatch
	Limits                                          *Limits
	Enabled                                         *bool
}

func (pt projectPatch) applyTo(dst *Project) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&dst.Name, pt.Name)
	set(&dst.Description, pt.Description)
	set(&dst.Goal, pt.Goal)
	set(&dst.Instructions, pt.Instructions)
	set(&dst.Interval, pt.Interval)
	if pt.Repo != nil {
		dst.Repo = *pt.Repo
	}
	if pt.Signal != nil {
		dst.Signal = *pt.Signal
	}
	if pt.Dispatch != nil {
		dst.Dispatch = *pt.Dispatch
	}
	if pt.Limits != nil {
		dst.Limits = *pt.Limits
	}
}

func (pt projectPatch) normalized(draft *Project) projectPatch {
	pick := func(present *string, v string) *string {
		if present == nil {
			return nil
		}
		return &v
	}
	out := projectPatch{
		Name:         pick(pt.Name, draft.Name),
		Description:  pick(pt.Description, draft.Description),
		Goal:         pick(pt.Goal, draft.Goal),
		Instructions: pick(pt.Instructions, draft.Instructions),
		Interval:     pick(pt.Interval, draft.Interval),
		Enabled:      pt.Enabled,
	}
	if pt.Repo != nil {
		out.Repo = &draft.Repo
	}
	if pt.Signal != nil {
		out.Signal = &draft.Signal
	}
	if pt.Dispatch != nil {
		out.Dispatch = &draft.Dispatch
	}
	if pt.Limits != nil {
		out.Limits = &draft.Limits
	}
	return out
}

func (r *Registry) update(ctx context.Context, agent, id string, pt projectPatch) (*Project, error) {
	cur, err := r.project(ctx, agent, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	draft := *cur
	pt.applyTo(&draft)
	if err := draft.normalize(); err != nil {
		return nil, err
	}
	if pt.Signal != nil {
		if err := r.checkSite(agent, draft.Signal.Site); err != nil {
			return nil, err
		}
	}
	if pt.Repo != nil && draft.Repo.BaseBranch == "" {
		base, err := r.defaultBranch(ctx, draft.Repo)
		if err != nil {
			return nil, err
		}
		draft.Repo.BaseBranch = base
	}
	norm := pt.normalized(&draft)
	now := time.Now()
	updated, err := r.updateDescriptor(ctx, agent, id, func(p *Project) error {
		norm.applyTo(p)
		if norm.Enabled != nil {
			p.Enabled = *norm.Enabled
			if p.Enabled {
				p.DisabledReason = ""
				p.ConsecutiveFailures = 0
			}
		}
		p.UpdatedAt = stamp(now)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	r.touch(agent, id)
	log.Printf("[projects] updated %s/%s (%q, enabled=%t)", agent, id, updated.Name, updated.Enabled)
	return updated, nil
}

// updateDescriptor reconciles the runner with what was stored, which may include another replica's edit that Update folded in.
func (r *Registry) updateDescriptor(ctx context.Context, agent, id string, fn func(*Project) error) (*Project, error) {
	k := store.Key(agent, id)
	before, cached := r.docs.Get(k)
	updated, err := r.docs.Update(ctx, k, fn)
	if errors.Is(err, store.ErrNotFound) {
		r.stopRunner(agent, id)
	}
	if err != nil {
		return nil, err
	}
	if !cached {
		before = &Project{}
	}
	r.reconcileRunner(before, updated)
	return updated, nil
}

// project reads the bucket when the cache has not picked up a descriptor another replica created yet.
func (r *Registry) project(ctx context.Context, agent, id string) (*Project, error) {
	if p, ok := r.docs.Get(store.Key(agent, id)); ok {
		return p, nil
	}
	return r.fetchProject(ctx, agent, id)
}

func (r *Registry) fetchProject(ctx context.Context, agent, id string) (*Project, error) {
	p, _, err := store.GetJSON[Project](ctx, r.b, Prefix+store.Key(agent, id))
	if err != nil {
		return nil, err
	}
	if p.Agent != agent || p.ID != id {
		return nil, errors.New("invalid project descriptor")
	}
	return p, nil
}

func (r *Registry) remove(agent, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if _, err := r.project(ctx, agent, id); err != nil {
		return fmt.Errorf("project %s/%s not found", agent, id)
	}
	if run := r.stopRunner(agent, id); run != nil {
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
		}
	}
	if led, err := r.readLedger(ctx, agent, id); err != nil {
		log.Printf("[projects] reading the ledger of %s/%s before deleting it: %v", agent, id, err)
	} else {
		for _, t := range led.Tasks {
			if t.Bound || !isTerminal(t.Status) {
				r.deleteBinding(ctx, t.SessionID)
			}
		}
	}
	r.deleteState(ctx, agent, id)
	if err := r.docs.Delete(ctx, store.Key(agent, id)); err != nil {
		return fmt.Errorf("remove project: %w", err)
	}
	r.touch(agent, id)
	log.Printf("[projects] deleted %s/%s", agent, id)
	return nil
}

func (r *Registry) deleteState(ctx context.Context, agent, id string) {
	for _, obj := range []string{ledgerKey(agent, id), memoryKey(agent, id)} {
		if err := r.b.Delete(ctx, obj, ""); err != nil {
			log.Printf("[projects] deleting %s: %v", obj, err)
		}
	}
}

// dispatchOut always carries token_configured, which the stored descriptor omits.
type dispatchOut struct {
	Dispatch
	TokenConfigured bool `json:"token_configured"`
}

type projectOut struct {
	Project
	Dispatch dispatchOut `json:"dispatch"`
}

func output(p *Project) *projectOut {
	return &projectOut{Project: *p, Dispatch: dispatchOut{Dispatch: p.Dispatch, TokenConfigured: routineToken(p.Dispatch.Token) != ""}}
}

func (r *Registry) list(agent string) []*projectOut {
	out := []*projectOut{}
	r.docs.Range(func(_ string, p *Project) {
		if agent == "" || p.Agent == agent {
			out = append(out, output(p.summary()))
		}
	})
	slices.SortFunc(out, func(a, b *projectOut) int {
		if c := strings.Compare(a.Agent, b.Agent); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

type projectView struct {
	projectOut
	Tasks      []Task      `json:"tasks"`
	Backlog    []Candidate `json:"backlog"`
	Memory     []Note      `json:"memory"`
	StateError string      `json:"state_error,omitempty"`
}

func (r *Registry) view(agent, id string) (*projectView, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), viewTimeout)
	defer cancel()
	p, err := r.project(ctx, agent, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[projects] reading %s/%s: %v", agent, id, err)
		}
		return nil, false
	}
	v := &projectView{projectOut: *output(p), Tasks: []Task{}, Backlog: []Candidate{}, Memory: []Note{}}
	st, err := r.state(ctx, agent, id)
	if err != nil {
		log.Printf("[projects] reading the state of %s/%s: %v", agent, id, err)
		v.StateError = "The tasks, backlog and memory could not be read; try again shortly."
		return v, true
	}
	v.Tasks = append(v.Tasks, st.ledger.Tasks...)
	v.Backlog = st.ledger.backlog(p.Signal.key(), p.Limits.effective(), time.Now().UTC(), backlogViewSize)
	v.Memory = append(v.Memory, st.memory.Notes...)
	return v, true
}

func (r *Registry) state(ctx context.Context, agent, id string) (*projectState, error) {
	r.mu.RLock()
	gen := r.gens[key(agent, id)]
	r.mu.RUnlock()
	return r.states.Get(ctx, stateKey{agent: agent, id: id, gen: gen})
}

// touch moves the project to a fresh state-cache generation, so this replica's next view reads its own writes.
func (r *Registry) touch(agent, id string) {
	r.mu.Lock()
	r.gens[key(agent, id)]++
	r.mu.Unlock()
}

func (r *Registry) fetchState(ctx context.Context, k stateKey) (*projectState, error) {
	led, err := r.readLedger(ctx, k.agent, k.id)
	if err != nil {
		return nil, err
	}
	mem, err := r.readMemory(ctx, k.agent, k.id)
	if err != nil {
		return nil, err
	}
	return &projectState{ledger: led, memory: mem}, nil
}

func (r *Registry) readLedger(ctx context.Context, agent, id string) (*Ledger, error) {
	led, _, err := store.GetJSON[Ledger](ctx, r.b, ledgerKey(agent, id))
	if errors.Is(err, store.ErrNotFound) {
		return &Ledger{Tasks: []Task{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return led, nil
}

func (r *Registry) readMemory(ctx context.Context, agent, id string) (*Memory, error) {
	mem, _, err := store.GetJSON[Memory](ctx, r.b, memoryKey(agent, id))
	if errors.Is(err, store.ErrNotFound) {
		return &Memory{Notes: []Note{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return mem, nil
}

// mergeLedger applies one transition with store.Merge; a dry run on cur skips the write when the transition has nothing to do.
func (r *Registry) mergeLedger(ctx context.Context, agent, id string, cur *Ledger, fn func(*Ledger) bool) (*Ledger, bool, error) {
	if cur != nil && !fn(cur.clone()) {
		return cur, false, nil
	}
	var changed bool
	doc, err := store.Merge(ctx, r.b, ledgerKey(agent, id), 0, func(l *Ledger) {
		changed = fn(l)
		if l.Tasks == nil {
			l.Tasks = []Task{}
		}
	})
	if err != nil {
		return cur, false, err
	}
	r.touch(agent, id)
	return doc, changed, nil
}

func (r *Registry) mergeMemory(ctx context.Context, agent, id string, fn func(*Memory) bool) (*Memory, bool, error) {
	var changed bool
	doc, err := store.Merge(ctx, r.b, memoryKey(agent, id), 0, func(m *Memory) {
		changed = fn(m)
		if m.Notes == nil {
			m.Notes = []Note{}
		}
	})
	if err != nil {
		return nil, false, err
	}
	r.touch(agent, id)
	return doc, changed, nil
}

func (r *Registry) addAdminNote(ctx context.Context, agent, id string, n Note) error {
	full := false
	_, _, err := r.mergeMemory(ctx, agent, id, func(m *Memory) bool {
		full = m.count(noteAdmin, "") >= maxNotes
		return !full && m.add(n)
	})
	if err != nil {
		return err
	}
	if full {
		return invalidf("project memory already holds %d admin notes; delete one first", maxNotes)
	}
	return nil
}

func (r *Registry) removeNote(ctx context.Context, agent, id, noteID string) (bool, error) {
	_, removed, err := r.mergeMemory(ctx, agent, id, func(m *Memory) bool { return m.remove(noteID) })
	return removed, err
}

func (r *Registry) writeBinding(ctx context.Context, sessionID string, b binding) error {
	sk := sessiontoken.NormalizeSessionID(sessionID)
	if sk == "" {
		return errors.New("the session id cannot be used as a binding key")
	}
	_, err := store.PutJSON(ctx, r.b, bindingKey(sk), b, store.Condition{IfNoneMatch: true})
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	cur, _, gerr := store.GetJSON[binding](ctx, r.b, bindingKey(sk))
	if gerr != nil {
		return gerr
	}
	if cur.Agent != b.Agent || cur.Project != b.Project || cur.Task != b.Task {
		return errors.New("the session is already bound to another task")
	}
	return nil
}

func (r *Registry) deleteBinding(ctx context.Context, sessionID string) bool {
	sk := sessiontoken.NormalizeSessionID(sessionID)
	if sk == "" {
		return true
	}
	if err := r.b.Delete(ctx, bindingKey(sk), ""); err != nil {
		log.Printf("[projects] deleting the binding of session %s: %v", sk, err)
		return false
	}
	return true
}

func (r *Registry) refreshStats(agent, id string, led *Ledger) {
	if led == nil {
		return
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	_, err := r.updateDescriptor(ctx, agent, id, func(d *Project) error {
		d.Stats = computeStats(led, d, now)
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[projects] refreshing the stats of %s/%s: %v", agent, id, err)
	}
}
