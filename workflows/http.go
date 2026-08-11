package workflows

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/justmike1/arbetern/internal/crud"
	"github.com/justmike1/arbetern/internal/safego"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes wires the per-agent view and workflow CRUD API onto the
// given mux. The shared GET/DELETE/list/data.json plumbing lives in
// internal/crud; the workflow-specific verbs (POST /<id>/run, PATCH/PUT
// for partial updates) are handled by the Custom hook below.
//
//	GET    /<agent>/workflow/<id>            → embedded HTML viewer
//	GET    /<agent>/workflow/<id>/data.json  → latest stored JSON
//	GET    /api/workflows                    → list (optional ?agent=)
//	GET    /api/workflows/<agent>/<id>       → get
//	PATCH  /api/workflows/<agent>/<id>       → partial update
//	DELETE /api/workflows/<agent>/<id>       → delete
//	POST   /api/workflows/<agent>/<id>/run   → run now (returns 202)
//
// Access control is enforced upstream by the global IP gate in main.
func (r *Registry) RegisterRoutes(mux *http.ServeMux, apiMux *http.ServeMux, knownAgents map[string]bool) {
	crud.Mount(mux, apiMux, knownAgents, crud.Spec{
		Kind:       "workflow",
		KindPlural: "workflows",
		ViewHTML:   viewHTML,
		Get: func(agent, id string) (any, string, bool) {
			wf, ok := r.Get(agent, id)
			if !ok {
				return nil, "", false
			}
			return wf, wf.Source, true
		},
		List: func(agent string) any {
			return r.List(agent)
		},
		Delete: func(agent, id string) error {
			return r.Delete(agent, id)
		},
		Custom: r.handleCustom,
	})
}

// handleCustom dispatches workflow-specific verbs. crud.Mount has already
// validated agent/id and stripped the `/api/workflows/<agent>/<id>` prefix
// into subpath.
func (r *Registry) handleCustom(w http.ResponseWriter, req *http.Request, agent, id string, subpath []string) bool {
	// /<agent>/<id>/run — fire-and-forget manual trigger.
	if len(subpath) == 1 && subpath[0] == "run" {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		// Run in its own goroutine and return 202 immediately. Workflow
		// ticks routinely run for several minutes (LLM tool loops, GitHub
		// API, rate-limit back-offs); blocking the HTTP response that long
		// ties up the "Run now" UI button and is usually killed by
		// intermediate proxies before the tick completes. The caller polls
		// /data.json to see results in run history. The per-workflow busy
		// try-lock inside runOnce still prevents concurrent runs of the
		// same workflow.
		safego.Go("workflows: manual run "+agent+"/"+id, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			_, _ = r.RunOnce(ctx, agent, id, "manual:api")
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": true,
			"agent":    agent,
			"id":       id,
			"message":  "workflow run queued; poll /data.json to see the result appear in run history",
		})
		return true
	}

	// Bare /<agent>/<id> with PATCH/PUT — partial update.
	if len(subpath) == 0 && (req.Method == http.MethodPatch || req.Method == http.MethodPut) {
		r.handleAPIUpdate(agent, id, w, req)
		return true
	}
	return false
}

// handleAPIUpdate applies a partial edit to a workflow from the UI. Body is a
// JSON object; only the keys actually present in the payload are treated as
// intentional edits (mirrors update_workflow tool semantics so a missing
// field means "unchanged" rather than "clear").
func (r *Registry) handleAPIUpdate(agent, id string, w http.ResponseWriter, req *http.Request) {
	// Reject UI edits to gitops-managed workflows. Toggling enabled is the
	// only allowed mutation; everything else is reverted on the next reconcile.
	if existing, ok := r.Get(agent, id); ok && existing.Source == "gitops" {
		bodyBytes, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		var peek map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &peek); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		onlyEnabled := len(peek) > 0
		for k := range peek {
			if k != "enabled" {
				onlyEnabled = false
				break
			}
		}
		if !onlyEnabled {
			http.Error(w, "workflow is managed via GitOps and cannot be edited from the UI; edit the file in the source repo instead", http.StatusForbidden)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var opts UpdateOpts
	if v, ok := raw["name"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			http.Error(w, "'name' must be a string", http.StatusBadRequest)
			return
		}
		opts.Name = &s
	}
	if v, ok := raw["description"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			http.Error(w, "'description' must be a string", http.StatusBadRequest)
			return
		}
		opts.Description = &s
	}
	if v, ok := raw["cron"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			http.Error(w, "'cron' must be a string (5-field cron expression in UTC, or '@every 1h' / '@daily' descriptor)", http.StatusBadRequest)
			return
		}
		opts.Cron = &s
	}
	if v, ok := raw["prompt"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			http.Error(w, "'prompt' must be a string", http.StatusBadRequest)
			return
		}
		opts.Prompt = &s
	}
	if v, ok := raw["tasks"]; ok {
		var tasks []Task
		if err := json.Unmarshal(v, &tasks); err != nil {
			http.Error(w, "'tasks' must be an array of {name,prompt}", http.StatusBadRequest)
			return
		}
		opts.Tasks = &tasks
	}
	if v, ok := raw["trigger"]; ok {
		var trig Trigger
		if err := json.Unmarshal(v, &trig); err != nil {
			http.Error(w, "'trigger' must be an object", http.StatusBadRequest)
			return
		}
		opts.Trigger = &trig
	}
	if v, ok := raw["enabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			http.Error(w, "'enabled' must be a boolean", http.StatusBadRequest)
			return
		}
		opts.Enabled = &b
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	updated, err := r.Update(ctx, agent, id, opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}
