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

	"github.com/Leihb/octo-agent/internal/provider"
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
		provider:       &stubProvider{},
	}
	srv.registerRoutes()
	// Wrap with CORS middleware so tests that exercise CORS hit the right layer.
	srv.http = &http.Server{Handler: srv.corsMiddleware(srv.mux)}
	return srv
}

// stubProvider implements provider.Provider for tests.
type stubProvider struct{}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Send(_ context.Context, _ provider.Request) (provider.Response, error) {
	return provider.Response{Content: "stub reply"}, nil
}

func (s *stubProvider) SendStream(_ context.Context, _ provider.Request, cb provider.StreamCallbacks) (provider.Response, error) {
	if cb.OnText != nil {
		cb.OnText("stub reply")
	}
	return provider.Response{Content: "stub reply"}, nil
}
