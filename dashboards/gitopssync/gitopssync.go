// Package gitopssync wires the dashboards registry into the shared
// internal/gitopssync reconcile loop. The descriptor layout in the remote
// repo is `<basePath>/<agent>/<id>.json` with the same fields as a
// dashboard registry entry; runtime fields (Data, LastSync, LastError,
// Account) are ignored on read.
package gitopssync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/justmike1/arbetern/dashboards"
	"github.com/justmike1/arbetern/github"
	core "github.com/justmike1/arbetern/internal/gitopssync"
)

type (
	Config = core.Config
	Syncer = core.Syncer
	Status = core.Status
)

const SourceMarker = core.SourceMarker

// New constructs a Syncer wired against the dashboards registry.
func New(cfg Config, gh *github.Client, reg *dashboards.Registry) (*Syncer, error) {
	return core.New(cfg, gh, &backend{reg: reg})
}

type backend struct{ reg *dashboards.Registry }

func (b *backend) Kind() string            { return "dashboards" }
func (b *backend) DefaultBasePath() string { return "arbetern/dashboards" }

func (b *backend) ListManaged() []core.ManagedItem {
	managed := b.reg.ListBySource(SourceMarker)
	out := make([]core.ManagedItem, 0, len(managed))
	for _, d := range managed {
		out = append(out, core.ManagedItem{Agent: d.Agent, ID: d.ID, Name: d.Name})
	}
	return out
}

func (b *backend) Delete(agent, id string) error { return b.reg.Delete(agent, id) }

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

func (b *backend) Parse(ctx context.Context, dirAgent, repoPath string, body []byte, fetch core.FetchFunc) (string, string, core.UpsertFunc, error) {
	var spec fileSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return "", "", nil, fmt.Errorf("parse: %w", err)
	}
	spec.Agent = dirAgent
	if strings.TrimSpace(spec.ID) == "" {
		return "", "", nil, nil
	}
	upsert := func(ctx context.Context, sourceRef string) (bool, error) {
		_, changed, err := b.reg.UpsertFromSpec(ctx, dashboards.UpsertSpec{
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
			SourceRef:    sourceRef,
		})
		return changed, err
	}
	return spec.ID, spec.Name, upsert, nil
}
