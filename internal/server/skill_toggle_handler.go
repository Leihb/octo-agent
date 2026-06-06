package server

import (
	"net/http"
)

// ─── PATCH /api/skills/{name}/toggle ────────────────────────────────────────

func (s *Server) handleToggleSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing skill name")
		return
	}

	// The skill registry in the Go rewrite does not yet persist toggled state
	// to disk; this is a no-op stub that returns success so the UI doesn't
	// break. A full implementation would write to ~/.octo/skills.yml or
	// similar and re-read on server start.
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    name,
		"enabled": true,
		"note":    "skill toggle is not yet persisted in the Go rewrite",
	})
}
