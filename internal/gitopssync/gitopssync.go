// Package gitopssync is the shared core for the workflows and dashboards
// GitOps reconcilers. It walks `<basePath>/<agent>/<id>.json` in a remote
// GitHub repository on a fixed interval and delegates the kind-specific
// parsing + upsert to a Backend implementation.
//
// One Syncer reconciles one Backend. The two existing callers
// (workflows/gitopssync and dashboards/gitopssync) are thin adapters that
// build a Backend from their respective registries.
package gitopssync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/github"
)

// SourceMarker is the value written into the entity's Source field for
// every item upserted by a Syncer. Both workflows and dashboards use the
// same marker so prune logic and UI badges can identify them.
const SourceMarker = "gitops"

// Config drives one Syncer.
type Config struct {
	Owner    string        // GitHub repo owner; empty resolves to the bot's owner.
	Repo     string        // GitHub repo name. Required.
	Branch   string        // Empty == repo default branch.
	BasePath string        // Empty falls back to Backend.DefaultBasePath().
	Interval time.Duration // Defaults to 5m, minimum 30s.
	Prune    bool          // When true, locally-managed items missing from git are deleted.
}

// ManagedItem is the minimal view a Backend exposes for its currently
// gitops-managed entries. Used for prune diffs and log output.
type ManagedItem struct {
	Agent string
	ID    string
	Name  string
}

// FetchFunc fetches the contents of another file referenced from a
// descriptor during Parse. `ref` is either:
//
//   - a path relative to the directory of the descriptor being parsed
//     (e.g. "./prompt.md", "prompt.md", "../shared/prompt.md"), or
//   - a repo-absolute path starting with "/", or
//   - an absolute https://github.com/<owner>/<repo>/blob/<ref>/<path> URL
//     (cross-repo / cross-branch references).
//
// The fetcher is scoped to the reconcile that produced it (owner/repo/
// branch are captured) and is safe to call only from within Parse.
type FetchFunc func(ctx context.Context, ref string) (string, error)

// Backend is the kind-specific glue between the generic reconcile loop
// and a concrete registry. Implementations are expected to be small.
type Backend interface {
	// Kind is used as the log prefix, e.g. "workflows" -> "[gitopssync]",
	// "dashboards" -> "[dashboards/gitopssync]".
	Kind() string
	// DefaultBasePath is used when Config.BasePath is empty
	// (e.g. "arbetern/workflows", "arbetern/dashboards").
	DefaultBasePath() string
	// Parse decodes one JSON file body. dirAgent is the directory name and
	// is authoritative — the implementation should log + override any
	// mismatching `agent` field in the file. repoPath is the descriptor's
	// path inside the source repo (used to resolve relative references).
	// fetch may be used to load companion files (e.g. an external prompt
	// markdown) referenced from the descriptor.
	//
	// Returns the desired item's id and human-readable name, plus an upsert
	// closure that performs the registry write. The closure receives the
	// long-lived sync context and the source ref string
	// ("<owner>/<repo>@<branch>:<path>") at apply time.
	//
	// When the file is unusable (missing id, malformed JSON), Parse returns
	// a non-nil error and the file is skipped.
	Parse(ctx context.Context, dirAgent, repoPath string, body []byte, fetch FetchFunc) (id, name string, upsert UpsertFunc, err error)
	// ListManaged returns the current set of items managed by this Backend
	// (Source field == SourceMarker). Used for prune diffs.
	ListManaged() []ManagedItem
	// Delete removes a managed item from the registry. Called only when
	// Config.Prune is true.
	Delete(agent, id string) error
}

// UpsertFunc applies one parsed item to the registry. changed reports
// whether the on-disk descriptor actually differed from the existing copy
// (false = no-op, useful so the syncer can tally unchanged items).
type UpsertFunc func(ctx context.Context, sourceRef string) (changed bool, err error)

// Syncer is the long-lived reconcile loop.
type Syncer struct {
	cfg     Config
	gh      *github.Client
	backend Backend
	prefix  string

	// runMu serialises reconcile() so a manual SyncNow can never overlap
	// with the periodic tick.
	runMu sync.Mutex

	mu       sync.Mutex
	stop     chan struct{}
	stopped  chan struct{}
	lastErr  error
	lastSync time.Time
	managed  map[string]string // agent/id -> repo path from last successful sync
	owner    string            // resolved repo owner (cached for SyncNow)
	running  bool              // a reconcile is currently in progress
}

// Status is a UI-facing snapshot of the syncer's configuration and the
// most recent reconcile result.
type Status struct {
	Owner     string    `json:"owner"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	BasePath  string    `json:"base_path"`
	Interval  string    `json:"interval"`
	Prune     bool      `json:"prune"`
	LastSync  time.Time `json:"last_sync"`
	LastError string    `json:"last_error,omitempty"`
	Managed   int       `json:"managed"`
	Running   bool      `json:"running"`
}

// New validates cfg and returns a Syncer. Call Start to begin polling.
func New(cfg Config, gh *github.Client, backend Backend) (*Syncer, error) {
	if gh == nil {
		return nil, errors.New("github client is required")
	}
	if backend == nil {
		return nil, errors.New("backend is required")
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, errors.New("gitopssync: Repo is required")
	}
	if strings.TrimSpace(cfg.BasePath) == "" {
		cfg.BasePath = backend.DefaultBasePath()
	}
	cfg.BasePath = strings.Trim(cfg.BasePath, "/")
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Interval < 30*time.Second {
		cfg.Interval = 30 * time.Second
	}
	prefix := "[gitopssync]"
	if k := strings.TrimSpace(backend.Kind()); k != "" && k != "workflows" {
		prefix = "[" + k + "/gitopssync]"
	}
	return &Syncer{
		cfg:     cfg,
		gh:      gh,
		backend: backend,
		prefix:  prefix,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		managed: make(map[string]string),
	}, nil
}

// Start runs the first reconcile immediately, then polls on Interval until
// Stop is called or ctx is cancelled.
func (s *Syncer) Start(ctx context.Context) { go s.run(ctx) }

func (s *Syncer) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	select {
	case <-s.stopped:
	case <-time.After(5 * time.Second):
	}
}

// LastResult returns the timestamp and error of the most recent reconcile.
func (s *Syncer) LastResult() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync, s.lastErr
}

// Status returns a snapshot suitable for serving over HTTP.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Owner:    s.owner,
		Repo:     s.cfg.Repo,
		Branch:   s.cfg.Branch,
		BasePath: s.cfg.BasePath,
		Interval: s.cfg.Interval.String(),
		Prune:    s.cfg.Prune,
		LastSync: s.lastSync,
		Managed:  len(s.managed),
		Running:  s.running,
	}
	if s.cfg.Owner != "" {
		st.Owner = s.cfg.Owner
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	return st
}

// SyncNow triggers an out-of-band reconcile and blocks until it
// completes (or the context is cancelled). It is safe to call
// concurrently with the periodic loop — runMu serialises both paths.
func (s *Syncer) SyncNow(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.mu.Lock()
	owner := s.owner
	if owner == "" {
		owner = s.cfg.Owner
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	if owner == "" {
		o, err := s.gh.ResolveOwner(ctx)
		if err != nil {
			wrapped := fmt.Errorf("resolve owner: %w", err)
			s.recordResult(wrapped)
			return wrapped
		}
		owner = o
		s.mu.Lock()
		s.owner = o
		s.mu.Unlock()
	}
	err := s.reconcile(ctx, owner)
	s.recordResult(err)
	if err != nil {
		log.Printf("%s manual reconcile failed: %v", s.prefix, err)
	}
	return err
}

func (s *Syncer) run(ctx context.Context) {
	defer close(s.stopped)
	owner := s.cfg.Owner
	if owner == "" {
		// Best-effort resolve; tick() retries on next interval if it fails.
		o, err := s.gh.ResolveOwner(ctx)
		if err != nil {
			log.Printf("%s could not resolve default owner: %v (will retry)", s.prefix, err)
		} else {
			owner = o
		}
	}
	s.mu.Lock()
	s.owner = owner
	s.mu.Unlock()
	log.Printf("%s polling %s/%s@%s:%s every %s (prune=%t)",
		s.prefix, nonEmpty(owner, "<unresolved>"), s.cfg.Repo, nonEmpty(s.cfg.Branch, "<default>"),
		s.cfg.BasePath, s.cfg.Interval, s.cfg.Prune)

	tick := func() {
		s.runMu.Lock()
		defer s.runMu.Unlock()
		if owner == "" {
			if o, err := s.gh.ResolveOwner(ctx); err == nil {
				owner = o
				s.mu.Lock()
				s.owner = o
				s.mu.Unlock()
			} else {
				s.recordResult(fmt.Errorf("resolve owner: %w", err))
				return
			}
		}
		s.mu.Lock()
		s.running = true
		s.mu.Unlock()
		err := s.reconcile(ctx, owner)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		s.recordResult(err)
		if err != nil {
			log.Printf("%s reconcile failed: %v", s.prefix, err)
		}
	}
	tick()

	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			tick()
		}
	}
}

func (s *Syncer) recordResult(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSync = time.Now().UTC()
	s.lastErr = err
}

// desired keys items by agent/id and remembers the upsert closure plus the
// repo-relative path (used both for the source ref and for prune diffs).
type desired struct {
	upsert UpsertFunc
	path   string
}

func (s *Syncer) reconcile(ctx context.Context, owner string) error {
	branch := s.cfg.Branch
	if branch == "" {
		b, err := s.gh.GetDefaultBranch(ctx, owner, s.cfg.Repo)
		if err != nil {
			return fmt.Errorf("resolve default branch: %w", err)
		}
		branch = b
	}

	agentDirs, err := s.gh.GetDirectoryContents(ctx, owner, s.cfg.Repo, s.cfg.BasePath, branch)
	if err != nil {
		// Missing base path is not an error — treat as empty desired set.
		if strings.Contains(err.Error(), "404") {
			log.Printf("%s base path %q not found in %s/%s@%s — skipping",
				s.prefix, s.cfg.BasePath, owner, s.cfg.Repo, branch)
			return s.maybePruneAgainst(nil)
		}
		return fmt.Errorf("list base dir: %w", err)
	}

	want := make(map[string]desired, 16)

	for _, entry := range agentDirs {
		if !strings.HasSuffix(entry, "/") {
			continue
		}
		agent := path.Base(strings.TrimRight(entry, "/"))
		if agent == "" {
			continue
		}
		agentPath := path.Join(s.cfg.BasePath, agent)
		files, err := s.gh.GetDirectoryContents(ctx, owner, s.cfg.Repo, agentPath, branch)
		if err != nil {
			log.Printf("%s list %s: %v (skipping agent)", s.prefix, agentPath, err)
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f, "/") || !strings.HasSuffix(f, ".json") {
				continue
			}
			filePath := f
			content, _, err := s.gh.GetFileContent(ctx, owner, s.cfg.Repo, filePath, branch)
			if err != nil {
				log.Printf("%s read %s: %v (skipping file)", s.prefix, filePath, err)
				continue
			}
			fetch := s.makeFetcher(owner, branch, filePath)
			id, _, upsertFn, err := s.backend.Parse(ctx, agent, filePath, []byte(content), fetch)
			if err != nil {
				log.Printf("%s parse %s: %v (skipping file)", s.prefix, filePath, err)
				continue
			}
			if strings.TrimSpace(id) == "" {
				log.Printf("%s %s: missing id — skipping", s.prefix, filePath)
				continue
			}
			k := agent + "/" + id
			if existing, dup := want[k]; dup {
				log.Printf("%s duplicate id %s in %s and %s — keeping first",
					s.prefix, k, existing.path, filePath)
				continue
			}
			want[k] = desired{upsert: upsertFn, path: filePath}
		}
	}

	sourceRefBase := fmt.Sprintf("%s/%s@%s", owner, s.cfg.Repo, branch)
	created, updated, unchanged, failed := 0, 0, 0, 0
	for k, d := range want {
		changed, err := d.upsert(ctx, sourceRefBase+":"+d.path)
		if err != nil {
			log.Printf("%s upsert %s (%s): %v", s.prefix, k, d.path, err)
			failed++
			continue
		}
		if !changed {
			unchanged++
			continue
		}
		if _, alreadyManaged := s.managed[k]; alreadyManaged {
			updated++
		} else {
			created++
		}
	}

	newManaged := make(map[string]string, len(want))
	for k, d := range want {
		newManaged[k] = d.path
	}
	pruned := 0
	if s.cfg.Prune {
		pruned = s.prune(want)
	} else {
		s.warnMissing(want)
	}

	s.mu.Lock()
	s.managed = newManaged
	s.mu.Unlock()

	log.Printf("%s reconciled: %d created, %d updated, %d unchanged, %d failed, %d pruned (total managed=%d)",
		s.prefix, created, updated, unchanged, failed, pruned, len(newManaged))
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed to upsert", failed)
	}
	return nil
}

func (s *Syncer) maybePruneAgainst(want map[string]desired) error {
	if s.cfg.Prune {
		s.prune(want)
	} else {
		s.warnMissing(want)
	}
	return nil
}

func (s *Syncer) warnMissing(want map[string]desired) {
	managed := s.backend.ListManaged()
	if len(managed) == 0 {
		return
	}
	missing := make([]string, 0)
	for _, m := range managed {
		k := m.Agent + "/" + m.ID
		if _, ok := want[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return
	}
	log.Printf("%s %d managed item(s) no longer in git (prune disabled): %s",
		s.prefix, len(missing), strings.Join(missing, ", "))
}

func (s *Syncer) prune(want map[string]desired) int {
	managed := s.backend.ListManaged()
	if len(managed) == 0 {
		return 0
	}
	n := 0
	for _, m := range managed {
		k := m.Agent + "/" + m.ID
		if _, ok := want[k]; ok {
			continue
		}
		if err := s.backend.Delete(m.Agent, m.ID); err != nil {
			log.Printf("%s prune %s: %v", s.prefix, k, err)
			continue
		}
		log.Printf("%s pruned %s (%q) — removed from git", s.prefix, k, m.Name)
		n++
	}
	return n
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// makeFetcher returns a FetchFunc that resolves references encountered
// while parsing a descriptor at descriptorPath. Relative references are
// resolved against the descriptor's directory in the current
// owner/repo@branch; absolute https://github.com/.../blob/... URLs are
// honoured as-is and may target a different repo or branch.
func (s *Syncer) makeFetcher(owner, branch, descriptorPath string) FetchFunc {
	repo := s.cfg.Repo
	descDir := path.Dir(descriptorPath)
	return func(ctx context.Context, ref string) (string, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return "", fmt.Errorf("empty file reference")
		}
		if strings.HasPrefix(ref, "https://github.com/") || strings.HasPrefix(ref, "http://github.com/") {
			o, r, b, p, err := parseGitHubBlobURL(ref)
			if err != nil {
				return "", err
			}
			content, _, err := s.gh.GetFileContent(ctx, o, r, p, b)
			return content, err
		}
		var resolved string
		if strings.HasPrefix(ref, "/") {
			resolved = path.Clean(strings.TrimPrefix(ref, "/"))
		} else {
			resolved = path.Clean(path.Join(descDir, ref))
		}
		if strings.HasPrefix(resolved, "../") || resolved == ".." {
			return "", fmt.Errorf("reference %q escapes repo root", ref)
		}
		content, _, err := s.gh.GetFileContent(ctx, owner, repo, resolved, branch)
		return content, err
	}
}

// parseGitHubBlobURL parses a https://github.com/<owner>/<repo>/blob/<branch>/<path>
// URL. Branch names containing slashes are not supported.
func parseGitHubBlobURL(raw string) (owner, repo, branch, filePath string, err error) {
	trimmed := raw
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimPrefix(trimmed, prefix)
			break
		}
	}
	parts := strings.SplitN(trimmed, "/", 5)
	if len(parts) < 5 || parts[2] != "blob" {
		return "", "", "", "", fmt.Errorf("unsupported github URL %q (expected https://github.com/<owner>/<repo>/blob/<branch>/<path>)", raw)
	}
	owner = parts[0]
	repo = parts[1]
	branch = parts[3]
	filePath = parts[4]
	if owner == "" || repo == "" || branch == "" || filePath == "" {
		return "", "", "", "", fmt.Errorf("unsupported github URL %q", raw)
	}
	return owner, repo, branch, filePath, nil
}
