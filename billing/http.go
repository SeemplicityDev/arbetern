package billing

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// RegisterRoutes mounts the read-only usage API and keeps the legacy
// /billing URL working by redirecting to the Usage & Billing page in the UI:
//
//	GET /billing                  → 302 /ui/billing
//	GET /api/billing/summary?days → aggregated usage/spend JSON
//
// Access control is enforced upstream by the global IP gate in main.
func (s *Store) RegisterRoutes(mux, apiMux *http.ServeMux) {
	mux.HandleFunc("/billing", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/billing", http.StatusFound)
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
