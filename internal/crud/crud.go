// Package crud provides the shared HTTP handlers for the workflows and
// dashboards CRUD APIs. Both registries expose a nearly identical surface
// (per-agent viewer + data.json + REST list/get/delete with a GitOps 403
// gate), so centralising the routing eliminates ~150 LOC of duplication
// and keeps response shapes in lockstep.
//
// Per-package extras (POST /<id>/run, PATCH/PUT for workflows) are wired
// in via Spec.Custom — the built-ins handle the bits that are truly the
// same across both packages.
package crud

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/justmike1/arbetern/internal/store"
)

// Spec parameterises Mount with entity-specific behaviour. All callbacks
// are required except Custom.
type Spec struct {
	// Kind is the singular noun used in per-agent URL paths and error
	// messages (e.g. "workflow" → /<agent>/workflow/<id>).
	Kind string
	// KindPlural is the API base segment (e.g. "workflows" → /api/workflows).
	KindPlural string
	// ViewHTML is the embedded viewer served at /<agent>/<kind>/<id>.
	ViewHTML string

	// Get returns the entity (any JSON-marshalable shape) and its GitOps
	// source marker. src == "gitops" triggers the 403 gate on DELETE.
	Get func(agent, id string) (entity any, src string, ok bool)
	// List returns the JSON-marshalable list payload, filtered when agent != "".
	List func(agent string) any
	// Delete removes the entity after the GitOps gate has passed.
	Delete func(agent, id string) error

	// Custom is invoked for sub-resources (e.g. /<id>/run) and for verbs
	// other than GET/DELETE on the bare /<agent>/<id> path. Return true
	// once the response has been written; false to fall through to the
	// built-in 404/405.
	Custom func(w http.ResponseWriter, req *http.Request, agent, id string, subpath []string) bool
}

// Mount wires the per-agent view route and the API routes:
//
//	GET    /<agent>/<kind>/<id>            → ViewHTML
//	GET    /<agent>/<kind>/<id>/data.json  → entity JSON
//	GET    /api/<plural>                   → List(agent)
//	GET    /api/<plural>/<agent>/<id>      → Get
//	DELETE /api/<plural>/<agent>/<id>      → Delete (403 if src == "gitops")
//	*      /api/<plural>/<agent>/<id>/...  → Spec.Custom
func Mount(mux, apiMux *http.ServeMux, knownAgents map[string]bool, spec Spec) {
	for agent := range knownAgents {
		a := agent
		mux.HandleFunc("/"+a+"/"+spec.Kind+"/", func(w http.ResponseWriter, req *http.Request) {
			handleAgentView(a, spec, w, req)
		})
	}
	apiMux.HandleFunc("/api/"+spec.KindPlural, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		agent := req.URL.Query().Get("agent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spec.List(agent))
	})
	apiMux.HandleFunc("/api/"+spec.KindPlural+"/", func(w http.ResponseWriter, req *http.Request) {
		handleAPIItem(spec, w, req)
	})
}

func handleAgentView(agent string, spec Spec, w http.ResponseWriter, req *http.Request) {
	trimmed := strings.TrimPrefix(req.URL.Path, "/"+agent+"/"+spec.Kind+"/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, req)
		return
	}
	id := parts[0]
	if !store.IDRe.MatchString(id) {
		http.Error(w, "invalid "+spec.Kind+" id", http.StatusBadRequest)
		return
	}
	entity, _, ok := spec.Get(agent, id)
	if !ok {
		http.NotFound(w, req)
		return
	}
	if len(parts) == 2 && parts[1] == "data.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(entity)
		return
	}
	if len(parts) == 2 && parts[1] != "" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(spec.ViewHTML))
}

func handleAPIItem(spec Spec, w http.ResponseWriter, req *http.Request) {
	trimmed := strings.TrimPrefix(req.URL.Path, "/api/"+spec.KindPlural+"/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "usage: /api/"+spec.KindPlural+"/<agent>/<id>", http.StatusBadRequest)
		return
	}
	agent, id := parts[0], parts[1]
	if !store.AgentRe.MatchString(agent) || !store.IDRe.MatchString(id) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	sub := parts[2:]

	// Sub-resources (e.g. /<id>/run) always go through Custom.
	if len(sub) > 0 {
		if spec.Custom != nil && spec.Custom(w, req, agent, id, sub) {
			return
		}
		http.NotFound(w, req)
		return
	}

	// Bare /<agent>/<id>: built-in GET/DELETE, everything else via Custom.
	switch req.Method {
	case http.MethodGet:
		entity, _, ok := spec.Get(agent, id)
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entity)
	case http.MethodDelete:
		if _, src, ok := spec.Get(agent, id); ok && src == "gitops" {
			http.Error(w, spec.Kind+" is managed via GitOps and cannot be deleted from the UI; remove the file from the source repo instead", http.StatusForbidden)
			return
		}
		if err := spec.Delete(agent, id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		if spec.Custom != nil && spec.Custom(w, req, agent, id, sub) {
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
