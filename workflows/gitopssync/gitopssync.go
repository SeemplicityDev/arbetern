// Package gitopssync wires the workflows registry into the shared
// internal/gitopssync reconcile loop. The descriptor layout in the remote
// repo is `<basePath>/<agent>/<id>.json` with the same fields as a
// workflow registry entry; runtime fields are ignored on read.
package gitopssync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	core "github.com/justmike1/arbetern/internal/gitopssync"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/workflows"
)

// Re-exports so existing call sites keep working.
type (
	Config = core.Config
	Syncer = core.Syncer
	Status = core.Status
)

const SourceMarker = core.SourceMarker

// New constructs a Syncer wired against the workflows registry.
func New(cfg Config, gh *github.Client, reg *workflows.Registry) (*Syncer, error) {
	return core.New(cfg, gh, &backend{reg: reg})
}

type backend struct{ reg *workflows.Registry }

func (b *backend) Kind() string            { return "workflows" }
func (b *backend) DefaultBasePath() string { return "arbetern/workflows" }

func (b *backend) ListManaged() []core.ManagedItem {
	managed := b.reg.ListBySource(SourceMarker)
	out := make([]core.ManagedItem, 0, len(managed))
	for _, w := range managed {
		out = append(out, core.ManagedItem{Agent: w.Agent, ID: w.ID, Name: w.Name})
	}
	return out
}

func (b *backend) Delete(agent, id string) error { return b.reg.Delete(agent, id) }

// fileSpec is the declarative on-disk shape. Runtime fields on
// workflows.Workflow are absent so contributors can paste registry
// exports unchanged.
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

func (b *backend) Parse(dirAgent string, body []byte) (string, string, core.UpsertFunc, error) {
	var spec fileSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return "", "", nil, fmt.Errorf("parse: %w", err)
	}
	if spec.Agent != "" && spec.Agent != dirAgent {
		// Directory name is authoritative.
		spec.Agent = dirAgent
	} else {
		spec.Agent = dirAgent
	}
	if strings.TrimSpace(spec.ID) == "" {
		return "", "", nil, nil
	}
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}
	upsert := func(ctx context.Context, sourceRef string) (bool, error) {
		_, changed, err := b.reg.Upsert(ctx, workflows.UpsertSpec{
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
			SourceRef:   sourceRef,
		})
		return changed, err
	}
	return spec.ID, spec.Name, upsert, nil
}
