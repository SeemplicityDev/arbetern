// Package crud provides the shared HTTP handlers for the workflows and
// dashboards CRUD APIs: the per-agent data.json route, the redirect from the
// legacy view path into the management console, and the REST list/get/delete
// verbs with their GitOps 403 gate. Per-package extras (POST /<id>/run,
// PATCH/PUT for workflows) are wired in via Spec.Custom.
package crud

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/justmike1/arbetern/internal/httpx"
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

	// Authorize gates every request that changes state or triggers a run. It
	// receives the owning agent so the check can use that agent's allow-lists:
	// editing or running a workflow drives that agent's tools, so it needs the
	// same permission as using the agent. A nil Authorize allows everything.
	Authorize func(req *http.Request, agent string) bool
}

// writes reports whether the method changes state or starts work.
func writes(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// permit applies the same-origin check and the Spec authorizer to a
// state-changing request, writing the rejection itself when it fails.
func (spec Spec) permit(w http.ResponseWriter, req *http.Request, agent string) bool {
	if !writes(req.Method) {
		return true
	}
	if err := httpx.CheckSameOrigin(req); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return false
	}
	if spec.Authorize != nil && !spec.Authorize(req, agent) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// Mount wires the per-agent routes and the API routes:
//
//	GET    /<agent>/<kind>/<id>            → 302 to /ui/<agent>/<kind>/<id>
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
	http.Redirect(w, req, ViewPath(spec.Kind, agent, id), http.StatusFound)
}

// ViewPath is the management-console page that renders one entity.
func ViewPath(kind, agent, id string) string {
	return "/ui/" + agent + "/" + kind + "/" + id
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
	if !spec.permit(w, req, agent) {
		return
	}

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
