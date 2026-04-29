// Package gitopssync reconciles workflow JSON descriptors from a remote
// GitHub repo into the local workflows registry on a fixed interval.
// Workflows the syncer manages are tagged Source=="gitops". When Prune is
// true, managed workflows missing from the remote are deleted locally.
// See docs/WORKFLOWS_GITOPS.md for layout and configuration.
package gitopssync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/workflows"
)

// SourceMarker tags every workflow this package upserts.
const SourceMarker = "gitops"

type Config struct {
	Owner    string        // GitHub repo owner; empty resolves to the bot's owner.
	Repo     string        // GitHub repo name. Required.
	Branch   string        // Empty == repo default branch.
	BasePath string        // Defaults to "arbetern/workflows".
	Interval time.Duration // Defaults to 5m, minimum 30s.
	Prune    bool          // When true, locally-managed workflows missing from git are deleted.
}

type Syncer struct {
	cfg      Config
	gh       *github.Client
	registry *workflows.Registry

	mu       sync.Mutex
	stop     chan struct{}
	stopped  chan struct{}
	lastErr  error
	lastSync time.Time
	lastRev  string            // resolved branch, for logs
	managed  map[string]string // agent/id -> repo path from last successful sync
}

// New validates cfg. Call Start to begin polling.
func New(cfg Config, gh *github.Client, reg *workflows.Registry) (*Syncer, error) {
	if gh == nil {
		return nil, errors.New("github client is required")
	}
	if reg == nil {
		return nil, errors.New("workflow registry is required")
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, errors.New("gitopssync: Repo is required")
	}
	if strings.TrimSpace(cfg.BasePath) == "" {
		cfg.BasePath = "arbetern/workflows"
	}
	cfg.BasePath = strings.Trim(cfg.BasePath, "/")
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Interval < 30*time.Second {
		cfg.Interval = 30 * time.Second
	}
	return &Syncer{
		cfg:      cfg,
		gh:       gh,
		registry: reg,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
		managed:  make(map[string]string),
	}, nil
}

// Start runs the first reconcile immediately, then polls on Interval until
// Stop is called or ctx is cancelled.
func (s *Syncer) Start(ctx context.Context) {
	go s.run(ctx)
}

func (s *Syncer) Stop() {
	select {
	case <-s.stop:
		// already closed
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

func (s *Syncer) run(ctx context.Context) {
	defer close(s.stopped)
	owner := s.cfg.Owner
	if owner == "" {
		// Best-effort resolve; tick() retries on next interval if it fails.
		o, err := s.gh.ResolveOwner(ctx)
		if err != nil {
			log.Printf("[gitopssync] could not resolve default owner: %v (will retry)", err)
		} else {
			owner = o
		}
	}
	log.Printf("[gitopssync] polling %s/%s@%s:%s every %s (prune=%t)",
		nonEmpty(owner, "<unresolved>"), s.cfg.Repo, nonEmpty(s.cfg.Branch, "<default>"),
		s.cfg.BasePath, s.cfg.Interval, s.cfg.Prune)

	tick := func() {
		if owner == "" {
			if o, err := s.gh.ResolveOwner(ctx); err == nil {
				owner = o
			} else {
				s.recordResult(fmt.Errorf("resolve owner: %w", err))
				return
			}
		}
		err := s.reconcile(ctx, owner)
		s.recordResult(err)
		if err != nil {
			log.Printf("[gitopssync] reconcile failed: %v", err)
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

// fileSpec is the declarative on-disk shape. Runtime fields on
// workflows.Workflow are intentionally absent so contributors can paste
// registry exports unchanged.
type fileSpec struct {
	ID          string            `json:"id"`
	Agent       string            `json:"agent"`
	Name        string            `json:"name"`
	ShortName   string            `json:"short_name"`
	Description string            `json:"description"`
	Cron        string            `json:"cron"`
	Prompt      string            `json:"prompt"`
	Tasks       []workflows.Task  `json:"tasks"`
	Trigger     workflows.Trigger `json:"trigger"`
	CreatedBy   string            `json:"created_by"`
	Enabled     *bool             `json:"enabled"`
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
	s.mu.Lock()
	s.lastRev = branch
	s.mu.Unlock()

	agentDirs, err := s.gh.GetDirectoryContents(ctx, owner, s.cfg.Repo, s.cfg.BasePath, branch)
	if err != nil {
		// Missing base path is not an error — treat as empty desired set.
		if strings.Contains(err.Error(), "404") {
			log.Printf("[gitopssync] base path %q not found in %s/%s@%s — skipping",
				s.cfg.BasePath, owner, s.cfg.Repo, branch)
			return s.maybePrune(nil)
		}
		return fmt.Errorf("list base dir: %w", err)
	}

	desired := make(map[string]fileSpec, 16)
	pathFor := make(map[string]string, 16)

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
			log.Printf("[gitopssync] list %s: %v (skipping agent)", agentPath, err)
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f, "/") || !strings.HasSuffix(f, ".json") {
				continue
			}
			filePath := f
			content, _, err := s.gh.GetFileContent(ctx, owner, s.cfg.Repo, filePath, branch)
			if err != nil {
				log.Printf("[gitopssync] read %s: %v (skipping file)", filePath, err)
				continue
			}
			var spec fileSpec
			if err := json.Unmarshal([]byte(content), &spec); err != nil {
				log.Printf("[gitopssync] parse %s: %v (skipping file)", filePath, err)
				continue
			}
			// Directory name is authoritative over the JSON's agent field.
			if spec.Agent != "" && spec.Agent != agent {
				log.Printf("[gitopssync] %s: agent in file (%q) != directory (%q); using directory",
					filePath, spec.Agent, agent)
			}
			spec.Agent = agent
			if strings.TrimSpace(spec.ID) == "" {
				log.Printf("[gitopssync] %s: missing id — skipping", filePath)
				continue
			}
			k := spec.Agent + "/" + spec.ID
			if existing, dup := desired[k]; dup {
				log.Printf("[gitopssync] duplicate id %s in %s and %s — keeping first",
					k, pathFor[k], filePath)
				_ = existing
				continue
			}
			desired[k] = spec
			pathFor[k] = filePath
		}
	}

	sourceRef := fmt.Sprintf("%s/%s@%s", owner, s.cfg.Repo, branch)
	created, updated, unchanged, failed := 0, 0, 0, 0
	for k, spec := range desired {
		enabled := true
		if spec.Enabled != nil {
			enabled = *spec.Enabled
		}
		_, changed, err := s.registry.Upsert(ctx, workflows.UpsertSpec{
			ID:          spec.ID,
			Agent:       spec.Agent,
			Name:        spec.Name,
			ShortName:   spec.ShortName,
			Description: spec.Description,
			Cron:        spec.Cron,
			Prompt:      spec.Prompt,
			Tasks:       spec.Tasks,
			Trigger:     spec.Trigger,
			CreatedBy:   spec.CreatedBy,
			Enabled:     enabled,
			Source:      SourceMarker,
			SourceRef:   sourceRef + ":" + pathFor[k],
		})
		if err != nil {
			log.Printf("[gitopssync] upsert %s (%s): %v", k, pathFor[k], err)
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

	newManaged := make(map[string]string, len(desired))
	for k, p := range pathFor {
		newManaged[k] = p
	}
	pruned := 0
	if err := s.maybePruneAgainst(desired); err != nil {
		log.Printf("[gitopssync] prune: %v", err)
	} else if s.cfg.Prune {
		pruned = len(s.managed) - len(newManaged) + created
		if pruned < 0 {
			pruned = 0
		}
	}

	s.mu.Lock()
	s.managed = newManaged
	s.mu.Unlock()

	log.Printf("[gitopssync] reconciled: %d created, %d updated, %d unchanged, %d failed, %d pruned (total managed=%d)",
		created, updated, unchanged, failed, pruned, len(newManaged))
	if failed > 0 {
		return fmt.Errorf("%d workflow(s) failed to upsert", failed)
	}
	return nil
}

// maybePrune is invoked when the base path is missing; with desired==nil
// Prune (if enabled) removes every locally-managed workflow.
func (s *Syncer) maybePrune(desired map[string]fileSpec) error {
	return s.maybePruneAgainst(desired)
}

func (s *Syncer) maybePruneAgainst(desired map[string]fileSpec) error {
	managed := s.registry.ListBySource(SourceMarker)
	if len(managed) == 0 {
		return nil
	}
	missing := make([]*workflows.Workflow, 0)
	for _, w := range managed {
		k := w.Agent + "/" + w.ID
		if _, ok := desired[k]; !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !s.cfg.Prune {
		names := make([]string, 0, len(missing))
		for _, w := range missing {
			names = append(names, w.Agent+"/"+w.ID)
		}
		log.Printf("[gitopssync] %d managed workflow(s) no longer in git (prune disabled): %s",
			len(missing), strings.Join(names, ", "))
		return nil
	}
	for _, w := range missing {
		if err := s.registry.Delete(w.Agent, w.ID); err != nil {
			log.Printf("[gitopssync] prune %s/%s: %v", w.Agent, w.ID, err)
			continue
		}
		log.Printf("[gitopssync] pruned %s/%s (%q) — removed from git", w.Agent, w.ID, w.Name)
	}
	return nil
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
