package dashboards

import (
	_ "embed"
	"net/http"

	"github.com/justmike1/arbetern/internal/crud"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes wires the per-agent view and the dashboard CRUD API onto
// the given mux. Routing/validation/GitOps gating live in internal/crud;
// this function only adapts the dashboards-specific Get/List/Delete shape.
//
//	GET    /<agent>/dashboard/<id>           → embedded HTML viewer
//	GET    /<agent>/dashboard/<id>/data.json → latest stored JSON
//	GET    /api/dashboards                   → list (optional ?agent=)
//	DELETE /api/dashboards/<agent>/<id>      → delete
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
	})
}
