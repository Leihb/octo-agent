package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/version"
)

// ─── Request/Response types ─────────────────────────────────────────────────

type createChatRequest struct {
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
	Name    string `json:"name,omitempty"`
}

type createChatResponse struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

type turnRequest struct {
	Message string `json:"message"`
}

// sessionItem is the shape the Web UI expects for each session in listings
// and after creation. It is a superset of the raw agent.Session fields.
type sessionItem struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Model        string    `json:"model"`
	ModelID      string    `json:"model_id,omitempty"`
	Status       string    `json:"status"`
	Source       string    `json:"source"`
	AgentProfile string    `json:"agent_profile"`
	Pinned       bool      `json:"pinned"`
	TotalTasks   int       `json:"total_tasks"`
	TurnCount    int       `json:"turn_count"`
}

type sessionDetail struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	Model     string          `json:"model"`
	Messages  []agent.Message `json:"messages"`
}

// sessionListResponse matches the Ruby frontend's expected list envelope.
type sessionListResponse struct {
	Sessions  []sessionItem `json:"sessions"`
	HasMore   bool          `json:"has_more"`
	CronCount int           `json:"cron_count"`
}

// sessionCreateRequest matches POST /api/sessions bodies from the Web UI.
type sessionCreateRequest struct {
	Name         string `json:"name"`
	AgentProfile string `json:"agent_profile"`
	Source       string `json:"source"`
	Model        string `json:"model,omitempty"`
}

// toSessionItem builds a frontend-friendly session descriptor.
func toSessionItem(s *agent.Session, source, agentProfile string) sessionItem {
	updated := s.CreatedAt
	if p, err := s.SavePath(); err == nil {
		if st, err := os.Stat(p); err == nil {
			updated = st.ModTime()
		}
	}
	name := s.Title
	if name == "" {
		name = s.DisplayTitle()
	}
	return sessionItem{
		ID:           s.ID,
		Name:         name,
		Title:        s.Title,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    updated,
		Model:        s.Model,
		Status:       "idle",
		Source:       source,
		AgentProfile: agentProfile,
		Pinned:       false,
		TotalTasks:   0,
		TurnCount:    s.TurnCount(),
	}
}

type toolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Enabled     bool   `json:"enabled"`
}

// ─── POST /api/chat ─────────────────────────────────────────────────────────

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureSender(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	var req createChatRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	model := s.model
	if req.Model != "" {
		model = req.Model
	}
	sess := agent.NewSession(model, s.system)
	if req.Name != "" {
		_ = sess.SetTitle(req.Name)
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()
	defer mu.Unlock()

	reply, err := s.runTurn(r.Context(), sess, req.Message)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := sess.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save session: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, createChatResponse{
		SessionID: sess.ID,
		Reply:     reply,
	})
}

// handleTurnOrSSE routes turn requests to either JSON or SSE handler.
func (s *Server) handleTurnOrSSE(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleTurnSSE(w, r)
		return
	}
	s.handleTurn(w, r)
}

// ─── POST /api/chat/:id/turn ────────────────────────────────────────────────

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureSender(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	var req turnRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	sess, err := agent.LoadSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()
	defer mu.Unlock()

	reply, err := s.runTurn(r.Context(), sess, req.Message)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := sess.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save session: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"reply": reply})
}

// ─── GET /api/sessions ──────────────────────────────────────────────────────

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := agent.ListSessions(50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]sessionItem, 0, len(sessions))
	cronCount := 0
	for _, sess := range sessions {
		// Source and agent_profile are not persisted on the session itself in
		// the Go rewrite; default to "manual" / "general" so the UI renders.
		item := toSessionItem(sess, "manual", "general")
		out = append(out, item)
		if item.Source == "cron" {
			cronCount++
		}
	}
	writeJSON(w, http.StatusOK, sessionListResponse{
		Sessions:  out,
		HasMore:   false,
		CronCount: cronCount,
	})
}

// ─── POST /api/sessions ─────────────────────────────────────────────────────

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req sessionCreateRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	model := s.model
	if req.Model != "" {
		model = req.Model
	}
	if model == "" {
		// Fall back to the user's configured default model.
		if cfg, err := config.Load(); err == nil && cfg.Model != "" {
			model = cfg.Model
		}
	}
	if model == "" {
		writeError(w, http.StatusBadRequest, "no default model configured")
		return
	}

	agentProfile := req.AgentProfile
	if agentProfile == "" {
		agentProfile = "general"
	}
	source := req.Source
	if source == "" {
		source = "manual"
	}

	sess := agent.NewSession(model, "")
	if req.Name != "" {
		sess.Title = req.Name
	}
	if err := sess.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save session: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"session": toSessionItem(sess, source, agentProfile)})
}

// ─── GET /api/sessions/:id/messages ─────────────────────────────────────────

func (s *Server) handleGetSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	sess, err := agent.LoadSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// The Web UI expects an event stream that mirrors the live WS traffic.
	// We translate the persisted message list into user/assistant events and
	// reconstruct tool_call / tool_result pairs from tool_use / tool_result
	// blocks so the history replay is visually complete.
	events := make([]map[string]any, 0, len(sess.Messages)*2)
	for i, m := range sess.Messages {
		switch m.Role {
		case agent.RoleUser:
			// Emit tool_result events for any tool_result blocks before the
			// user message (they carry the actual output).
			for _, b := range m.Blocks {
				if b.Type == "tool_result" {
					events = append(events, map[string]any{
						"type":   "tool_result",
						"result": b.Result,
					})
				}
			}
			// Use the message's own CreatedAt when available.  Older session
			// files don't have per-message timestamps, so fall back to the
			// array index as a unique cursor (not sess.CreatedAt — that
			// collides with the Web UI's dedup logic and drops everything
			// after the first user message).
			createdAt := m.CreatedAt.Unix()
			if m.CreatedAt.IsZero() {
				createdAt = int64(i + 1)
			}
			// Only emit history_user_message if there is actual text content
			// (tool_result-only messages are bookkeeping, not user-visible).
			if m.Content != "" {
				events = append(events, map[string]any{
					"type":       "history_user_message",
					"content":    m.Content,
					"created_at": createdAt,
				})
			}
		case agent.RoleAssistant:
			// Emit tool_call events for any tool_use blocks.
			for _, b := range m.Blocks {
				if b.Type == "tool_use" {
					events = append(events, map[string]any{
						"type": "tool_call",
						"name": b.Name,
						"args": b.Input,
					})
				}
			}
			events = append(events, map[string]any{
				"type":    "assistant_message",
				"content": m.Content,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"has_more": false,
		"events":   events,
	})
}

// ─── GET /api/sessions/:id ──────────────────────────────────────────────────

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	sess, err := agent.LoadSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionDetail{
		ID:        sess.ID,
		CreatedAt: sess.CreatedAt,
		Model:     sess.Model,
		Messages:  sess.Messages,
	})
}

// ─── DELETE /api/sessions/:id ───────────────────────────────────────────────

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	if err := agent.DeleteSession(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.forgetTurnLock(id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": []string{id}})
}

// ─── POST /api/sessions/delete (batch) ──────────────────────────────────────

type deleteSessionsRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) handleDeleteSessions(w http.ResponseWriter, r *http.Request) {
	var req deleteSessionsRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}

	deleted := make([]string, 0, len(req.IDs))
	failed := map[string]string{}
	for _, id := range req.IDs {
		if err := agent.DeleteSession(id); err != nil {
			failed[id] = err.Error()
			continue
		}
		s.forgetTurnLock(id)
		deleted = append(deleted, id)
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "failed": failed})
}

// ─── GET /api/tools ─────────────────────────────────────────────────────────

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	defs := tools.DefaultTools()
	out := make([]toolInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, toolInfo{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Parameters,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── GET /api/skills ────────────────────────────────────────────────────────

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	list := s.skillReg.All()
	out := make([]skillInfo, 0, len(list))
	for _, sk := range list {
		out = append(out, skillInfo{
			Name:        sk.Name,
			Description: sk.Description,
			Source:      sk.Source,
			Enabled:     s.skillReg.IsEnabled(sk.Name),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// ─── GET /api/health ────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	_ = r
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── GET /api/version ───────────────────────────────────────────────────────

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}

// ─── Turn execution ─────────────────────────────────────────────────────────

// runTurn executes one user message against a session. It builds the agent,
// runs the tool loop if enabled, and returns the assistant's text reply.
func (s *Server) runTurn(ctx context.Context, sess *agent.Session, userInput string) (string, error) {
	ctx = context.WithValue(ctx, ctxKeySessionID{}, sess.ID)
	a := s.buildAgent(sess)

	if !s.cfg.Tools {
		reply, err := a.Turn(ctx, userInput)
		if err != nil {
			return "", err
		}
		sess.SyncFrom(a.History)
		return reply.Content, nil
	}

	// Tool-enabled path: wire the per-turn tool environment (gate + ctx-scoped
	// sub-agent manager + task store) bound to this turn's agent.
	ctx, executor, err := s.prepareToolTurn(ctx, a)
	if err != nil {
		return "", err
	}

	reply, err := a.Run(ctx, userInput, tools.DefaultToolsFor(a.Model), executor)
	if err != nil {
		return "", err
	}

	sess.SyncFrom(a.History)
	return reply.Content, nil
}

// prepareToolTurn wires the per-turn tool environment for agent a: the strict,
// non-interactive permission gate, plus a sub-agent manager and task store
// bound to THIS turn's agent and stamped into ctx so the sub-agent / task tools
// dispatch to them rather than the process-global gating sentinels. The manager
// runs synchronously — a request/response turn has no follow-up channel for an
// async sub-agent result — and each turn gets a private store, so concurrent
// sessions never share sub-agent or task state. Returns the augmented ctx and
// the executor to run the turn with.
func (s *Server) prepareToolTurn(ctx context.Context, a *agent.Agent) (context.Context, agent.ToolExecutor, error) {
	executor := tools.NewDefaultRegistry()

	engine, err := permission.New(permissionConfigPath(), s.cwd, resolvePermissionMode())
	if err != nil {
		return ctx, nil, fmt.Errorf("permission engine: %w", err)
	}
	// Wire interactive permission confirmation when we know the session.
	var ask app.PermissionAsk
	if sid, ok := ctx.Value(ctxKeySessionID{}).(string); ok && sid != "" {
		ask = s.permissionAskFrom(sid)
	}
	a.Gate = app.NewPermissionGate(engine, ask)

	spawner := app.NewSpawner(a, executor, func() []agent.ToolDefinition {
		return tools.DefaultToolsFor(a.Model)
	})
	mgr := tools.NewSubAgentManager(spawner)
	mgr.SetSynchronous(true)
	ctx = tools.WithSubAgentManager(ctx, mgr)
	ctx = tools.WithTaskStore(ctx, tasks.New())

	return ctx, executor, nil
}

func permissionConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".octo", "permissions.yml")
}

// resolvePermissionMode reads the persisted config and returns the configured
// permission mode (or ModeInteractive as the default). This lets the server
// pick up mode changes written by the setup panel / onboard skill without
// restarting.
func resolvePermissionMode() permission.Mode {
	cfg, _ := config.Load()
	switch cfg.PermissionMode {
	case string(permission.ModeAutoApprove):
		return permission.ModeAutoApprove
	default:
		return permission.ModeInteractive
	}
}

// ─── POST /api/file-action ────────────────────────────────────────────────

// fileActionRequest is sent when the user clicks a file:// link in chat.
type fileActionRequest struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "open" or "download"
}

// handleFileAction handles file:// links from the chat UI.
// When action is "open" and the server is running on localhost,
// it attempts to open the file with the OS default handler.
// When action is "download", it streams the file contents back.
func (s *Server) handleFileAction(w http.ResponseWriter, r *http.Request) {
	var req fileActionRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	switch req.Action {
	case "open":
		// Only allow opening files on localhost for security.
		host := r.Host
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			writeError(w, http.StatusForbidden, "open action only allowed on localhost")
			return
		}
		var cmd string
		switch runtime.GOOS {
		case "darwin":
			cmd = "open"
		case "windows":
			cmd = "start"
		default:
			cmd = "xdg-open"
		}
		if err := exec.Command(cmd, req.Path).Start(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "opened"})

	case "download":
		f, err := os.Open(req.Path)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(req.Path)+"\"")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
		http.ServeContent(w, r, filepath.Base(req.Path), info.ModTime(), f)

	default:
		writeError(w, http.StatusBadRequest, "unknown action")
	}
}
