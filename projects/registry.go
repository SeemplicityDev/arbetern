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

	"github.com/justmike1/arbetern/billing"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/queue"
	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/ttlcache"
	"github.com/justmike1/arbetern/llm"
	"github.com/justmike1/arbetern/mcp"
)

var (
	errNotFound = errors.New("project not found")
	errBusy     = errors.New("a run of this project is in progress")
)

// ToolProvider runs arbetern's integration tools for sessions.
type ToolProvider interface {
	Tools() []llm.Tool
	Call(ctx context.Context, caller, name, args string) string
}

// Deps are the services a Registry works with.
type Deps struct {
	Backend    *store.Backend
	GitHub     *github.Client       // nil → create/PR features answer a clear error
	Datadog    *datadog.MultiClient // nil → no signal can be scanned
	Skills     func(ids []string) []string
	SkillKnown func(id string) bool // nil → skill ids are not checked against the console
	Slack      Poster               // nil → no notifications
	Usage      func(billing.Event)  // records the model usage of sessions
	Tools      ToolProvider         // nil → sessions get only the work-order tools
	Connectors ConnectorTools       // nil → projects cannot name MCP connectors
	Tickets    TicketFiler          // nil → projects whose conventions file tickets cannot open pull requests
	Cluster    ClusterConfig
}

// ConnectorTools serves the MCP connectors a project names to its sessions, with arbetern's stored credentials.
type ConnectorTools interface {
	Get(id string) (*mcp.Connector, bool)
	ProjectTools(ids []string) []mcp.AgentTool
	Call(ctx context.Context, connectorID, tool string, args json.RawMessage) (string, error)
}

type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type stateKey struct {
	id  string
	gen uint64
}

type projectState struct {
	ledger *Ledger
	memory *Memory
}

// Registry caches the stored projects and runs their ticks.
type Registry struct {
	deps    Deps
	b       *store.Backend
	gh      *github.Client
	cluster *cluster
	usage   *usageMeter
	docs    *store.Documents[Project]

	mu      sync.RWMutex
	runners map[string]*runner
	busy    map[string]bool
	gens    map[string]uint64
	baseCtx context.Context
	active  bool
	tasks   *queue.Queue

	states *ttlcache.Keyed[stateKey, *projectState]
	login  *ttlcache.Cache[string]
	orgs   *ttlcache.Keyed[string, []github.OrgRepository]
	diffs  *ttlcache.Keyed[diffKey, string]
}

type tickTask struct {
	ID      string `json:"id"`
	Trigger string `json:"trigger"`
}

// New creates a Registry; call Load before serving.
func New(deps Deps) *Registry {
	r := &Registry{
		deps:    deps,
		b:       deps.Backend,
		gh:      deps.GitHub,
		cluster: newCluster(deps.Cluster),
		usage:   newUsageMeter(deps.Usage),
		runners: map[string]*runner{},
		busy:    map[string]bool{},
		gens:    map[string]uint64{},
	}
	r.docs = store.NewDocuments(deps.Backend, Prefix, store.FlatKeyRe, func(p *Project) error {
		if !store.IDRe.MatchString(p.ID) {
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
	r.diffs = ttlcache.NewKeyed(diffsTTL, 512, func(ctx context.Context, k diffKey) (string, error) {
		if r.gh == nil {
			return "", errors.New("GitHub is not configured")
		}
		return r.gh.ChangeFingerprint(ctx, k.owner, k.repo, k.base, k.sha)
	})
	r.orgs = ttlcache.NewKeyed(orgReposTTL, 32, func(ctx context.Context, owner string) ([]github.OrgRepository, error) {
		if r.gh == nil {
			return nil, errors.New("GitHub is not configured")
		}
		// Every waiter shares this listing, so one caller's cancellation must not fail it for the others.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		return r.gh.ListOrgRepositories(ctx, owner)
	})
	return r
}

func (r *Registry) scopeRepos(ctx context.Context, repo Repo) ([]github.OrgRepository, error) {
	all, err := r.orgs.Get(ctx, strings.ToLower(repo.Owner))
	if err != nil {
		return nil, fmt.Errorf("listing the repositories of %s: %w", repo.Owner, err)
	}
	var out []github.OrgRepository
	for _, o := range all {
		if repo.includes(o.Name) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b github.OrgRepository) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (r *Registry) scopeRepo(ctx context.Context, repo Repo, name string) (github.OrgRepository, bool, error) {
	list, err := r.scopeRepos(ctx, repo)
	if err != nil {
		return github.OrgRepository{}, false, err
	}
	for _, o := range list {
		if strings.EqualFold(o.Name, name) {
			return o, true, nil
		}
	}
	return github.OrgRepository{}, false, nil
}

func docKey(id string) string { return id + ".json" }

// Load reads every stored project into the cache without starting runners.
func (r *Registry) Load(ctx context.Context) error {
	if err := r.docs.Load(ctx); err != nil {
		return fmt.Errorf("load projects: %w", err)
	}
	r.docs.Range(func(_ string, p *Project) {
		log.Printf("[projects] loaded %s (%q, repo=%s, enabled=%t)", p.ID, p.Name, p.Repo, p.Enabled)
	})
	return nil
}

// Count is the number of stored projects.
func (r *Registry) Count() int { return r.docs.Len() }

// RunUsageMeter records sessions' model usage every few minutes until ctx ends, then once more.
func (r *Registry) RunUsageMeter(ctx context.Context) { r.usage.run(ctx) }

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
	if !store.IDRe.MatchString(t.ID) {
		return fmt.Errorf("invalid project reference %q", t.ID)
	}
	r.tick(ctx, t.ID, t.Trigger)
	return nil
}

func (r *Registry) trigger(ctx context.Context, id string) error {
	r.mu.RLock()
	q := r.tasks
	r.mu.RUnlock()
	if q != nil {
		err := q.Enqueue(ctx, QueueTopic, tickTask{ID: id, Trigger: "manual"})
		if err == nil {
			return nil
		}
		log.Printf("[projects] queueing a tick of %s failed, running locally: %v", id, err)
	}
	safego.Go("projects: tick "+id, func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueTimeout)
		defer cancel()
		r.tick(tctx, id, "manual")
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
				r.stopRunner(c.Old.ID)
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
			r.stopRunner(updated.ID)
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
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	if old, ok := r.runners[p.ID]; ok {
		old.cancel()
	}
	parent := r.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	run := &runner{cancel: cancel, done: make(chan struct{})}
	r.runners[p.ID] = run
	r.mu.Unlock()

	id, every := p.ID, p.interval()
	go func() {
		defer close(run.done)
		defer safego.Recover("projects: runner " + id)
		wait := rand.N(maxStagger)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			safego.Run("projects: tick "+id, func() { r.tick(ctx, id, "schedule") })
			wait = every
		}
	}()
}

func (r *Registry) stopRunner(id string) *runner {
	r.mu.Lock()
	run, ok := r.runners[id]
	delete(r.runners, id)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	run.cancel()
	return run
}

func (r *Registry) tryBusy(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[id] {
		return false
	}
	r.busy[id] = true
	return true
}

func (r *Registry) releaseBusy(id string) {
	r.mu.Lock()
	delete(r.busy, id)
	r.mu.Unlock()
}

func (r *Registry) datadogFor(site string) *datadog.Client {
	return r.deps.Datadog.ForSite(site)
}

func (r *Registry) checkTickets(c Conventions) error {
	if c.Ticket != nil && r.deps.Tickets == nil {
		return invalidf("conventions.ticket needs the deployment's Jira connection, which is not configured")
	}
	return nil
}

func (r *Registry) checkConnectors(ids []string) error {
	if len(ids) > 0 && r.deps.Connectors == nil {
		return invalidf("connectors: this deployment has no MCP connectors")
	}
	for _, id := range ids {
		c, ok := r.deps.Connectors.Get(id)
		switch {
		case !ok:
			return invalidf("connectors: %q is not an MCP connector", id)
		case len(c.Agents) > 0:
			return invalidf("connectors: %s is limited to some agents; a project may name only a connector open to every agent", c.Name)
		}
	}
	return nil
}

func (r *Registry) checkSkills(ids []string) error {
	if r.deps.SkillKnown == nil {
		return nil
	}
	for _, id := range ids {
		if !r.deps.SkillKnown(id) {
			return invalidf("skills: %q is not a console skill", id)
		}
	}
	return nil
}

func (r *Registry) checkSites(s Signal) error {
	for _, site := range s.sites() {
		if r.datadogFor(site) == nil {
			return invalidf("this deployment has no Datadog credentials for site %s", site)
		}
	}
	return nil
}

func (r *Registry) skillsFor(ids []string) []string {
	if r.deps.Skills == nil {
		return nil
	}
	return r.deps.Skills(ids)
}

func (r *Registry) resolveRepo(ctx context.Context, repo *Repo) error {
	if !repo.org() {
		if repo.BaseBranch != "" {
			return nil
		}
		base, err := r.defaultBranch(ctx, *repo)
		repo.BaseBranch = base
		return err
	}
	if r.gh == nil {
		return invalidf("GitHub is not configured in this deployment, so repo.include cannot be resolved")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	list, err := r.scopeRepos(ctx, *repo)
	if err != nil {
		return invalidf("%v", err)
	}
	if len(list) == 0 {
		return invalidf("repo.include matches none of the repositories of %s that arbetern can see", repo.Owner)
	}
	return nil
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
	if err := draft.normalize(); err != nil {
		return nil, err
	}
	if err := r.checkSites(draft.Signal); err != nil {
		return nil, err
	}
	if err := r.checkSkills(draft.Skills); err != nil {
		return nil, err
	}
	if err := r.checkConnectors(draft.Connectors); err != nil {
		return nil, err
	}
	if err := r.checkTickets(draft.Conventions); err != nil {
		return nil, err
	}
	if r.gh == nil {
		return nil, invalidf("GitHub is not configured in this deployment; projects need it to open pull requests")
	}
	if err := r.resolveRepo(ctx, &draft.Repo); err != nil {
		return nil, err
	}
	id, err := store.NewID()
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID:           id,
		Name:         draft.Name,
		Description:  draft.Description,
		Goal:         draft.Goal,
		Instructions: draft.Instructions,
		Skills:       draft.Skills,
		Connectors:   draft.Connectors,
		Conventions:  draft.Conventions,
		Repo:         draft.Repo,
		Signal:       draft.Signal,
		Session:      draft.Session,
		Review:       draft.Review,
		Notify:       draft.Notify,
		Limits:       draft.Limits,
		Interval:     draft.Interval,
		Enabled:      enabled,
		CreatedBy:    createdBy,
		CreatedAt:    stamp(time.Now()),
	}
	if err := r.docs.Create(ctx, docKey(p.ID), p); err != nil {
		return nil, err
	}
	if p.Enabled {
		r.startRunner(p)
	}
	log.Printf("[projects] created %s (%q, repo=%s)", p.ID, p.Name, p.Repo)
	cp := *p
	return &cp, nil
}

type projectPatch struct {
	Name, Description, Goal, Instructions, Interval *string
	Skills                                          *[]string
	Connectors                                      *[]string
	Repo                                            *Repo
	Signal                                          *Signal
	Session                                         *Session
	Review                                          *Review
	Notify                                          *Notify
	Conventions                                     *Conventions
	Limits                                          *Limits
	Enabled                                         *bool

	limitsJSON json.RawMessage
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
	if pt.Skills != nil {
		dst.Skills = *pt.Skills
	}
	if pt.Connectors != nil {
		dst.Connectors = *pt.Connectors
	}
	if pt.Repo != nil {
		dst.Repo = *pt.Repo
	}
	if pt.Signal != nil {
		dst.Signal = *pt.Signal
	}
	if pt.Session != nil {
		dst.Session = *pt.Session
	}
	if pt.Review != nil {
		dst.Review = *pt.Review
	}
	if pt.Notify != nil {
		dst.Notify = *pt.Notify
	}
	if pt.Conventions != nil {
		dst.Conventions = *pt.Conventions
	}
	switch {
	case pt.limitsJSON != nil:
		// Limits left out keep their value, so a console tab that predates a limit cannot reset it.
		_ = json.Unmarshal(pt.limitsJSON, &dst.Limits)
	case pt.Limits != nil:
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
	if pt.Skills != nil {
		out.Skills = &draft.Skills
	}
	if pt.Connectors != nil {
		out.Connectors = &draft.Connectors
	}
	if pt.Repo != nil {
		out.Repo = &draft.Repo
	}
	if pt.Signal != nil {
		out.Signal = &draft.Signal
	}
	if pt.Session != nil {
		out.Session = &draft.Session
	}
	if pt.Review != nil {
		out.Review = &draft.Review
	}
	if pt.Notify != nil {
		out.Notify = &draft.Notify
	}
	if pt.Conventions != nil {
		out.Conventions = &draft.Conventions
	}
	if pt.Limits != nil {
		out.Limits = &draft.Limits
	}
	return out
}

func (r *Registry) update(ctx context.Context, id string, pt projectPatch) (*Project, error) {
	cur, err := r.project(ctx, id)
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
	if pt.Skills != nil {
		if err := r.checkSkills(draft.Skills); err != nil {
			return nil, err
		}
	}
	if pt.Connectors != nil {
		if err := r.checkConnectors(draft.Connectors); err != nil {
			return nil, err
		}
	}
	if pt.Conventions != nil {
		if err := r.checkTickets(draft.Conventions); err != nil {
			return nil, err
		}
	}
	if pt.Signal != nil {
		if err := r.checkSites(draft.Signal); err != nil {
			return nil, err
		}
	}
	if pt.Repo != nil {
		if err := r.resolveRepo(ctx, &draft.Repo); err != nil {
			return nil, err
		}
		// Unfinished tasks of the old repository keep it, so their rounds and pull requests do not follow the new one.
		if old := cur.Repo; !old.org() && old.String() != draft.Repo.String() {
			if _, _, err := r.mergeLedger(ctx, id, nil, func(l *Ledger) bool { return l.pinRepo(old) }); err != nil {
				return nil, fmt.Errorf("recording the old repository on open tasks: %w", err)
			}
		}
	}
	norm := pt.normalized(&draft)
	now := time.Now()
	updated, err := r.updateDescriptor(ctx, id, func(p *Project) error {
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
	r.touch(id)
	log.Printf("[projects] updated %s (%q, enabled=%t)", id, updated.Name, updated.Enabled)
	return updated, nil
}

// updateDescriptor reconciles the runner with what was stored, which may include another replica's edit that Update folded in.
func (r *Registry) updateDescriptor(ctx context.Context, id string, fn func(*Project) error) (*Project, error) {
	before, cached := r.docs.Get(docKey(id))
	updated, err := r.docs.Update(ctx, docKey(id), fn)
	if errors.Is(err, store.ErrNotFound) {
		r.stopRunner(id)
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
func (r *Registry) project(ctx context.Context, id string) (*Project, error) {
	if p, ok := r.docs.Get(docKey(id)); ok {
		return p, nil
	}
	return r.fetchProject(ctx, id)
}

func (r *Registry) fetchProject(ctx context.Context, id string) (*Project, error) {
	p, _, err := store.GetJSON[Project](ctx, r.b, Prefix+docKey(id))
	if err != nil {
		return nil, err
	}
	if p.ID != id {
		return nil, errors.New("invalid project descriptor")
	}
	return p, nil
}

func (r *Registry) remove(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if _, err := r.project(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound
		}
		return fmt.Errorf("reading project %s: %w", id, err)
	}
	// A tick of the project, on this replica or behind the lease on another, must not start a Job for a project that is going away.
	if !r.tryBusy(id) {
		return errBusy
	}
	defer r.releaseBusy(id)
	lease := store.NewLease(r.b, "locks/projects/"+id, store.InstanceID(), leaseTTL)
	switch _, release, acquired, err := lease.Acquire(ctx); {
	case err != nil:
		log.Printf("[projects] %s delete: lease unavailable, continuing without it: %v", id, err)
	case !acquired:
		return errBusy
	default:
		defer release()
	}
	if run := r.stopRunner(id); run != nil {
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
		}
	}
	led, lerr := r.readLedger(ctx, id)
	// The descriptor goes first: a project whose delete fails keeps its record of open pull requests.
	if err := r.docs.Delete(ctx, docKey(id)); err != nil {
		return fmt.Errorf("remove project: %w", err)
	}
	r.touch(id)
	if lerr != nil {
		log.Printf("[projects] reading the ledger of %s before deleting its sessions: %v", id, lerr)
	} else {
		for _, t := range led.Tasks {
			if t.Bound || !isTerminal(t.Status) {
				r.deleteBinding(ctx, t.SessionID)
			}
			if t.Review == nil {
				continue
			}
			for _, rd := range t.Review.Rounds {
				if rd.Bound || isActiveRound(rd.Status) {
					r.deleteBinding(ctx, rd.SessionID)
				}
			}
		}
	}
	r.cluster.stopProject(ctx, id)
	r.deleteState(ctx, id)
	r.deleteTranscripts(ctx, id)
	log.Printf("[projects] deleted %s", id)
	return nil
}

func (r *Registry) deleteState(ctx context.Context, id string) {
	for _, obj := range []string{ledgerKey(id), memoryKey(id)} {
		if err := r.b.Delete(ctx, obj, ""); err != nil {
			log.Printf("[projects] deleting %s: %v", obj, err)
		}
	}
}

func (r *Registry) list() []*Project {
	out := []*Project{}
	r.docs.Range(func(_ string, p *Project) {
		out = append(out, p.summary())
	})
	slices.SortFunc(out, func(a, b *Project) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

type projectView struct {
	Project
	Tasks      []Task      `json:"tasks"`
	Backlog    []Candidate `json:"backlog"`
	Memory     []Note      `json:"memory"`
	StateError string      `json:"state_error,omitempty"`
}

func (r *Registry) view(id string) (*projectView, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), viewTimeout)
	defer cancel()
	p, err := r.project(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("[projects] reading %s: %v", id, err)
		}
		return nil, false
	}
	v := &projectView{Project: *p, Tasks: []Task{}, Backlog: []Candidate{}, Memory: []Note{}}
	st, err := r.state(ctx, id)
	if err != nil {
		log.Printf("[projects] reading the state of %s: %v", id, err)
		v.StateError = "The tasks, backlog and memory could not be read; try again shortly."
		return v, true
	}
	v.Tasks = append(v.Tasks, st.ledger.Tasks...)
	v.Backlog = st.ledger.backlog(p.Signal.key(), p.Limits.effective(), time.Now().UTC(), backlogViewSize)
	v.Memory = append(v.Memory, st.memory.Notes...)
	return v, true
}

func (r *Registry) state(ctx context.Context, id string) (*projectState, error) {
	r.mu.RLock()
	gen := r.gens[id]
	r.mu.RUnlock()
	return r.states.Get(ctx, stateKey{id: id, gen: gen})
}

// touch moves the project to a fresh state-cache generation, so this replica's next view reads its own writes.
func (r *Registry) touch(id string) {
	r.mu.Lock()
	r.gens[id]++
	r.mu.Unlock()
}

func (r *Registry) fetchState(ctx context.Context, k stateKey) (*projectState, error) {
	led, err := r.readLedger(ctx, k.id)
	if err != nil {
		return nil, err
	}
	mem, err := r.readMemory(ctx, k.id)
	if err != nil {
		return nil, err
	}
	return &projectState{ledger: led, memory: mem}, nil
}

func (r *Registry) readLedger(ctx context.Context, id string) (*Ledger, error) {
	led, _, err := store.GetJSON[Ledger](ctx, r.b, ledgerKey(id))
	if errors.Is(err, store.ErrNotFound) {
		return &Ledger{Tasks: []Task{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return led, nil
}

func (r *Registry) readMemory(ctx context.Context, id string) (*Memory, error) {
	mem, _, err := store.GetJSON[Memory](ctx, r.b, memoryKey(id))
	if errors.Is(err, store.ErrNotFound) {
		return &Memory{Notes: []Note{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return mem, nil
}

// mergeLedger applies one transition with store.Merge; a dry run on cur skips the write when the transition has nothing to do.
func (r *Registry) mergeLedger(ctx context.Context, id string, cur *Ledger, fn func(*Ledger) bool) (*Ledger, bool, error) {
	if cur != nil && !fn(cur.clone()) {
		return cur, false, nil
	}
	var changed bool
	doc, err := store.Merge(ctx, r.b, ledgerKey(id), 0, func(l *Ledger) {
		changed = fn(l)
		if l.Tasks == nil {
			l.Tasks = []Task{}
		}
	})
	if err != nil {
		return cur, false, err
	}
	r.touch(id)
	return doc, changed, nil
}

func (r *Registry) mergeMemory(ctx context.Context, id string, fn func(*Memory) bool) (*Memory, bool, error) {
	var changed bool
	doc, err := store.Merge(ctx, r.b, memoryKey(id), 0, func(m *Memory) {
		changed = fn(m)
		if m.Notes == nil {
			m.Notes = []Note{}
		}
	})
	if err != nil {
		return nil, false, err
	}
	r.touch(id)
	return doc, changed, nil
}

func (r *Registry) addAdminNote(ctx context.Context, id string, n Note) error {
	full := false
	_, _, err := r.mergeMemory(ctx, id, func(m *Memory) bool {
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

func (r *Registry) removeNote(ctx context.Context, id, noteID string) (bool, error) {
	_, removed, err := r.mergeMemory(ctx, id, func(m *Memory) bool { return m.remove(noteID) })
	return removed, err
}

func (r *Registry) writeBinding(ctx context.Context, sessionID string, b binding) error {
	if !sessionKeyRe.MatchString(sessionID) {
		return errors.New("the session id cannot be used as a binding key")
	}
	_, err := store.PutJSON(ctx, r.b, bindingKey(sessionID), b, store.Condition{IfNoneMatch: true})
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	cur, _, gerr := store.GetJSON[binding](ctx, r.b, bindingKey(sessionID))
	if gerr != nil {
		return gerr
	}
	if cur.Project != b.Project || cur.Task != b.Task || cur.Round != b.Round {
		return errors.New("the session is already bound to another task")
	}
	return nil
}

func (r *Registry) deleteBinding(ctx context.Context, sessionID string) bool {
	if !sessionKeyRe.MatchString(sessionID) {
		return true
	}
	if err := r.b.Delete(ctx, bindingKey(sessionID), ""); err != nil {
		log.Printf("[projects] deleting the binding of session %s: %v", sessionID, err)
		return false
	}
	return true
}

func (r *Registry) refreshStats(id string, led *Ledger) {
	if led == nil {
		return
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	_, err := r.updateDescriptor(ctx, id, func(d *Project) error {
		d.Stats = computeStats(led, d, now)
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[projects] refreshing the stats of %s: %v", id, err)
	}
}
