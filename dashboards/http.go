package dashboards

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/crud"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes wires the per-agent view and the dashboard CRUD API onto
// the given mux. Routing/validation/GitOps gating live in internal/crud;
// this function only adapts the dashboards-specific Get/List/Delete shape and
// the prompt-dashboard render verb.
//
//	GET    /<agent>/dashboard/<id>              → embedded HTML viewer
//	GET    /<agent>/dashboard/<id>/data.json    → latest stored JSON
//	GET    /api/dashboards                      → list (optional ?agent=)
//	DELETE /api/dashboards/<agent>/<id>         → delete
//	POST   /api/dashboards/<agent>/<id>/render  → render a prompt instance
//
// Access control is enforced upstream by the global IP gate in main.
func (r *Registry) RegisterRoutes(mux *http.ServeMux, apiMux *http.ServeMux, knownAgents map[string]bool) {
	crud.Mount(mux, apiMux, knownAgents, crud.Spec{
		Kind:       "dashboard",
		KindPlural: "dashboards",
		ViewHTML:   viewHTML,
		Get: func(agent, id string) (any, string, bool) {
			d, ok := r.Get(agent, id)
			if !ok {
				return nil, "", false
			}
			return d, d.Source, true
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

// handleCustom dispatches dashboard-specific verbs. crud.Mount has already
// validated agent/id and stripped the `/api/dashboards/<agent>/<id>` prefix
// into subpath.
func (r *Registry) handleCustom(w http.ResponseWriter, req *http.Request, agent, id string, subpath []string) bool {
	// /<agent>/<id>/render — render a prompt template into a per-input instance.
	if len(subpath) == 1 && subpath[0] == "render" {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		r.handleRender(w, req, agent, id)
		return true
	}
	return false
}

type renderRequest struct {
	Inputs   map[string]string `json:"inputs"`
	Interval string            `json:"interval"`
}

// handleRender renders a prompt template into an instance and returns 202 with
// the instance's view URL. The LLM render runs in the instance's runner
// goroutine (started by RenderInstance); the client polls the instance's
// data.json until Markdown appears — the same fire-and-forget model as the
// workflow "run now" button.
func (r *Registry) handleRender(w http.ResponseWriter, req *http.Request, agent, id string) {
	var body renderRequest
	if req.Body != nil {
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && err.Error() != "EOF" {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	inputs := make(map[string]string, len(body.Inputs))
	for k, v := range body.Inputs {
		inputs[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	inst, err := r.RenderInstance(ctx, agent, id, inputs, body.Interval)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted": true,
		"agent":    agent,
		"id":       inst.ID,
		"view_url": inst.ViewURL(),
		"message":  "render queued; poll the instance data.json until markdown appears",
	})
}
