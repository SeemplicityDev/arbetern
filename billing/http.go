package billing

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
)

//go:embed view.html
var viewHTML string

// RegisterRoutes mounts the Usage & Billing viewer and its read-only API:
//
//	GET /billing                  → embedded HTML dashboard
//	GET /api/billing/summary?days → aggregated usage/spend JSON
//
// Access control is enforced upstream by the global IP gate in main.
func (s *Store) RegisterRoutes(mux, apiMux *http.ServeMux) {
	mux.HandleFunc("/billing", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(viewHTML))
	})
	apiMux.HandleFunc("/api/billing/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.Summarize(days))
	})
}
