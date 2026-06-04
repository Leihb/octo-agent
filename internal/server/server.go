// Package server provides the HTTP server for octo's REST API and Web UI.
//
// It is the M8 milestone implementation: a stateless HTTP layer over the
// existing agent/session/tool infrastructure. Each request carries its own
// context; sessions are loaded from / saved to disk on every turn.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
)

// Config holds server-level settings.
type Config struct {
	// Bind address (e.g. ":8080", "127.0.0.1:8080").
	Addr string

	// Provider and model override; zero values fall back to env/config.
	Provider string
	Model    string
	System   string

	// MaxTokens forwarded to the agent per turn.
	MaxTokens int

	// Tools enables the agentic tool loop.
	Tools bool

	// CORSOrigins lists allowed origins for CORS. Empty disables CORS.
	CORSOrigins []string
}

// Server is the HTTP server skeleton. It owns the mux, the agent factory,
// and the in-memory turn lock (one turn per session at a time).
type Server struct {
	cfg  Config
	mux  *http.ServeMux
	http *http.Server

	// agent factory state (resolved once at start)
	sender         agent.Sender
	model          string
	system         string
	skillReg       *skills.Registry
	skillsManifest string
	cwd            string
	envCtx         string

	// session-scoped turn locks: one turn per session at a time.
	turnLocks map[string]*sync.Mutex
	turnMu    sync.Mutex
}

// New builds a Server. It resolves provider/model, discovers skills, and
// validates the bind address against security rules.
func New(cfg Config) (*Server, error) {
	if err := validateBindAddr(cfg.Addr); err != nil {
		return nil, err
	}

	sender, model, err := resolveProviderAndModel(cfg.Provider, cfg.Model)
	if err != nil {
		return nil, err
	}

	cwd, _ := os.Getwd()
	envCtx := buildEnvContext(cwd)

	skillReg := skills.Discover(cwd)
	skillsManifest := skills.RenderManifest(skillReg)
	tools.SetSkills(skillReg)

	s := &Server{
		cfg:            cfg,
		mux:            http.NewServeMux(),
		sender:         sender,
		model:          model,
		system:         cfg.System,
		skillReg:       skillReg,
		skillsManifest: skillsManifest,
		cwd:            cwd,
		envCtx:         envCtx,
		turnLocks:      map[string]*sync.Mutex{},
	}

	s.registerRoutes()
	s.enableSubAgentTools()

	s.http = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.corsMiddleware(s.mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	return s, nil
}

// enableSubAgentTools registers the process-global sub-agent manager + task
// store so DefaultToolsFor advertises launch_agent / send_message / task_* . On
// the server these globals are gating sentinels and a never-hit fallback: every
// turn stamps its own ctx-scoped manager + store (bound to that turn's agent)
// via prepareToolTurn, and dispatch prefers the ctx-scoped ones — so concurrent
// sessions never share sub-agent or task state. The template agent only seeds
// the sentinel spawner. No-op when tools are disabled.
func (s *Server) enableSubAgentTools() {
	if !s.cfg.Tools {
		return
	}
	template := agent.New(s.sender, s.model)
	template.System = prompt.Compose(s.system, s.cwd, s.envCtx, s.skillsManifest, "", true)
	executor := tools.NewDefaultRegistry()
	spawner := app.NewSpawner(template, executor, func() []agent.ToolDefinition {
		return tools.DefaultToolsFor(s.model)
	})
	mgr := tools.NewSubAgentManager(spawner)
	mgr.SetSynchronous(true)
	tools.SetDefaultSubAgentManager(mgr)
	tools.SetTaskStore(tasks.New())
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

// Shutdown gracefully shuts down the server, releasing the process-global
// sub-agent + task registrations installed by enableSubAgentTools.
func (s *Server) Shutdown(ctx context.Context) error {
	tools.SetDefaultSubAgentManager(nil)
	tools.SetTaskStore(nil)
	return s.http.Shutdown(ctx)
}

// registerRoutes wires all handlers.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /api/chat", s.handleCreateChat)
	s.mux.HandleFunc("POST /api/chat/{id}/turn", s.handleTurnOrSSE)
	s.mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	s.mux.HandleFunc("POST /api/sessions/delete", s.handleDeleteSessions)
	s.mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	s.mux.HandleFunc("GET /api/tools", s.handleListTools)
	s.mux.HandleFunc("GET /api/skills", s.handleListSkills)
	s.mux.HandleFunc("GET /api/health", s.handleHealth)

	// Static files (Web UI) — served from embedded filesystem.
	s.mux.Handle("/", s.staticHandler())
}

// corsMiddleware wraps the mux with CORS headers when configured.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	if len(s.cfg.CORSOrigins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := false
		for _, o := range s.cfg.CORSOrigins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// sessionTurnLock returns the mutex for a given session ID, creating it if
// needed. This serialises turns per session so concurrent POSTs to the same
// session don't interleave.
func (s *Server) sessionTurnLock(id string) *sync.Mutex {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if mu, ok := s.turnLocks[id]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	s.turnLocks[id] = mu
	return mu
}

// forgetTurnLock drops a session's turn mutex after the session is deleted, so
// the map doesn't accumulate locks for sessions that no longer exist.
func (s *Server) forgetTurnLock(id string) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	delete(s.turnLocks, id)
}

// buildAgent creates a fresh agent for a turn. The caller must have locked
// the session's turn mutex.
func (s *Server) buildAgent(sess *agent.Session) *agent.Agent {
	a := agent.New(s.sender, s.model)
	a.CWD = s.cwd
	a.MaxTokens = s.cfg.MaxTokens
	a.System = prompt.Compose(s.system, s.cwd, s.envCtx, s.skillsManifest, "", true)
	if sess.Model != "" {
		a.Model = sess.Model
	}
	if len(sess.Messages) > 0 {
		a.History = sess.ToHistory()
	}
	return a
}

// writeJSON is a convenience helper for JSON responses.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a structured error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readBodyJSON reads and unmarshals the request body.
func readBodyJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ─── Provider resolution ────────────────────────────────────────────────────
//
// The server resolves provider/model/key/base-URL exactly as before, then hands
// the result to app.NewSender — internal/app is the single place that builds the
// vendor client, so the server no longer imports internal/provider.

func resolveProviderAndModel(flagProvider, flagModel string) (agent.Sender, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", fmt.Errorf("load config: %w", err)
	}

	provName := firstNonEmpty(flagProvider, os.Getenv("OCTO_PROVIDER"), cfg.Provider, "anthropic")
	model := flagModel
	if model == "" {
		model = modelFromEnv(provName)
	}
	if model == "" && provName == cfg.Provider {
		model = cfg.Model
	}
	if model == "" {
		model = defaultModelFor(provName)
	}
	if model == "" {
		return nil, "", fmt.Errorf("unknown provider %q", provName)
	}

	apiKey, err := resolveAPIKey(provName, cfg)
	if err != nil {
		return nil, "", err
	}
	sender, err := app.NewSender(app.SenderOptions{
		Provider: provName,
		APIKey:   apiKey,
		BaseURL:  resolveBaseURL(provName, cfg),
	})
	if err != nil {
		return nil, "", err
	}
	return sender, model, nil
}

// resolveAPIKey returns the API key for the vendor: the provider's env var,
// else cfg.APIKey when the stored config targets the same provider. Errors with
// the same "X is not set" message the server reported before.
func resolveAPIKey(name string, cfg config.Config) (string, error) {
	switch name {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" && cfg.Provider == "anthropic" {
			apiKey = cfg.APIKey
		}
		if apiKey == "" {
			return "", fmt.Errorf("ANTHROPIC_API_KEY is not set")
		}
		return apiKey, nil
	case "openai":
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" && cfg.Provider == "openai" {
			apiKey = cfg.APIKey
		}
		if apiKey == "" {
			return "", fmt.Errorf("OPENAI_API_KEY is not set")
		}
		return apiKey, nil
	default:
		return "", fmt.Errorf("unknown provider %q", name)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func modelFromEnv(provider string) string {
	switch provider {
	case "anthropic":
		return os.Getenv("ANTHROPIC_MODEL")
	case "openai":
		return os.Getenv("OPENAI_MODEL")
	}
	return ""
}

func defaultModelFor(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-haiku-4-5-20251001"
	case "openai":
		return "gpt-4o-mini"
	}
	return ""
}

func resolveBaseURL(provider string, cfg config.Config) string {
	if cfg.Provider == provider {
		return cfg.BaseURL
	}
	return ""
}

// buildEnvContext mirrors cmd/octo's env context builder.
func buildEnvContext(cwd string) string {
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	if cwd != "" {
		fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
	}
	fmt.Fprintf(&b, "- Today's date: %s\n", time.Now().Format("2006-01-02"))
	return b.String()
}

// validateBindAddr enforces the M6.5 security rule: non-localhost binds
// require a permissions.yml file to exist.
func validateBindAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Try adding a default port for bare IPs.
		host, _, err = net.SplitHostPort(addr + ":8080")
		if err != nil {
			return fmt.Errorf("invalid bind address %q", addr)
		}
	}

	// localhost, 127.0.0.1, ::1, and empty (all interfaces shorthand) are
	// allowed without a permissions file. Any other host requires one.
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot verify permissions.yml: %w", err)
	}
	permPath := filepath.Join(home, ".octo", "permissions.yml")
	if _, err := os.Stat(permPath); os.IsNotExist(err) {
		return fmt.Errorf("bind address %q is not localhost: create %s to enable non-localhost binds", addr, permPath)
	}
	return nil
}
