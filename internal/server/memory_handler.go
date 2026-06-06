package server

import (
	"net/http"
	"os"
	"path/filepath"
)

// ─── GET /api/memories/{filename} ───────────────────────────────────────────

func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	fname := r.PathValue("filename")
	if fname == "" {
		writeError(w, http.StatusBadRequest, "missing filename")
		return
	}

	// Security: prevent path traversal.
	fname = filepath.Base(fname)
	p := filepath.Join(octoDir(), "memories", fname)

	data, err := os.ReadFile(p)
	if err != nil {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"filename": fname,
		"content":  string(data),
		"path":     p,
	})
}

// ─── DELETE /api/memories/{filename} ────────────────────────────────────────

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	fname := r.PathValue("filename")
	if fname == "" {
		writeError(w, http.StatusBadRequest, "missing filename")
		return
	}

	fname = filepath.Base(fname)
	p := filepath.Join(octoDir(), "memories", fname)

	if err := os.Remove(p); err != nil {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
