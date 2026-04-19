package workflows

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes wires the per-agent view and API endpoints onto the given mux.
//
// Per-agent (public-ish):
//
//	GET /<agent>/workflow/<id>           → embedded HTML viewer
//	GET /<agent>/workflow/<id>/data.json → latest stored JSON
//
// API (behind the UI IP whitelist):
//
//	GET    /api/workflows              → list all workflows (optional ?agent=)
//	DELETE /api/workflows/<agent>/<id> → delete a workflow
func (r *Registry) RegisterRoutes(mux *http.ServeMux, apiMux *http.ServeMux, knownAgents map[string]bool) {
	for agent := range knownAgents {
		a := agent
		mux.HandleFunc("/"+a+"/workflow/", func(w http.ResponseWriter, req *http.Request) {
			r.handleAgentView(a, w, req)
		})
	}
	apiMux.HandleFunc("/api/workflows", r.handleList)
	apiMux.HandleFunc("/api/workflows/", r.handleAPIItem)
}

func (r *Registry) handleAgentView(agent string, w http.ResponseWriter, req *http.Request) {
	trimmed := strings.TrimPrefix(req.URL.Path, "/"+agent+"/workflow/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, req)
		return
	}
	id := parts[0]
	if !idValidRe.MatchString(id) {
		http.Error(w, "invalid workflow id", http.StatusBadRequest)
		return
	}
	wf, ok := r.Get(agent, id)
	if !ok {
		http.NotFound(w, req)
		return
	}
	if len(parts) == 2 && parts[1] == "data.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(wf)
		return
	}
	if len(parts) == 2 && parts[1] != "" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(viewHTML))
}

func (r *Registry) handleList(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agent := req.URL.Query().Get("agent")
	list := r.List(agent)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (r *Registry) handleAPIItem(w http.ResponseWriter, req *http.Request) {
	trimmed := strings.TrimPrefix(req.URL.Path, "/api/workflows/")
	parts := strings.Split(trimmed, "/")
	// Accept /api/workflows/<agent>/<id> and /api/workflows/<agent>/<id>/run
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "usage: /api/workflows/<agent>/<id>[/run]", http.StatusBadRequest)
		return
	}
	agent, id := parts[0], parts[1]
	if !agentValidRe.MatchString(agent) || !idValidRe.MatchString(id) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if len(parts) == 3 && parts[2] == "run" {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out, err := r.RunOnce(req.Context(), agent, id, "manual:api")
		resp := map[string]any{"result": out}
		if err != nil {
			resp["error"] = err.Error()
			w.WriteHeader(http.StatusInternalServerError)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	if len(parts) != 2 {
		http.Error(w, "usage: /api/workflows/<agent>/<id>[/run]", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case http.MethodGet:
		wf, ok := r.Get(agent, id)
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wf)
	case http.MethodDelete:
		if err := r.Delete(agent, id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
