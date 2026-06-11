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
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/upgrade"
)

// serveLoopback dispatches through the given handler as a local browser
// would: loopback peer, local Host — so handler tests exercise their
// endpoint, not the auth middleware. Auth-specific tests craft their own
// RemoteAddr/Host (see auth_test.go).
func serveLoopback(h http.Handler, w http.ResponseWriter, req *http.Request) {
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	h.ServeHTTP(w, req)
}

func TestHandleHealth(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

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
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body sessionListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Sessions == nil {
		t.Fatal("expected non-nil sessions slice")
	}
}

func TestHandleGetSession_NotFound(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/nonexistent", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

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
	serveLoopback(srv.mux, w, req)

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
	serveLoopback(srv.mux, w, req)

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
	serveLoopback(srv.mux, w, req)

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
	serveLoopback(srv.mux, w, req)

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
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestStaticHandler_IndexFallback(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.http.Handler, w, req)

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
	serveLoopback(srv.http.Handler, w, req)

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
	serveLoopback(srv.http.Handler, w, req)

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
	serveLoopback(srv.http.Handler, w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("expected no CORS header for disallowed origin")
	}
}

// mustAccessKey resolves the test server's key without ever persisting —
// generation is in-memory only here; only server.New writes config.yml.
func mustAccessKey(cfg Config) string {
	key, _ := resolveAccessKey(cfg.AccessKey, config.Config{})
	return key
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
		accessKey:      mustAccessKey(cfg),
		sessionAgents:  make(map[string]*agent.Agent),
		steerQueues:    make(map[string][]agent.InboxItem),
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

// ─── New API tests ──────────────────────────────────────────────────────────

func TestHandleOnboardStatus(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodGet, "/api/onboard/status", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["needs_onboard"] == nil {
		t.Fatal("expected needs_onboard field")
	}
}

func TestHandleListProviders(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	providers, ok := body["providers"].([]any)
	if !ok || len(providers) == 0 {
		t.Fatal("expected non-empty providers list")
	}
}

func TestHandleGetConfig(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body configResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Language != "en" {
		t.Fatalf("language = %q, want en", body.Language)
	}
}

func TestHandleTestConfig(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	payload, _ := json.Marshal(testConfigRequest{Model: "gpt-4", BaseURL: "https://api.openai.com/v1"})
	req := httptest.NewRequest(http.MethodPost, "/api/config/test", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Without an API key in env, this returns ok=false because we require a key for validation.
	if body["ok"] != false {
		t.Fatalf("ok = %v, want false", body["ok"])
	}
}

func TestHandleSaveModelConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	payload, _ := json.Marshal(saveModelRequest{Type: "default", Model: "gpt-4", BaseURL: "https://api.openai.com/v1", APIKey: "sk-test"})
	req := httptest.NewRequest(http.MethodPost, "/api/config/models", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true", body["ok"])
	}
}

func TestHandleToggleSkill(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	// Create a real skill so the toggle has something to act on.
	skillDir := filepath.Join(tmp, ".octo", "skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: test\ndescription: a test skill\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	// Replace the skillReg with one that sees our temp skill.
	srv.skillReg = skills.Discover("")
	srv.skillsManifest = skills.RenderManifest(srv.skillReg)

	// Toggle off (disable).
	req := httptest.NewRequest(http.MethodPatch, "/api/skills/test-skill/toggle", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != false {
		t.Fatalf("enabled = %v, want false", body["enabled"])
	}

	// Verify config was persisted.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tools.DisabledSkills) != 1 || cfg.Tools.DisabledSkills[0] != "test-skill" {
		t.Fatalf("disabled skills = %v, want [test-skill]", cfg.Tools.DisabledSkills)
	}

	// Toggle back on (enable).
	req = httptest.NewRequest(http.MethodPatch, "/api/skills/test-skill/toggle", nil)
	w = httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != true {
		t.Fatalf("enabled = %v, want true", body["enabled"])
	}

	cfg, _ = config.Load()
	if len(cfg.Tools.DisabledSkills) != 0 {
		t.Fatalf("disabled skills = %v, want empty", cfg.Tools.DisabledSkills)
	}
}

func TestHandleToggleSkill_NotFound(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodPatch, "/api/skills/nonexistent/toggle", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestHandleListSkills_RescansDisk is the regression guard for the stale
// skill panel: the registry is discovered once at server start, so without a
// per-request re-scan a skill directory deleted (or added) on disk would stay
// wrong in the panel until the server restarts.
func TestHandleListSkills_RescansDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	hasSkill := func(name string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/skills", nil)
		w := httptest.NewRecorder()
		serveLoopback(srv.mux, w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var body struct {
			Skills []struct {
				Name string `json:"name"`
			} `json:"skills"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, sk := range body.Skills {
			if sk.Name == name {
				return true
			}
		}
		return false
	}

	// A skill dropped on disk AFTER server start must show up without restart.
	skillDir := filepath.Join(tmp, ".octo", "skills", "ghost")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: ghost\ndescription: appears and disappears\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasSkill("ghost") {
		t.Fatal("skill added on disk after start not listed; list handler did not re-scan")
	}

	// A skill deleted on disk must vanish from the next list response.
	if err := os.RemoveAll(skillDir); err != nil {
		t.Fatal(err)
	}
	if hasSkill("ghost") {
		t.Fatal("skill deleted on disk still listed; panel would show a ghost entry until restart")
	}
}

func TestHandleBenchmark_Success(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test-session/benchmark", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body benchmarkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK {
		t.Fatalf("ok = false, want true")
	}
	if len(body.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(body.Results))
	}
	res := body.Results[0]
	if !res.OK {
		t.Fatalf("result.ok = false, want true")
	}
	if res.ModelID != "stub-model" {
		t.Fatalf("model_id = %q, want stub-model", res.ModelID)
	}
	// stubSender fires the chunk immediately, so TTFT should be ≥ 0 and very small.
	if res.TTFTMs < 0 {
		t.Fatalf("ttft_ms = %d, want >= 0", res.TTFTMs)
	}
}

func TestHandleGetMemory_NotFound(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodGet, "/api/memories/nonexistent.md", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleVersionUpgrade_AcceptedAndConflict(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	// While an upgrade runs, a second POST conflicts.
	srv.upgradeRunning = true
	req := httptest.NewRequest(http.MethodPost, "/api/version/upgrade", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while running", w.Code)
	}
	srv.upgradeRunning = false

	// A fresh POST is accepted; the background run refuses internally (the
	// test binary fails the eligibility check) and resets the single-flight
	// latch — poll for that so the goroutine's lifecycle is covered too.
	// Broadcasts go through the nil-safe helper (no WS hub in mustServer).
	// Belt-and-braces: pin the release origin to an unroutable URL so even
	// an eligibility-rule change can't make this test dial out.
	origURL := upgrade.BaseURL
	upgrade.BaseURL = "http://127.0.0.1:0"
	t.Cleanup(func() { upgrade.BaseURL = origURL })
	req = httptest.NewRequest(http.MethodPost, "/api/version/upgrade", nil)
	w = httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.upgradeMu.Lock()
		running := srv.upgradeRunning
		srv.upgradeMu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upgradeRunning never reset after background run")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleVersion_NoUpdateCheck(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"] == "" || body["current"] == "" {
		t.Errorf("version/current missing: %v", body)
	}
	// UpdateCheck off (the default for every test-constructed server):
	// degrade to current==latest, no outbound call.
	if body["latest"] != body["current"] {
		t.Errorf("latest = %v, want current %v without UpdateCheck", body["latest"], body["current"])
	}
	if body["needs_update"] != false {
		t.Errorf("needs_update = %v, want false", body["needs_update"])
	}
	if body["cli_command"] != "octo" {
		t.Errorf("cli_command = %v, want the constant octo", body["cli_command"])
	}
}

func TestHandleUpdateSessionReasoningEffort(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	payload, _ := json.Marshal(updateSessionReasoningEffortRequest{ReasoningEffort: "high"})
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+sess.ID+"/reasoning_effort", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true", body["ok"])
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DefaultEntry().ReasoningEffort; got != "high" {
		t.Fatalf("reasoning_effort = %q, want high", got)
	}
}

func TestHandleUpdateSessionWorkingDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	payload, _ := json.Marshal(updateSessionWorkingDirRequest{WorkingDir: tmp})
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+sess.ID+"/working_dir", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true", body["ok"])
	}
	if srv.cwd != tmp {
		t.Fatalf("server cwd = %q, want %q", srv.cwd, tmp)
	}
}

func TestHandleUpdateSessionModel_RawStringIsPerSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	payload, _ := json.Marshal(updateSessionModelRequest{ModelID: "kimi-for-coding"})
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+sess.ID+"/model", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true", body["ok"])
	}

	// The switch is per-session: the session carries the new model, the
	// server default and the persisted config stay untouched.
	got, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "kimi-for-coding" || got.ModelConfig != "" {
		t.Fatalf("session = (%q, %q), want (kimi-for-coding, \"\")", got.Model, got.ModelConfig)
	}
	if srv.model != "stub-model" {
		t.Fatalf("server default model changed to %q — must stay stub-model", srv.model)
	}
	cfg, _ := config.Load()
	if len(cfg.Models) != 0 {
		t.Fatalf("global config written by a per-session switch: %+v", cfg.Models)
	}
}

func TestHandleUpdateSessionModel_EntryNameBindsSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	seed := config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6"},
			{Name: "kimi", Provider: "kimi", Model: "kimi-k2.6", APIKey: "sk-kimi"},
		},
		DefaultModel: "main",
	}
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	sess := agent.NewSession("claude-sonnet-4-6", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	payload, _ := json.Marshal(updateSessionModelRequest{ModelID: "kimi"})
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+sess.ID+"/model", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	got, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelConfig != "kimi" || got.Model != "kimi-k2.6" {
		t.Fatalf("session = (%q, %q), want (kimi-k2.6, kimi)", got.Model, got.ModelConfig)
	}
	cfg, _ := config.Load()
	if cfg.DefaultModel != "main" {
		t.Fatalf("global default changed to %q — must stay main", cfg.DefaultModel)
	}

	// The bound session's turns resolve to the entry's sender and model;
	// an unbound session stays on the server default.
	sender, model := srv.senderForSession(got)
	if model != "kimi-k2.6" {
		t.Errorf("senderForSession model = %q, want kimi-k2.6", model)
	}
	if sender == srv.sender {
		t.Error("bound session must not ride the default sender")
	}
	plain := agent.NewSession("stub-model", "")
	if sender2, model2 := srv.senderForSession(plain); sender2 != srv.sender || model2 != "stub-model" {
		t.Errorf("unbound session = (%v, %q), want default sender + stub-model", sender2, model2)
	}

	// Deleting the bound entry degrades to the default sender instead of
	// failing the turn.
	if w := doJSON(t, srv, http.MethodDelete, "/api/config/models/kimi", ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	if sender3, _ := srv.senderForSession(got); sender3 != srv.sender {
		t.Error("stale binding must fall back to the default sender")
	}
}

// TestHandleGetSessionMessages_IncludesToolCalls verifies that assistant
// messages carrying tool_use blocks are translated into tool_call events in
// the history response.
func TestHandleGetSessionMessages_IncludesToolCalls(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	sess.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "list files"},
		agent.NewToolUseMessage([]agent.ContentBlock{
			agent.NewToolUseBlock("call_1", "terminal", map[string]any{"command": "ls"}),
		}),
		agent.NewToolResultMessage([]agent.ContentBlock{
			agent.NewToolResultBlock("call_1", "file.txt\n", false),
		}),
		agent.NewAssistantMessage("Here are the files: file.txt"),
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var body struct {
		HasMore bool             `json:"has_more"`
		Events  []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	var toolCallCount int
	for _, ev := range body.Events {
		if ev["type"] == "tool_call" {
			toolCallCount++
		}
	}
	if toolCallCount != 1 {
		t.Fatalf("tool_call events = %d, want 1; events=%v", toolCallCount, body.Events)
	}
}

// History replay must never render <system-reminder> spans (background
// completion notes, recalled memories) as user bubbles — the TUI skips them
// and the web transcript must match.
func TestHandleGetSessionMessages_StripsSystemReminders(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	sess.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "run the build in the background"},
		agent.NewAssistantMessage("Started."),
		// A background-completion note injected via steer: pure reminder, no
		// user speech. Must produce NO history_user_message.
		{Role: agent.RoleUser, Content: "<system-reminder>\n[BACKGROUND COMPLETED]\nBackground process bg_1 (`make build`) exited: 0.\nOutput since last check:\nhuge build log…\n</system-reminder>"},
		agent.NewAssistantMessage("Build finished."),
		// Mixed turn: reminder prepended to real user text. Only the user
		// text survives.
		{Role: agent.RoleUser, Content: "<system-reminder>recalled memory</system-reminder>\nnow run the tests"},
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	var userTexts []string
	for _, ev := range body.Events {
		if ev["type"] == "history_user_message" {
			userTexts = append(userTexts, ev["content"].(string))
		}
	}
	want := []string{"run the build in the background", "now run the tests"}
	if len(userTexts) != len(want) {
		t.Fatalf("user messages = %q, want %q", userTexts, want)
	}
	for i := range want {
		if userTexts[i] != want[i] {
			t.Errorf("user message[%d] = %q, want %q", i, userTexts[i], want[i])
		}
	}
}

// TestHandleGetSessionMessages_MultiToolUse verifies the full event sequence
// for a session with multiple tool_use blocks and tool_results.
func TestHandleGetSessionMessages_MultiToolUse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	// res1 carries a structured UI payload; res2 doesn't. The replay must
	// surface the payload as ui_payload after the session JSON round-trip.
	res1 := agent.NewToolResultBlock("call_1", "file.txt\n", false)
	res1.UI = map[string]any{"type": "file_list", "total": 1}

	sess := agent.NewSession("stub-model", "")
	sess.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "list and grep"},
		agent.NewToolUseMessage([]agent.ContentBlock{
			agent.NewToolUseBlock("call_1", "terminal", map[string]any{"command": "ls"}),
			agent.NewToolUseBlock("call_2", "grep", map[string]any{"pattern": "foo"}),
		}),
		agent.NewToolResultMessage([]agent.ContentBlock{
			res1,
			agent.NewToolResultBlock("call_2", "file.txt:1:foo\n", false),
		}),
		agent.NewAssistantMessage("Found it"),
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var body struct {
		HasMore bool             `json:"has_more"`
		Events  []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	var gotTypes []string
	for _, ev := range body.Events {
		gotTypes = append(gotTypes, ev["type"].(string))
	}

	wantTypes := []string{
		"history_user_message",
		"tool_call",
		"tool_call",
		"assistant_message",
		"tool_result",
		"tool_result",
		"assistant_message",
	}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Fatalf("event[%d].type = %q, want %q; full=%v", i, gotTypes[i], want, gotTypes)
		}
	}

	// events[4] / events[5] are the two tool_results: only the first has UI.
	ui, ok := body.Events[4]["ui_payload"].(map[string]any)
	if !ok || ui["type"] != "file_list" {
		t.Errorf("events[4].ui_payload = %#v, want file_list payload", body.Events[4]["ui_payload"])
	}
	if _, present := body.Events[5]["ui_payload"]; present {
		t.Errorf("events[5].ui_payload present, want absent: %#v", body.Events[5]["ui_payload"])
	}
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

// TestDoAgentTurn_SeedsThinkingProgress is the regression guard for the web-UI
// "no spinner after send" gap. doAgentTurn must broadcast an initial "thinking"
// progress immediately — before any text streams — so the frontend keeps an
// indicator on screen between swapping the ghost bubble for the real one and
// the first delta. Provider connect + pre-text reasoning fire no events, so
// without the seed the session looks hung.
func TestDoAgentTurn_SeedsThinkingProgress(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.initWS()
	// Maps doAgentTurn touches that the full constructor sets up but mustServer
	// (a minimal hand-built Server) does not.
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]agent.InboxItem)
	srv.sessionAgents = make(map[string]*agent.Agent)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Subscribe a fake connection so we capture what the turn broadcasts.
	conn := &wsConn{
		hub:        srv.wsHub,
		send:       make(chan []byte, 256),
		subscribed: map[string]struct{}{},
	}
	srv.wsHub.register <- conn
	srv.wsHub.subscribe(conn, sess.ID)

	srv.doAgentTurn(sess, "hi", nil, nil)

	// Broadcast is async through the hub's event loop; drain until the turn's
	// terminal "complete" event (or a safety deadline).
	var types []string
	firstProgressIdx, firstTextIdx := -1, -1
	progressType := ""
	deadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case b := <-conn.send:
			var ev map[string]any
			if err := json.Unmarshal(b, &ev); err != nil {
				continue
			}
			typ, _ := ev["type"].(string)
			switch typ {
			case "progress":
				if firstProgressIdx < 0 {
					firstProgressIdx = len(types)
					progressType, _ = ev["progress_type"].(string)
				}
			case "text_delta":
				if firstTextIdx < 0 {
					firstTextIdx = len(types)
				}
			}
			types = append(types, typ)
			if typ == "complete" {
				break drain
			}
		case <-deadline:
			break drain
		}
	}

	if firstProgressIdx < 0 {
		t.Fatalf("turn broadcast no progress event; got %v", types)
	}
	if progressType != "thinking" {
		t.Errorf("first progress_type = %q, want \"thinking\"; events=%v", progressType, types)
	}
	if firstTextIdx >= 0 && firstProgressIdx > firstTextIdx {
		t.Errorf("thinking progress must precede first text_delta; progress@%d text@%d; events=%v",
			firstProgressIdx, firstTextIdx, types)
	}
}

// TestDoAgentTurn_LiveAndHistoryCreatedAtMatch guards the Web UI's
// live-vs-history dedup key. On session open, the initial /messages fetch can
// race the live history_user_message broadcast; the frontend dedups the two
// render paths by exact created_at equality, so the broadcast and the history
// endpoint must report the SAME millisecond value for the same user message.
// A mismatch (one side seconds, the other milliseconds, or two separate
// time.Now() calls) renders the user's message twice until a page refresh.
func TestDoAgentTurn_LiveAndHistoryCreatedAtMatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.initWS()
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]agent.InboxItem)
	srv.sessionAgents = make(map[string]*agent.Agent)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	conn := &wsConn{
		hub:        srv.wsHub,
		send:       make(chan []byte, 256),
		subscribed: map[string]struct{}{},
	}
	srv.wsHub.register <- conn
	srv.wsHub.subscribe(conn, sess.ID)

	srv.doAgentTurn(sess, "hello dedup", nil, nil)

	// Capture the live broadcast's created_at.
	var liveCreatedAt float64
	deadline := time.After(2 * time.Second)
drainDedup:
	for {
		select {
		case b := <-conn.send:
			var ev map[string]any
			if err := json.Unmarshal(b, &ev); err != nil {
				continue
			}
			if typ, _ := ev["type"].(string); typ == "history_user_message" {
				liveCreatedAt, _ = ev["created_at"].(float64)
			} else if typ == "complete" {
				break drainDedup
			}
		case <-deadline:
			break drainDedup
		}
	}
	if liveCreatedAt <= 0 {
		t.Fatal("no live history_user_message with created_at captured")
	}

	// Fetch history and find the same user message.
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	w := httptest.NewRecorder()
	serveLoopback(srv.mux, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("messages status = %d; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var histCreatedAt float64
	for _, ev := range body.Events {
		if typ, _ := ev["type"].(string); typ == "history_user_message" {
			if c, _ := ev["content"].(string); c == "hello dedup" {
				histCreatedAt, _ = ev["created_at"].(float64)
			}
		}
	}
	if histCreatedAt <= 0 {
		t.Fatal("user message not found in history endpoint response")
	}
	if liveCreatedAt != histCreatedAt {
		t.Fatalf("dedup key mismatch: live created_at = %v, history created_at = %v — frontend would render the message twice",
			liveCreatedAt, histCreatedAt)
	}
}

// TestWSStreamWriter_ReseedsThinkingAfterTool is the regression guard for the
// web-UI "tools pop out of dead air" gap. The frontend clears the progress
// indicator when a tool_call renders, and the next LLM round emits nothing
// until its first delta — so after every tool_result (and recoverable
// tool_error) the stream writer must broadcast a fresh "thinking" progress
// and keep it in live state for late-subscriber replay.
func TestWSStreamWriter_ReseedsThinkingAfterTool(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	srv.initWS()

	const sid = "sess-reseed"
	srv.liveStateMu.Lock()
	srv.liveStates[sid] = &sessionLiveState{}
	srv.liveStateMu.Unlock()

	conn := &wsConn{
		hub:        srv.wsHub,
		send:       make(chan []byte, 256),
		subscribed: map[string]struct{}{},
	}
	srv.wsHub.register <- conn
	srv.wsHub.subscribe(conn, sid)

	sw := srv.newWSStreamWriter(sid)

	// drainUntilProgress reads broadcasts until a progress event (or deadline)
	// and returns it along with every event type seen on the way.
	drainUntilProgress := func() (map[string]any, []string) {
		deadline := time.After(2 * time.Second)
		var types []string
		for {
			select {
			case b := <-conn.send:
				var ev map[string]any
				if err := json.Unmarshal(b, &ev); err != nil {
					continue
				}
				typ, _ := ev["type"].(string)
				types = append(types, typ)
				if typ == "progress" {
					return ev, types
				}
			case <-deadline:
				return nil, types
			}
		}
	}

	for _, tc := range []struct {
		name  string
		event agent.AgentEvent
	}{
		{"tool_done", agent.AgentEvent{Kind: agent.EventToolDone, ToolName: "terminal", Output: "ok"}},
		{"tool_error", agent.AgentEvent{Kind: agent.EventToolError, ToolName: "terminal", Err: "exit 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw.handleEvent(tc.event)

			ev, types := drainUntilProgress()
			if ev == nil {
				t.Fatalf("no progress broadcast after %s; got %v", tc.name, types)
			}
			if pt, _ := ev["progress_type"].(string); pt != "thinking" {
				t.Errorf("progress_type = %q, want \"thinking\"", pt)
			}
			if ph, _ := ev["phase"].(string); ph != "active" {
				t.Errorf("phase = %q, want \"active\"", ph)
			}
			if at, _ := ev["started_at"].(float64); at <= 0 {
				t.Errorf("started_at = %v, want > 0", ev["started_at"])
			}

			// Live state must carry the reseeded progress so a tab subscribing
			// mid-gap replays an indicator instead of a blank screen.
			srv.liveStateMu.Lock()
			ls := srv.liveStates[sid]
			srv.liveStateMu.Unlock()
			if ls == nil {
				t.Fatalf("live state deleted after %s; replay would be blank", tc.name)
			}
			if ls.progress == nil || ls.progress.ProgressType != "thinking" {
				t.Errorf("live state progress = %+v, want reseeded thinking phase", ls.progress)
			}
		})
	}
}

// PATCH /api/sessions/{id} is the sidebar rename action — the title must
// persist through a session reload, and bad input must not slip through.
func TestHandleUpdateSession_Rename(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := agent.NewSession("stub-model", "")
	sess.Messages = []agent.Message{{Role: agent.RoleUser, Content: "hi"}}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	do := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		serveLoopback(srv.mux, w, req)
		return w
	}

	if w := do(sess.ID, `{"name":"My research session"}`); w.Code != http.StatusOK {
		t.Fatalf("rename status = %d; body=%s", w.Code, w.Body.String())
	}
	loaded, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Title != "My research session" {
		t.Errorf("Title = %q, want the new name", loaded.Title)
	}

	if w := do(sess.ID, `{"name":"  "}`); w.Code != http.StatusBadRequest {
		t.Errorf("blank name status = %d, want 400", w.Code)
	}
	if w := do("no-such-session", `{"name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", w.Code)
	}
}
