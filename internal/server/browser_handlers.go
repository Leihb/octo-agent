package server

import "net/http"

// ─── GET /api/browser/status ────────────────────────────────────────────────

func (s *Server) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	// The Go rewrite does not yet have a persistent browser automation
	// subsystem (Ruby had a CDP-based browser). Return a disabled stub.
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   false,
		"available": false,
		"status":    "not_implemented",
		"message":   "Browser automation is not yet available in the Go rewrite",
	})
}

// ─── POST /api/browser/toggle ───────────────────────────────────────────────

func (s *Server) handleBrowserToggle(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": false,
		"note":    "Browser automation is not yet available in the Go rewrite",
	})
}
