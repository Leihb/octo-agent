package server

import (
	"net/http"

	"github.com/Leihb/octo-agent/internal/scheduler"
)

// ─── GET /api/cron-tasks ────────────────────────────────────────────────────

func (s *Server) handleListCronTasks(w http.ResponseWriter, r *http.Request) {
	// Delegate to the existing scheduler-backed task list and wrap the
	// bare array into the envelope the Web UI expects.
	s.initScheduler()
	if s.scheduler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"cron_tasks": []taskResponse{}})
		return
	}
	tasks := s.scheduler.List()
	out := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskToResponse(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron_tasks": out})
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

	// Find task by name or id and run it. The run happens in the background —
	// an agent turn can take minutes, far longer than a browser fetch survives.
	for _, t := range s.scheduler.List() {
		if t.Name == name || t.ID == name {
			if err := s.scheduler.RunNow(t.ID); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "id": t.ID})
			return
		}
	}
	writeError(w, http.StatusNotFound, "task not found")
}

// ─── PATCH /api/cron-tasks/{name} ───────────────────────────────────────────

type patchCronTaskRequest struct {
	Enabled   *bool                    `json:"enabled,omitempty"`
	Cron      *string                  `json:"cron,omitempty"`
	Prompt    *string                  `json:"prompt,omitempty"`
	Model     *string                  `json:"model,omitempty"`
	Agent     *string                  `json:"agent,omitempty"`
	Directory *string                  `json:"directory,omitempty"`
	Notify    *scheduler.NotifyTargets `json:"notify,omitempty"`
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

	// Find task by name or id and update.
	for _, t := range s.scheduler.List() {
		if t.Name == name || t.ID == name {
			changed := false
			if req.Enabled != nil {
				t.Enabled = *req.Enabled
				changed = true
			}
			if req.Cron != nil {
				t.Cron = *req.Cron
				changed = true
			}
			if req.Prompt != nil {
				t.Prompt = *req.Prompt
				changed = true
			}
			if req.Model != nil {
				t.Model = *req.Model
				changed = true
			}
			if req.Agent != nil {
				t.Agent = *req.Agent
				changed = true
			}
			if req.Directory != nil {
				t.Directory = *req.Directory
				changed = true
			}
			if req.Notify != nil {
				t.Notify = *req.Notify
				changed = true
			}
			if changed {
				if err := s.scheduler.Update(t); err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
			return
		}
	}
	writeError(w, http.StatusNotFound, "task not found")
}
