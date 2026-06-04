package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/skills"
)

func TestValidateBindAddr_LocalhostAllowed(t *testing.T) {
	cases := []string{
		"127.0.0.1:8080",
		"localhost:8080",
		"[::1]:8080",
		":8080",
	}
	for _, addr := range cases {
		if err := validateBindAddr(addr); err != nil {
			t.Errorf("validateBindAddr(%q) = %v, want nil", addr, err)
		}
	}
}

func TestValidateBindAddr_NonLocalhostRequiresPermissions(t *testing.T) {
	// Point HOME to a temp dir without permissions.yml.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp) // Windows uses USERPROFILE

	addr := "0.0.0.0:8080"
	if err := validateBindAddr(addr); err == nil {
		t.Errorf("validateBindAddr(%q) = nil, want error", addr)
	} else if !strings.Contains(err.Error(), "permissions.yml") {
		t.Errorf("error should mention permissions.yml: %v", err)
	}
}

func TestValidateBindAddr_NonLocalhostWithPermissions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp) // Windows uses USERPROFILE

	octoDir := filepath.Join(tmp, ".octo")
	if err := os.MkdirAll(octoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	permFile := filepath.Join(octoDir, "permissions.yml")
	if err := os.WriteFile(permFile, []byte("terminal:\n  - allow: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	addr := "0.0.0.0:8080"
	if err := validateBindAddr(addr); err != nil {
		t.Errorf("validateBindAddr(%q) = %v, want nil", addr, err)
	}
}

func TestHandleHealth(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want ok", body["status"])
	}
}

func TestHandleListSessions(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body []sessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("expected non-nil slice")
	}
}

func TestHandleGetSession_NotFound(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleDeleteSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	sess.Messages = []agent.Message{{Role: agent.RoleUser, Content: "hi"}}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sess.ID, nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if _, err := agent.LoadSession(sess.ID); err == nil {
		t.Fatal("session still loadable after delete")
	}
}

func TestHandleDeleteSessions_Batch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	var ids []string
	for i := 0; i < 3; i++ {
		sess := agent.NewSession("stub-model", "")
		sess.Messages = []agent.Message{{Role: agent.RoleUser, Content: "hi"}}
		if err := sess.Save(); err != nil {
			t.Fatalf("save: %v", err)
		}
		ids = append(ids, sess.ID)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	payload, _ := json.Marshal(deleteSessionsRequest{IDs: ids})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/delete", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Deleted []string          `json:"deleted"`
		Failed  map[string]string `json:"failed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Deleted) != 3 {
		t.Fatalf("deleted = %v, want 3", body.Deleted)
	}
	for _, id := range ids {
		if _, err := agent.LoadSession(id); err == nil {
			t.Errorf("session %s still loadable after batch delete", id)
		}
	}
}

func TestHandleDeleteSessions_EmptyIDs(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/delete", bytes.NewReader([]byte(`{"ids":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreateChat_MissingMessage(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	body := bytes.NewReader([]byte(`{}`))
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleTurn_MissingSession(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	body := bytes.NewReader([]byte(`{"message":"hello"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/chat/nonexistent/turn", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestStaticHandler_IndexFallback(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(w, req)

	// The static handler must serve the SPA entrypoint at "/" with a 200 and
	// HTML body. It must never reply with a redirect: http.FileServer's
	// "/index.html" → "./" canonicalisation would bounce "/" back to itself
	// in an infinite loop, leaving the UI unable to load.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("expected HTML body, got %q", w.Body.String())
	}
}

func TestStaticHandler_APIFallback(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/unknown", nil)
	w := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(w, req)

	// The mux handles API 404s; accept 404 Not Found.
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestCORS(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false, CORSOrigins: []string{"http://localhost:3000"}})

	req := httptest.NewRequest(http.MethodOptions, "/api/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("CORS origin = %q, want http://localhost:3000", got)
	}
}

func TestCORS_DisallowedOrigin(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false, CORSOrigins: []string{"http://localhost:3000"}})

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("expected no CORS header for disallowed origin")
	}
}

// mustServer creates a Server for testing. It skips tests that need a real
// provider by using a stub sender.
func mustServer(t *testing.T, cfg Config) *Server {
	t.Helper()

	// Create a minimal server with stubbed provider.
	srv := &Server{
		cfg:            cfg,
		mux:            http.NewServeMux(),
		model:          "stub-model",
		system:         "",
		skillReg:       skills.Discover(""),
		skillsManifest: "",
		cwd:            "",
		envCtx:         "",
		turnLocks:      map[string]*sync.Mutex{},
		sender:         &stubSender{},
	}
	srv.registerRoutes()
	// Wrap with CORS middleware so tests that exercise CORS hit the right layer.
	srv.http = &http.Server{Handler: srv.corsMiddleware(srv.mux)}
	return srv
}

// stubSender is a no-network agent.Sender for tests. It satisfies the full
// tool-aware surface so the agent loop never degrades, and always replies
// "stub reply".
type stubSender struct{}

func (stubSender) SendMessages(_ context.Context, _, _ string, _ []agent.Message, _ int) (agent.Reply, error) {
	return agent.Reply{Content: "stub reply"}, nil
}

func (stubSender) StreamMessages(_ context.Context, _, _ string, _ []agent.Message, _ int, onChunk func(string), _ func(string)) (agent.Reply, error) {
	if onChunk != nil {
		onChunk("stub reply")
	}
	return agent.Reply{Content: "stub reply"}, nil
}

func (stubSender) SendMessagesWithTools(_ context.Context, _, _ string, _ []agent.Message, _ int, _ []agent.ToolDefinition) (agent.Reply, error) {
	return agent.Reply{Content: "stub reply"}, nil
}

func (stubSender) StreamMessagesWithTools(_ context.Context, _, _ string, _ []agent.Message, _ int, _ []agent.ToolDefinition, onChunk func(string), _ agent.ToolInputDeltaFunc, _ agent.ThinkingDeltaFunc) (agent.Reply, error) {
	if onChunk != nil {
		onChunk("stub reply")
	}
	return agent.Reply{Content: "stub reply"}, nil
}

// recordingSender captures the tool definitions the agent layer forwarded so a
// test can assert the full tool set reached the wire.
type recordingSender struct{ lastTools []agent.ToolDefinition }

func (s *recordingSender) SendMessages(_ context.Context, _, _ string, _ []agent.Message, _ int) (agent.Reply, error) {
	return agent.Reply{Content: "ok"}, nil
}

func (s *recordingSender) StreamMessages(_ context.Context, _, _ string, _ []agent.Message, _ int, onChunk func(string), _ func(string)) (agent.Reply, error) {
	if onChunk != nil {
		onChunk("ok")
	}
	return agent.Reply{Content: "ok"}, nil
}

func (s *recordingSender) SendMessagesWithTools(_ context.Context, _, _ string, _ []agent.Message, _ int, toolDefs []agent.ToolDefinition) (agent.Reply, error) {
	s.lastTools = toolDefs
	return agent.Reply{Content: "ok"}, nil
}

func (s *recordingSender) StreamMessagesWithTools(_ context.Context, _, _ string, _ []agent.Message, _ int, toolDefs []agent.ToolDefinition, onChunk func(string), _ agent.ToolInputDeltaFunc, _ agent.ThinkingDeltaFunc) (agent.Reply, error) {
	s.lastTools = toolDefs
	if onChunk != nil {
		onChunk("ok")
	}
	return agent.Reply{Content: "ok"}, nil
}

// TestRunTurnForwardsTools is the regression guard for the web-UI bug where the
// server's sender only implemented the base Sender, so the agent loop fell back
// to a tool-less Turn and every tool (web_search included) vanished.
func TestRunTurnForwardsTools(t *testing.T) {
	rec := &recordingSender{}
	srv := &Server{
		cfg:       Config{Tools: true},
		model:     "stub-model",
		turnLocks: map[string]*sync.Mutex{},
		sender:    rec,
	}

	sess := agent.NewSession("stub-model", "")
	if _, err := srv.runTurn(context.Background(), sess, "hi"); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	if len(rec.lastTools) == 0 {
		t.Fatal("expected tools forwarded to sender, got none (sender degraded to tool-less Turn)")
	}
	found := false
	for _, d := range rec.lastTools {
		if d.Name == "web_search" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("web_search not among forwarded tools: %v", rec.lastTools)
	}
}
