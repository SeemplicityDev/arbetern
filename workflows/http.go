package workflows

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes wires the per-agent view and API endpoints onto the given mux.
//
// Per-agent:
//
//	GET /<agent>/workflow/<id>           → embedded HTML viewer
//	GET /<agent>/workflow/<id>/data.json → latest stored JSON
//
// API:
//
//	GET    /api/workflows              → list all workflows (optional ?agent=)
//	DELETE /api/workflows/<agent>/<id> → delete a workflow
//
// Access control is handled upstream by the global IP gate in main; this
// package deliberately stays transport-agnostic.
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
		// Kick the workflow off in its own goroutine and return 202 Accepted
		// immediately. A workflow tick can easily take several minutes
		// (LLM tool loops, GitHub API, rate-limit back-offs); blocking the
		// HTTP response that long ties up the "Run now" UI button and the
		// request itself will usually be killed by intermediate proxies
		// before the tick completes. The caller polls /data.json to see
		// the result appear in the run-history list.
		//
		// The per-workflow `busy` try-lock inside runOnce still prevents
		// concurrent runs of the same workflow, so double-clicking the
		// button (or a schedule overlapping a manual trigger) is safe.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			_, _ = r.RunOnce(ctx, agent, id, "manual:api")
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": true,
			"agent":    agent,
			"id":       id,
			"message":  "workflow run queued; poll /data.json to see the result appear in run history",
		})
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
	case http.MethodPatch, http.MethodPut:
		r.handleAPIUpdate(agent, id, w, req)
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

// handleAPIUpdate applies a partial edit to a workflow from the UI. Body is a
// JSON object; only the keys actually present in the payload are treated as
// intentional edits (mirrors the update_workflow tool semantics so a missing
// field means "unchanged" rather than "clear").
func (r *Registry) handleAPIUpdate(agent, id string, w http.ResponseWriter, req *http.Request) {
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
