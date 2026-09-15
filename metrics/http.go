package metrics

import (
	"net/http"
	"strconv"

	"github.com/justmike1/arbetern/internal/httpx"
)

// RegisterRoutes mounts the read-only performance API:
//
//	GET /api/metrics/summary?days → aggregated latency/throughput JSON
//
// Access control is enforced upstream by the global IP gate in main.
func (s *Store) RegisterRoutes(apiMux *http.ServeMux) {
	apiMux.HandleFunc("/api/metrics/summary", func(w http.ResponseWriter, r *http.Request) {
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
		httpx.WriteJSON(w, http.StatusOK, s.Summarize(days))
	})
}
