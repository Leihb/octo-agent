package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/Leihb/octo-agent/internal/scheduler"
	"github.com/Leihb/octo-agent/internal/tools"
)

// ─── Tasks REST API ─────────────────────────────────────────────────────────

type taskRequest struct {
	Name   string                  `json:"name"`
	Cron   string                  `json:"cron"`
	Prompt string                  `json:"prompt"`
	Model  string                  `json:"model,omitempty"`
	Agent  string                  `json:"agent,omitempty"`
	Notify *scheduler.NotifyTarget `json:"notify,omitempty"`
}

type taskResponse struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Cron      string                  `json:"cron"`
	Prompt    string                  `json:"prompt"`
	Model     string                  `json:"model,omitempty"`
	Agent     string                  `json:"agent,omitempty"`
	Notify    *scheduler.NotifyTarget `json:"notify,omitempty"`
	Enabled   bool                    `json:"enabled"`
	CreatedAt string                  `json:"created_at,omitempty"`
	LastRun   string                  `json:"last_run,omitempty"`
	SessionID string                  `json:"session_id,omitempty"`
}

// initScheduler creates the scheduler if not already initialized. It is
// called eagerly from ListenAndServe so scheduled tasks fire from server
// start; the calls in individual handlers remain as a safety net (and as the
// only path in tests that exercise the mux directly).
func (s *Server) initScheduler() {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.scheduler != nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".octo", "tasks")
	sch, err := scheduler.New(dir, s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "octo serve: scheduler: %v\n", err)
		return
	}
	sch.Start()
	s.scheduler = sch
}

// RunTask implements scheduler.Runner. It executes a scheduled task by
// creating (or reusing) a session and running a single turn.
func (s *Server) RunTask(ctx context.Context, task scheduler.Task) (string, error) {
	// Try to load an existing session for this task.
	sess, err := agent.LoadSession(task.SessionID)
	if err != nil {
		// Create a new session.
		model := task.Model
		if model == "" {
			model = s.model
		}
		sess = agent.NewSession(model, s.system)
		sess.Source = "cron"
		task.SessionID = sess.ID
		// If the task specifies a directory, note it in system prompt.
		if task.Directory != "" {
			sess.System = fmt.Sprintf("Working directory: %s\n%s", task.Directory, sess.System)
		}
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()
	defer mu.Unlock()

	a := s.buildAgent(sess)

	var toolDefs []agent.ToolDefinition
	var executor agent.ToolExecutor
	if s.cfg.Tools {
		var perr error
		ctx, executor, _, perr = s.prepareToolTurn(ctx, a)
		if perr != nil {
			return sess.ID, fmt.Errorf("prepare tools: %w", perr)
		}
		toolDefs = tools.DefaultToolsFor(a.Model)
	}

	reply, err := a.Run(ctx, task.Prompt, toolDefs, executor)
	if err != nil {
		sess.SyncFrom(a.History)
		_ = sess.Save()
		s.notifyTaskResult(task, fmt.Sprintf("⏰ %s failed: %v", task.Name, err))
		return sess.ID, fmt.Errorf("run task: %w", err)
	}
	s.notifyTaskResult(task, fmt.Sprintf("⏰ %s\n\n%s", task.Name, reply.Content))

	sess.SyncFrom(a.History)
	if err := sess.Save(); err != nil {
		return sess.ID, fmt.Errorf("save session: %w", err)
	}

	return sess.ID, nil
}

// notifyTaskResult pushes a task run's outcome to its IM notify target, if
// one is configured. Delivery failures are logged, never fatal — the run
// itself already happened and is recorded in the session.
func (s *Server) notifyTaskResult(task scheduler.Task, text string) {
	if task.Notify == nil {
		return
	}
	if err := channel.SendOnce(task.Notify.Platform, task.Notify.ChatID, text); err != nil {
		log.Printf("[scheduler] task %q notify: %v", task.Name, err)
	}
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	s.initScheduler()
	if s.scheduler == nil {
		writeJSON(w, http.StatusOK, []taskResponse{})
		return
	}
	tasks := s.scheduler.List()
	out := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskToResponse(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	s.initScheduler()
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "scheduler not available")
		return
	}
	var req taskRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" || req.Cron == "" || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "name, cron, and prompt are required")
		return
	}
	task := scheduler.Task{
		Name:    req.Name,
		Cron:    req.Cron,
		Prompt:  req.Prompt,
		Model:   req.Model,
		Agent:   req.Agent,
		Notify:  req.Notify,
		Enabled: true,
	}
	if err := s.scheduler.Add(&task); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": task.ID})
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	s.initScheduler()
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "scheduler not available")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing task id")
		return
	}
	if err := s.scheduler.Delete(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunTask(w http.ResponseWriter, r *http.Request) {
	s.initScheduler()
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "scheduler not available")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing task id")
		return
	}
	if err := s.scheduler.RunNow(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "id": id})
}

func taskToResponse(t scheduler.Task) taskResponse {
	r := taskResponse{
		ID:      t.ID,
		Name:    t.Name,
		Cron:    t.Cron,
		Prompt:  t.Prompt,
		Model:   t.Model,
		Agent:   t.Agent,
		Notify:  t.Notify,
		Enabled: t.Enabled,
	}
	if !t.CreatedAt.IsZero() {
		r.CreatedAt = t.CreatedAt.Format("2006-01-02T15:04:05Z")
	}
	if !t.LastRun.IsZero() {
		r.LastRun = t.LastRun.Format("2006-01-02T15:04:05Z")
	}
	r.SessionID = t.SessionID
	return r
}
