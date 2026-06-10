package server

import (
	"context"
	"net/http"
	"sync"

	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/mcp"
)

// Web-side MCP OAuth: the CLI runs the device flow inline (print code, wait),
// but a browser can't block an HTTP request for minutes. So the flow is
// split: POST …/oauth/start launches the connect in a goroutine with a
// prompt that captures the device code, and the panel polls
// GET …/oauth/status until the state settles. The token lands in the usual
// ~/.octo/mcp-tokens/<server>.json cache, so every later connect (including
// `octo serve` restarts) reuses it without re-authorizing.

// mcpOAuthFlow tracks one in-flight device flow. It doubles as the
// mcp.OAuthPrompt handed to the connect path.
type mcpOAuthFlow struct {
	mu                      sync.Mutex
	state                   string // "starting" | "authorizing" | "connected" | "failed"
	userCode                string
	verificationURI         string
	verificationURIComplete string
	err                     string
}

// ShowAuthorization implements mcp.OAuthPrompt: the device flow has the
// code, the user now needs to see it.
func (f *mcpOAuthFlow) ShowAuthorization(userCode, verificationURI, verificationURIComplete string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = "authorizing"
	f.userCode = userCode
	f.verificationURI = verificationURI
	f.verificationURIComplete = verificationURIComplete
}

func (f *mcpOAuthFlow) Progress() {}
func (f *mcpOAuthFlow) Done()     {}

func (f *mcpOAuthFlow) finish(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.state = "failed"
		f.err = err.Error()
		return
	}
	f.state = "connected"
}

func (f *mcpOAuthFlow) snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{"state": f.state}
	if f.userCode != "" {
		out["user_code"] = f.userCode
		out["verification_uri"] = f.verificationURI
	}
	if f.verificationURIComplete != "" {
		out["verification_uri_complete"] = f.verificationURIComplete
	}
	if f.err != "" {
		out["error"] = f.err
	}
	return out
}

// inFlight reports whether the flow is still running (a finished flow can be
// replaced by a fresh start).
func (f *mcpOAuthFlow) inFlight() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state == "starting" || f.state == "authorizing"
}

// ─── POST /api/mcp/servers/{name}/oauth/start ───────────────────────────────

func (s *Server) handleStartMCPOAuth(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()

	if !s.cfg.Tools {
		writeError(w, http.StatusConflict, "tools are disabled on this server")
		return
	}

	managed, err := mcp.LoadManaged(s.cwd)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var entry *mcp.ManagedServer
	for i := range managed {
		if managed[i].Name == name {
			entry = &managed[i]
			break
		}
	}
	switch {
	case entry == nil:
		writeError(w, http.StatusNotFound, "mcp server not found")
		return
	case entry.Invalid != "":
		writeError(w, http.StatusConflict, "cannot authorize an invalid entry: "+entry.Invalid)
		return
	case entry.Entry.Disabled:
		writeError(w, http.StatusConflict, "server is disabled; enable it first")
		return
	case entry.Entry.Auth != "oauth":
		writeError(w, http.StatusBadRequest, "server is not configured for oauth")
		return
	}

	if s.mcpOAuthFlows == nil {
		s.mcpOAuthFlows = map[string]*mcpOAuthFlow{}
	}
	// Idempotent while a flow is running: return its current snapshot so a
	// double-click (or a second tab) doesn't spawn a competing device flow.
	if existing := s.mcpOAuthFlows[name]; existing != nil && existing.inFlight() {
		writeJSON(w, http.StatusOK, existing.snapshot())
		return
	}

	flow := &mcpOAuthFlow{state: "starting"}
	s.mcpOAuthFlows[name] = flow
	serverEntry := entry.Entry
	go func() {
		// Background context: the flow outlives the start request by design.
		flow.finish(app.ConnectMCPServerAuth(context.Background(), name, serverEntry, flow, nil))
	}()

	writeJSON(w, http.StatusOK, flow.snapshot())
}

// ─── GET /api/mcp/servers/{name}/oauth/status ───────────────────────────────

func (s *Server) handleMCPOAuthStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	s.mcpMu.Lock()
	flow := s.mcpOAuthFlows[name]
	s.mcpMu.Unlock()

	if flow == nil {
		writeError(w, http.StatusNotFound, "no oauth flow for this server")
		return
	}
	writeJSON(w, http.StatusOK, flow.snapshot())
}
