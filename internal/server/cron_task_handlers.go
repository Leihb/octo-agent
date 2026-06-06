package server

import (
	"net/http"
)

// ─── GET /api/cron-tasks ────────────────────────────────────────────────────

func (s *Server) handleListCronTasks(w http.ResponseWriter, r *http.Request) {
	// Delegate to the existing scheduler-backed task list.
	s.handleListTasks(w, r)
}

// ─── POST /api/cron-tasks/{name}/run ────────────────────────────────────────

func (s *Server) handleRunCronTask(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing task name")
		return
	}

	s.initScheduler()
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "scheduler not available")
		return
	}

	// Find task by name and run it.
	for _, t := range s.scheduler.List() {
		if t.Name == name {
			sessionID, err := s.scheduler.RunNow(r.Context(), t.ID)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"session_id": sessionID})
			return
		}
	}
	writeError(w, http.StatusNotFound, "task not found")
}

// ─── PATCH /api/cron-tasks/{name} ───────────────────────────────────────────

type patchCronTaskRequest struct {
	Enabled *bool `json:"enabled,omitempty"`
}

func (s *Server) handlePatchCronTask(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing task name")
		return
	}

	var req patchCronTaskRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	s.initScheduler()
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "scheduler not available")
		return
	}

	// Find task by name and update.
	for _, t := range s.scheduler.List() {
		if t.Name == name {
			if req.Enabled != nil {
				t.Enabled = *req.Enabled
				_ = s.scheduler.Update(t)
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
			return
		}
	}
	writeError(w, http.StatusNotFound, "task not found")
}
