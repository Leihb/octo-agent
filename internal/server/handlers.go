package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
}

// ─── POST /api/chat ─────────────────────────────────────────────────────────

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
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
	// For now we translate the persisted message list into user/assistant
	// events; tool calls are not reconstructed from the transcript.
	events := make([]map[string]any, 0, len(sess.Messages))
	for _, m := range sess.Messages {
		switch m.Role {
		case agent.RoleUser:
			events = append(events, map[string]any{
				"type":       "history_user_message",
				"content":    m.Content,
				"created_at": sess.CreatedAt,
			})
		case agent.RoleAssistant:
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
	list := s.skillReg.List()
	out := make([]skillInfo, 0, len(list))
	for _, sk := range list {
		out = append(out, skillInfo{
			Name:        sk.Name,
			Description: sk.Description,
			Source:      sk.Source,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// ─── GET /api/health ────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// When an access_key is provided (query, header, or cookie), validate it
	// so the Web UI can use this endpoint to verify the key before storing it.
	// Without any key, stay public.
	hasKey := r.URL.Query().Get("access_key") != "" ||
		r.Header.Get("X-Access-Key") != "" ||
		r.Header.Get("Authorization") != ""
	if !hasKey {
		if _, err := r.Cookie("octo_access_key"); err == nil {
			hasKey = true
		}
	}
	if hasKey && !s.validateAccessKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
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

	engine, err := permission.New(permissionConfigPath(), s.cwd, permission.ModeStrict)
	if err != nil {
		return ctx, nil, fmt.Errorf("permission engine: %w", err)
	}
	a.Gate = app.NewPermissionGate(engine, nil)

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
