package dashboards

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes wires the per-agent view and per-API endpoints onto the given mux.
//
// Per-agent (public-ish — behind the same gating the agent uses):
//
//	GET /<agent>/dashboard/<id>           → embedded HTML viewer
//	GET /<agent>/dashboard/<id>/data.json → latest stored JSON
//
// API (behind the UI IP whitelist, same as other /api/* routes):
//
//	GET    /api/dashboards               → list all dashboards (optional ?agent=)
//	DELETE /api/dashboards/<agent>/<id>  → delete a dashboard
//
// Agents are validated against the knownAgents set so arbitrary path segments
// can't be used to access files outside the dashboards tree.
func (r *Registry) RegisterRoutes(mux *http.ServeMux, apiMux *http.ServeMux, knownAgents map[string]bool) {
	// Per-agent routes: /<agent>/dashboard/...
	for agent := range knownAgents {
		a := agent
		mux.HandleFunc("/"+a+"/dashboard/", func(w http.ResponseWriter, req *http.Request) {
			r.handleAgentView(a, w, req)
		})
	}

	apiMux.HandleFunc("/api/dashboards", r.handleList)
	apiMux.HandleFunc("/api/dashboards/", r.handleAPIItem)
}

func (r *Registry) handleAgentView(agent string, w http.ResponseWriter, req *http.Request) {
	// Path is /<agent>/dashboard/<id>[/data.json]
	trimmed := strings.TrimPrefix(req.URL.Path, "/"+agent+"/dashboard/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, req)
		return
	}
	id := parts[0]
	if !idValidRe.MatchString(id) {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	d, ok := r.Get(agent, id)
	if !ok {
		http.NotFound(w, req)
		return
	}
	// /data.json — serve the latest stored JSON snapshot.
	if len(parts) == 2 && parts[1] == "data.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(d)
		return
	}
	// Any other sub-path → 404. Empty sub-path → serve view.
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
	// /api/dashboards/<agent>/<id>
	trimmed := strings.TrimPrefix(req.URL.Path, "/api/dashboards/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "usage: /api/dashboards/<agent>/<id>", http.StatusBadRequest)
		return
	}
	agent, id := parts[0], parts[1]
	if !agentValidRe.MatchString(agent) || !idValidRe.MatchString(id) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case http.MethodGet:
		d, ok := r.Get(agent, id)
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d)
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
