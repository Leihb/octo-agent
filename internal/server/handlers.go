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
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/tools"
)

// ─── Request/Response types ─────────────────────────────────────────────────

type createChatRequest struct {
	Message string `json:"message"`
}

type createChatResponse struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

type turnRequest struct {
	Message string `json:"message"`
}

type sessionSummary struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Model     string    `json:"model"`
	TurnCount int       `json:"turn_count"`
	Title     string    `json:"title"`
}

type sessionDetail struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	Model     string          `json:"model"`
	Messages  []agent.Message `json:"messages"`
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

	sess := agent.NewSession(s.model, s.system)

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

	out := make([]sessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionSummary{
			ID:        sess.ID,
			CreatedAt: sess.CreatedAt,
			Model:     sess.Model,
			TurnCount: sess.TurnCount(),
			Title:     sess.DisplayTitle(),
		})
	}
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, out)
}

// ─── GET /api/health ────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	// Tool-enabled path: build executor and permission gate.
	executor := tools.NewDefaultRegistry()
	toolDefs := tools.DefaultTools()

	// Server permission gate: strict mode — ask → deny.
	cwd := s.cwd
	engine, err := permission.New(permissionConfigPath(), cwd, permission.ModeStrict)
	if err != nil {
		return "", fmt.Errorf("permission engine: %w", err)
	}
	a.Gate = &serverPermissionGate{engine: engine}

	reply, err := a.Run(ctx, userInput, toolDefs, executor)
	if err != nil {
		return "", err
	}

	sess.SyncFrom(a.History)
	return reply.Content, nil
}

// serverPermissionGate adapts permission.Engine into agent.PermissionGate.
// In server mode, "ask" resolves to "deny" (strict mode already does this,
// but we double-check here for clarity).
type serverPermissionGate struct {
	engine *permission.Engine
}

func (g *serverPermissionGate) Check(ctx context.Context, name string, input map[string]any) (bool, string) {
	switch g.engine.Check(name, input) {
	case permission.Allow:
		return true, ""
	case permission.Deny:
		return false, g.engine.DenialReason(name, input)
	case permission.Ask:
		return false, g.engine.DenialReason(name, input)
	}
	return false, g.engine.DenialReason(name, input)
}

func permissionConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".octo", "permissions.yml")
}
