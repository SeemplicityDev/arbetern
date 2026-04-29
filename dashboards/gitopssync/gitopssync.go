// Package gitopssync reconciles dashboard JSON descriptors from a remote
// GitHub repo into the local dashboards registry on a fixed interval.
// Dashboards the syncer manages are tagged Source=="gitops". When Prune is
// true, managed dashboards missing from the remote are deleted locally.
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

	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/github"
)

// SourceMarker tags every dashboard this package upserts.
const SourceMarker = "gitops"

type Config struct {
	Owner    string        // GitHub repo owner; empty resolves to the bot's owner.
	Repo     string        // GitHub repo name. Required.
	Branch   string        // Empty == repo default branch.
	BasePath string        // Defaults to "arbetern/dashboards".
	Interval time.Duration // Defaults to 5m, minimum 30s.
	Prune    bool          // When true, locally-managed dashboards missing from git are deleted.
}

type Syncer struct {
	cfg      Config
	gh       *github.Client
	registry *dashboards.Registry

	mu       sync.Mutex
	stop     chan struct{}
	stopped  chan struct{}
	lastErr  error
	lastSync time.Time
	managed  map[string]string // agent/id -> repo path from last successful sync
}

func New(cfg Config, gh *github.Client, reg *dashboards.Registry) (*Syncer, error) {
	if gh == nil {
		return nil, errors.New("github client is required")
	}
	if reg == nil {
		return nil, errors.New("dashboard registry is required")
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, errors.New("gitopssync: Repo is required")
	}
	if strings.TrimSpace(cfg.BasePath) == "" {
		cfg.BasePath = "arbetern/dashboards"
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

func (s *Syncer) LastResult() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync, s.lastErr
}

func (s *Syncer) run(ctx context.Context) {
	defer close(s.stopped)
	owner := s.cfg.Owner
	if owner == "" {
		if o, err := s.gh.ResolveOwner(ctx); err != nil {
			log.Printf("[dashboards/gitopssync] could not resolve default owner: %v (will retry)", err)
		} else {
			owner = o
		}
	}
	log.Printf("[dashboards/gitopssync] polling %s/%s@%s:%s every %s (prune=%t)",
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
			log.Printf("[dashboards/gitopssync] reconcile failed: %v", err)
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
// dashboards.Dashboard (Data, LastSync, LastError, Account) are absent
// so contributors can paste registry exports unchanged.
type fileSpec struct {
	ID           string                  `json:"id"`
	Agent        string                  `json:"agent"`
	Name         string                  `json:"name"`
	ShortName    string                  `json:"short_name"`
	Description  string                  `json:"description"`
	Kind         string                  `json:"kind"`
	SyncInterval string                  `json:"sync_interval"`
	Sources      []dashboards.DataSource `json:"sources"`
	CreatedBy    string                  `json:"created_by"`
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
		if strings.Contains(err.Error(), "404") {
			log.Printf("[dashboards/gitopssync] base path %q not found in %s/%s@%s — skipping",
				s.cfg.BasePath, owner, s.cfg.Repo, branch)
			return s.maybePruneAgainst(nil)
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
			log.Printf("[dashboards/gitopssync] list %s: %v (skipping agent)", agentPath, err)
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f, "/") || !strings.HasSuffix(f, ".json") {
				continue
			}
			filePath := f
			content, _, err := s.gh.GetFileContent(ctx, owner, s.cfg.Repo, filePath, branch)
			if err != nil {
				log.Printf("[dashboards/gitopssync] read %s: %v (skipping file)", filePath, err)
				continue
			}
			var spec fileSpec
			if err := json.Unmarshal([]byte(content), &spec); err != nil {
				log.Printf("[dashboards/gitopssync] parse %s: %v (skipping file)", filePath, err)
				continue
			}
			if spec.Agent != "" && spec.Agent != agent {
				log.Printf("[dashboards/gitopssync] %s: agent in file (%q) != directory (%q); using directory",
					filePath, spec.Agent, agent)
			}
			spec.Agent = agent
			if strings.TrimSpace(spec.ID) == "" {
				log.Printf("[dashboards/gitopssync] %s: missing id — skipping", filePath)
				continue
			}
			k := spec.Agent + "/" + spec.ID
			if _, dup := desired[k]; dup {
				log.Printf("[dashboards/gitopssync] duplicate id %s in %s and %s — keeping first",
					k, pathFor[k], filePath)
				continue
			}
			desired[k] = spec
			pathFor[k] = filePath
		}
	}

	sourceRef := fmt.Sprintf("%s/%s@%s", owner, s.cfg.Repo, branch)
	created, updated, unchanged, failed := 0, 0, 0, 0
	for k, spec := range desired {
		_, changed, err := s.registry.UpsertFromSpec(ctx, dashboards.UpsertSpec{
			ID:           spec.ID,
			Agent:        spec.Agent,
			Name:         spec.Name,
			ShortName:    spec.ShortName,
			Description:  spec.Description,
			Kind:         spec.Kind,
			SyncInterval: spec.SyncInterval,
			Sources:      spec.Sources,
			CreatedBy:    spec.CreatedBy,
			Source:       SourceMarker,
			SourceRef:    sourceRef + ":" + pathFor[k],
		})
		if err != nil {
			log.Printf("[dashboards/gitopssync] upsert %s (%s): %v", k, pathFor[k], err)
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
		log.Printf("[dashboards/gitopssync] prune: %v", err)
	} else if s.cfg.Prune {
		pruned = len(s.managed) - len(newManaged) + created
		if pruned < 0 {
			pruned = 0
		}
	}

	s.mu.Lock()
	s.managed = newManaged
	s.mu.Unlock()

	log.Printf("[dashboards/gitopssync] reconciled: %d created, %d updated, %d unchanged, %d failed, %d pruned (total managed=%d)",
		created, updated, unchanged, failed, pruned, len(newManaged))
	if failed > 0 {
		return fmt.Errorf("%d dashboard(s) failed to upsert", failed)
	}
	return nil
}

func (s *Syncer) maybePruneAgainst(desired map[string]fileSpec) error {
	managed := s.registry.ListBySource(SourceMarker)
	if len(managed) == 0 {
		return nil
	}
	missing := make([]*dashboards.Dashboard, 0)
	for _, d := range managed {
		k := d.Agent + "/" + d.ID
		if _, ok := desired[k]; !ok {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !s.cfg.Prune {
		names := make([]string, 0, len(missing))
		for _, d := range missing {
			names = append(names, d.Agent+"/"+d.ID)
		}
		log.Printf("[dashboards/gitopssync] %d managed dashboard(s) no longer in git (prune disabled): %s",
			len(missing), strings.Join(names, ", "))
		return nil
	}
	for _, d := range missing {
		if err := s.registry.Delete(d.Agent, d.ID); err != nil {
			log.Printf("[dashboards/gitopssync] prune %s/%s: %v", d.Agent, d.ID, err)
			continue
		}
		log.Printf("[dashboards/gitopssync] pruned %s/%s (%q) — removed from git", d.Agent, d.ID, d.Name)
	}
	return nil
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
