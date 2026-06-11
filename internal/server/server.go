// Package server provides the HTTP server for octo's REST API and Web UI.
//
// It is the M8 milestone implementation: a stateless HTTP layer over the
// existing agent/session/tool infrastructure. Each request carries its own
// context; sessions are loaded from / saved to disk on every turn.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/channel"

	// The IM adapters self-register into the channel registry at init time.
	// The server is what runs them (startChannels) and looks them up by name
	// (channel.Find for task-notify pushes), so it owns the imports.
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/dingtalk"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/discord"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/feishu"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/telegram"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/wecom"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/weixin"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/scheduler"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
)

// getDefaultToolsFor bridges to tools.DefaultToolsFor for ws_handlers.go.
func getDefaultToolsFor(model string) []agent.ToolDefinition {
	return tools.DefaultToolsFor(model)
}

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

	// NoChannel disables IM channel (DingTalk, Feishu, etc.) startup.
	// When false (default), channels are started from ~/.octo/channels.yml
	// alongside the HTTP server.
	NoChannel bool

	// AccessKey is the shared secret used to authenticate Web UI and API
	// requests. When empty, the server looks for OCTO_ACCESS_KEY env var.
	// If still empty, a random key is generated and printed on startup.
	AccessKey string

	// NoMemory disables cross-session memory injection.
	NoMemory bool
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
	memDir         string
	homeMemDir     string

	// session-scoped turn locks: one turn per session at a time.
	turnLocks map[string]*sync.Mutex
	turnMu    sync.Mutex

	// session-scoped memory injectors: one injector per session so triggered
	// rules are recalled at most once per session. Deleted alongside turnLocks.
	sessionInjectors map[string]*memory.Injector
	injectorMu       sync.Mutex

	// turnRunning tracks which sessions have an active turn goroutine.
	// Guarded by the session's turnLocks mutex.
	turnRunning map[string]bool

	// steerQueues holds mid-turn user messages (steer) that arrive while a
	// turn is in flight.  Consumed by the turn loop after each iteration.
	steerQueues map[string][]agent.InboxItem
	steerMu     sync.Mutex

	// sessionAgents tracks the currently-running Agent per session so that
	// mid-turn steer messages can be injected directly into its Inbox.
	sessionAgents   map[string]*agent.Agent
	sessionAgentsMu sync.Mutex

	// senderCache holds one sender per config entry name, built lazily for
	// sessions bound to a non-default model entry. Invalidated wholesale on
	// any /api/config/models mutation; the next turn rebuilds.
	senderCache   map[string]agent.Sender
	senderCacheMu sync.Mutex

	// titlePending guards one in-flight title generation per session
	// (lazily initialised by claimTitleGeneration).
	titlePending map[string]bool
	titleMu      sync.Mutex

	// WebSocket hub for real-time browser communication.
	wsHub *wsHub

	// interrupt cancellation per session.
	interrupts  map[string]context.CancelFunc
	interruptMu sync.Mutex

	// confirmation channels (from request_user_feedback in browser).
	confirmations map[string]chan string
	confirmMu     sync.Mutex

	// question channels (from request_user_question in browser).
	questionChans map[string]chan tools.AskResponse
	questionMu    sync.Mutex

	// pending interactive prompts per session, replayed on WS (re)subscribe so
	// a page refresh doesn't orphan an in-flight question or confirmation —
	// the original broadcast only reached the tabs connected at the time.
	pendingQuestions map[string]wsEventRequestUserQuestion
	pendingConfirms  map[string]wsEventRequestConfirmation
	pendingPromptMu  sync.Mutex

	// live state tracking per session for WS replay on subscribe.
	liveStates  map[string]*sessionLiveState
	liveStateMu sync.RWMutex

	// scheduler for cron-based background tasks. schedulerMu serialises its
	// lazy initialisation (handlers and server start may race).
	scheduler   *scheduler.Scheduler
	schedulerMu sync.Mutex

	// channel manager for IM platform bridging (DingTalk, Feishu, etc.).
	// nil when NoChannel is set or no channels are configured.
	channelCfg      *channel.Config
	channelMgr      *channel.Manager
	channelCancel   context.CancelFunc
	runningAdapters sync.Map // string -> channel.Adapter

	// weixinLogin tracks the in-flight web QR login flow (one at a time).
	weixinLogin weixinLoginFlow

	// mcpCleanup unregisters + closes the MCP registry connected at start.
	// Always non-nil after New (a no-op when no servers connected).
	mcpCleanup func()

	// mcpMu serialises MCP config mutations + registry swaps from the web
	// management API (and Shutdown) — SwapMCP must not run concurrently
	// with itself or with incremental connects. Also guards mcpOAuthFlows.
	mcpMu sync.Mutex

	// mcpOAuthFlows tracks in-flight web device-flow authorizations by
	// server name. Lazily initialised by the oauth/start handler.
	mcpOAuthFlows map[string]*mcpOAuthFlow

	// accessKey is the shared secret for Web UI / API authentication.
	accessKey string

	// restartPending is set by Restart and read after the listener closes to
	// distinguish a restart-driven shutdown (exit ExitRestart) from a plain
	// one (exit 0).
	restartPending atomic.Bool

	// shutdownMu/shutdownDone/shutdownErr make Shutdown single-flight: the
	// first caller runs it, later callers wait on shutdownDone and read
	// shutdownErr. All zero-valued until the first Shutdown call.
	shutdownMu   sync.Mutex
	shutdownDone chan struct{}
	shutdownErr  error

	// drain gates every turn execution path during a restart. drainTimeout
	// overrides the 30s default in tests; zero means the default.
	drain        drainGate
	drainTimeout time.Duration

	// senderMu serialises lazy initialisation of sender when the server starts
	// in onboarding mode (API key missing) and the user completes setup later.
	senderMu sync.Mutex
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
	fileCfg, _ := config.Load()
	skillReg.SetDisabled(fileCfg.Tools.DisabledSkills)
	skillsManifest := skills.RenderManifest(skillReg)
	tools.SetSkills(skillReg)

	accessKey := resolveAccessKey(cfg.AccessKey, fileCfg)

	// Resolve cross-session memory directory.
	var memDir, homeMemDir string
	if !cfg.NoMemory {
		if d, err := memory.Dir(memory.ProjectRoot(cwd)); err == nil {
			if memory.EnsureDir(d) == nil {
				memDir = d
			}
		}
		if d, err := memory.HomeDir(); err == nil {
			if memory.EnsureDir(d) == nil {
				homeMemDir = d
			}
		}
	}

	s := &Server{
		cfg:              cfg,
		mux:              http.NewServeMux(),
		sender:           sender,
		model:            model,
		system:           cfg.System,
		skillReg:         skillReg,
		skillsManifest:   skillsManifest,
		cwd:              cwd,
		envCtx:           envCtx,
		memDir:           memDir,
		homeMemDir:       homeMemDir,
		turnLocks:        map[string]*sync.Mutex{},
		turnRunning:      make(map[string]bool),
		steerQueues:      make(map[string][]agent.InboxItem),
		sessionAgents:    make(map[string]*agent.Agent),
		accessKey:        accessKey,
		confirmations:    make(map[string]chan string),
		questionChans:    make(map[string]chan tools.AskResponse),
		pendingQuestions: make(map[string]wsEventRequestUserQuestion),
		pendingConfirms:  make(map[string]wsEventRequestConfirmation),
		sessionInjectors: make(map[string]*memory.Injector),
	}

	// Register the WebSocket-backed asker so ask_user_question appears in the
	// tool catalog and can be dispatched through the browser.
	tools.SetAsker(s.wsAsker())

	// Register the restarter so restart_server appears in the tool catalog —
	// the agent can then honour "重启一下服务" from web or IM. Permission is
	// ask-class (defaults.yml): the web UI confirms via the browser prompt,
	// IM via the in-chat reply prompt. The restart drains in-flight turns
	// (including the one calling the tool) before the worker exits with
	// ExitRestart for the supervisor to respawn.
	tools.SetRestarter(s.Restart)

	s.registerRoutes()
	s.initWS()
	if sender != nil {
		s.enableSubAgentTools()
	}
	s.enableMCP()
	s.initChannels()

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
// store so DefaultToolsFor advertises sub_agent / task_* . On
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
	var memInjection string
	if s.memDir != "" {
		memInjection = memory.RenderInjection(s.memDir, s.homeMemDir)
	}
	template.System = prompt.Compose(s.system, s.cwd, s.envCtx, s.skillsManifest, memInjection, true)
	executor := tools.NewDefaultRegistry()
	spawner := app.NewSpawner(template, executor, func() []agent.ToolDefinition {
		return tools.DefaultToolsFor(s.model)
	})
	mgr := tools.NewSubAgentManager(spawner)
	mgr.SetSynchronous(true)
	tools.SetDefaultSubAgentManager(mgr)
	tools.SetTaskStore(tasks.New())
}

// enableMCP applies the Tool Search config and connects the configured MCP
// servers non-interactively, registering their surface so DefaultToolsFor
// advertises them (or the Tool Search bridge). Best-effort: a misconfigured or
// unreachable server is logged and skipped. No-op when tools are disabled.
// mcpCleanup is always set (a no-op when nothing connected) so Shutdown can call
// it unconditionally.
func (s *Server) enableMCP() {
	s.mcpCleanup = func() {}
	if !s.cfg.Tools {
		return
	}
	if cfg, err := config.Load(); err == nil {
		tools.SetToolSearchConfig(app.ToolSearchConfigFrom(cfg.Tools.ToolSearch))
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	if err := app.SwapMCP(context.Background(), s.cwd, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "octo serve: mcp: %v\n", err)
	}
	s.mcpCleanup = func() {
		s.mcpMu.Lock()
		defer s.mcpMu.Unlock()
		app.ShutdownMCP()
	}
}

// AccessKey always returns an empty string: access-key authentication was
// removed in v0.16.0. Kept for backward compatibility.
func (s *Server) AccessKey() string {
	return ""
}

// ListenAndServe starts the HTTP server. The cron scheduler is armed before
// serving so persisted tasks fire from server start, not from the first hit
// on a task endpoint.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	return s.serveOn(ln)
}

// serveOn runs the server on an already-bound listener (split from
// ListenAndServe so tests can use an ephemeral port). A stop via Shutdown is
// reported as nil — it is the expected end of a server's life, not an error —
// unless a restart was requested, in which case ErrRestartRequested tells the
// caller to exit with ExitRestart.
func (s *Server) serveOn(ln net.Listener) error {
	s.initScheduler()
	s.startChannels()
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		if s.restartPending.Load() {
			return ErrRestartRequested
		}
		return nil
	}
	return err
}

// Shutdown gracefully shuts down the server, releasing the process-global
// sub-agent + task registrations installed by enableSubAgentTools.
//
// It is single-flight: Restart's background shutdown and the Ctrl-C signal
// path can race here, and the body mutates process-global registries that
// must not be torn down twice concurrently. The first caller runs the
// shutdown; later callers wait for it to finish (or their ctx to expire)
// and return the same error.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownMu.Lock()
	if s.shutdownDone == nil {
		done := make(chan struct{})
		s.shutdownDone = done
		s.shutdownMu.Unlock()

		err := s.doShutdown(ctx)

		s.shutdownMu.Lock()
		s.shutdownErr = err
		s.shutdownMu.Unlock()
		close(done)
		return err
	}
	done := s.shutdownDone
	s.shutdownMu.Unlock()

	select {
	case <-done:
		s.shutdownMu.Lock()
		defer s.shutdownMu.Unlock()
		return s.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) doShutdown(ctx context.Context) error {
	s.stopChannels()
	// Kill background processes started via web/IM sessions so they don't
	// outlive the daemon — the same orphan-prevention the CLI/TUI do on exit.
	// terminal background commands run through the process-global manager
	// regardless of which session launched them, so one call covers all.
	tools.KillAllBackground()
	tools.KillAllSessionSubAgents()
	tools.SetDefaultSubAgentManager(nil)
	tools.SetTaskStore(nil)
	if s.mcpCleanup != nil {
		s.mcpCleanup()
	}
	return s.http.Shutdown(ctx)
}

// registerRoutes wires all handlers. API and WS routes require auth.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /api/chat", s.requireAuth(s.handleCreateChat))
	s.mux.HandleFunc("POST /api/chat/{id}/turn", s.requireAuth(s.handleTurnOrSSE))
	s.mux.HandleFunc("GET /api/sessions", s.requireAuth(s.handleListSessions))
	s.mux.HandleFunc("POST /api/sessions", s.requireAuth(s.handleCreateSession))
	s.mux.HandleFunc("POST /api/sessions/delete", s.requireAuth(s.handleDeleteSessions))
	s.mux.HandleFunc("GET /api/sessions/{id}", s.requireAuth(s.handleGetSession))
	s.mux.HandleFunc("GET /api/sessions/{id}/messages", s.requireAuth(s.handleGetSessionMessages))
	s.mux.HandleFunc("GET /api/sessions/{id}/artifacts", s.requireAuth(s.handleGetArtifact))
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.requireAuth(s.handleDeleteSession))
	s.mux.HandleFunc("PATCH /api/sessions/{id}", s.requireAuth(s.handleUpdateSession))
	s.mux.HandleFunc("PATCH /api/sessions/{id}/model", s.requireAuth(s.handleUpdateSessionModel))
	s.mux.HandleFunc("PATCH /api/sessions/{id}/reasoning_effort", s.requireAuth(s.handleUpdateSessionReasoningEffort))
	s.mux.HandleFunc("PATCH /api/sessions/{id}/working_dir", s.requireAuth(s.handleUpdateSessionWorkingDir))
	s.mux.HandleFunc("GET /api/tools", s.requireAuth(s.handleListTools))
	s.mux.HandleFunc("GET /api/skills", s.requireAuth(s.handleListSkills))
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("GET /api/channels", s.requireAuth(s.handleListChannels))
	s.mux.HandleFunc("GET /api/channels/available", s.requireAuth(s.handleAvailableChannels))
	s.mux.HandleFunc("GET /api/channels/{platform}", s.requireAuth(s.handleGetChannel))
	s.mux.HandleFunc("POST /api/channels/{platform}", s.requireAuth(s.handleSaveChannel))
	s.mux.HandleFunc("DELETE /api/channels/{platform}", s.requireAuth(s.handleDeleteChannel))
	s.mux.HandleFunc("POST /api/channels/{platform}/test", s.requireAuth(s.handleTestChannel))
	s.mux.HandleFunc("POST /api/channels/weixin/login", s.requireAuth(s.handleWeixinLoginStart))
	s.mux.HandleFunc("GET /api/channels/weixin/login", s.requireAuth(s.handleWeixinLoginStatus))
	s.mux.HandleFunc("DELETE /api/channels/weixin/login", s.requireAuth(s.handleWeixinLoginCancel))
	s.mux.HandleFunc("GET /api/tasks", s.requireAuth(s.handleListTasks))
	s.mux.HandleFunc("POST /api/tasks", s.requireAuth(s.handleCreateTask))
	s.mux.HandleFunc("DELETE /api/tasks/{id}", s.requireAuth(s.handleDeleteTask))
	s.mux.HandleFunc("POST /api/tasks/{id}/run", s.requireAuth(s.handleRunTask))
	s.mux.HandleFunc("GET /api/profile/soul", s.requireAuth(s.handleGetProfileSoul))
	s.mux.HandleFunc("GET /api/profile/user", s.requireAuth(s.handleGetProfileUser))
	s.mux.HandleFunc("GET /api/memories", s.requireAuth(s.handleGetMemories))
	s.mux.HandleFunc("GET /api/trash", s.requireAuth(s.handleGetTrash))
	s.mux.HandleFunc("POST /api/trash/empty", s.requireAuth(s.handleEmptyTrash))
	s.mux.HandleFunc("POST /api/trash/{id}/restore", s.requireAuth(s.handleRestoreTrash))
	s.mux.HandleFunc("DELETE /api/trash/{id}", s.requireAuth(s.handleDeleteTrash))

	// Onboard & config
	s.mux.HandleFunc("GET /api/onboard/status", s.requireAuth(s.handleOnboardStatus))
	s.mux.HandleFunc("POST /api/onboard/complete", s.requireAuth(s.handleOnboardComplete))
	s.mux.HandleFunc("GET /api/providers", s.requireAuth(s.handleListProviders))
	s.mux.HandleFunc("GET /api/config", s.requireAuth(s.handleGetConfig))
	s.mux.HandleFunc("POST /api/config/test", s.requireAuth(s.handleTestConfig))
	s.mux.HandleFunc("POST /api/config/models", s.requireAuth(s.handleSaveModelConfig))
	s.mux.HandleFunc("PATCH /api/config/models/{id}", s.requireAuth(s.handleUpdateModelConfig))
	s.mux.HandleFunc("DELETE /api/config/models/{id}", s.requireAuth(s.handleDeleteModelConfig))
	s.mux.HandleFunc("POST /api/config/models/{id}/default", s.requireAuth(s.handleSetDefaultModelConfig))
	s.mux.HandleFunc("POST /api/config/models/{id}/lite", s.requireAuth(s.handleSetLiteModelConfig))

	// Upload
	s.mux.HandleFunc("POST /api/upload", s.requireAuth(s.handleUpload))
	s.mux.HandleFunc("GET /api/uploads/{name}", s.requireAuth(s.handleGetUpload))

	// File action (open / download)
	s.mux.HandleFunc("POST /api/file-action", s.requireAuth(s.handleFileAction))

	// Skill toggle & delete
	s.mux.HandleFunc("PATCH /api/skills/{name}/toggle", s.requireAuth(s.handleToggleSkill))
	s.mux.HandleFunc("DELETE /api/skills/{name}", s.requireAuth(s.handleDeleteSkill))
	s.mux.HandleFunc("POST /api/skills/import", s.requireAuth(s.handleImportSkill))

	// MCP server management
	s.mux.HandleFunc("GET /api/mcp/servers", s.requireAuth(s.handleListMCPServers))
	s.mux.HandleFunc("POST /api/mcp/servers", s.requireAuth(s.handleCreateMCPServer))
	s.mux.HandleFunc("PATCH /api/mcp/servers/{name}", s.requireAuth(s.handleUpdateMCPServer))
	s.mux.HandleFunc("DELETE /api/mcp/servers/{name}", s.requireAuth(s.handleDeleteMCPServer))
	s.mux.HandleFunc("PATCH /api/mcp/servers/{name}/toggle", s.requireAuth(s.handleToggleMCPServer))
	s.mux.HandleFunc("POST /api/mcp/servers/{name}/reconnect", s.requireAuth(s.handleReconnectMCPServer))
	s.mux.HandleFunc("POST /api/mcp/servers/{name}/oauth/start", s.requireAuth(s.handleStartMCPOAuth))
	s.mux.HandleFunc("GET /api/mcp/servers/{name}/oauth/status", s.requireAuth(s.handleMCPOAuthStatus))
	s.mux.HandleFunc("POST /api/mcp/reload", s.requireAuth(s.handleReloadMCP))
	s.mux.HandleFunc("GET /api/config/toolsearch", s.requireAuth(s.handleGetToolSearch))
	s.mux.HandleFunc("PUT /api/config/toolsearch", s.requireAuth(s.handlePutToolSearch))

	// Benchmark
	s.mux.HandleFunc("POST /api/sessions/{id}/benchmark", s.requireAuth(s.handleBenchmark))

	// Memory detail
	s.mux.HandleFunc("GET /api/memories/{filename}", s.requireAuth(s.handleGetMemory))
	s.mux.HandleFunc("DELETE /api/memories/{filename}", s.requireAuth(s.handleDeleteMemory))

	// Cron tasks (alias for scheduler tasks)
	s.mux.HandleFunc("GET /api/cron-tasks", s.requireAuth(s.handleListCronTasks))
	s.mux.HandleFunc("POST /api/cron-tasks/{name}/run", s.requireAuth(s.handleRunCronTask))
	s.mux.HandleFunc("PATCH /api/cron-tasks/{name}", s.requireAuth(s.handlePatchCronTask))

	// Version & restart
	s.mux.HandleFunc("POST /api/version/upgrade", s.requireAuth(s.handleVersionUpgrade))
	s.mux.HandleFunc("POST /api/restart", s.requireAuth(s.handleRestart))

	s.mux.HandleFunc("GET /ws", s.requireAuth(s.handleWS))

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

	s.injectorMu.Lock()
	defer s.injectorMu.Unlock()
	delete(s.sessionInjectors, id)
}

// buildAgent creates a fresh agent for a turn. The caller must have locked
// the session's turn mutex.
func (s *Server) buildAgent(sess *agent.Session) *agent.Agent {
	sender, model := s.senderForSession(sess)
	a := agent.New(sender, model)
	a.CWD = s.cwd
	a.MaxTokens = s.cfg.MaxTokens
	if cfg, err := config.Load(); err == nil {
		a.LiteSender, a.LiteModel = s.liteSenderFromConfig(cfg)
	}

	// L1: project memory embedded in the system prompt (stable across turns).
	var memInjection string
	if s.memDir != "" {
		memInjection = memory.RenderInjection(s.memDir, s.homeMemDir)
	}
	a.System = prompt.Compose(s.system, s.cwd, s.envCtx, s.skillsManifest, memInjection, true)

	// L2: attention-layer rules injected per user turn (triggered keywords),
	// plus the save-nudge appended to milestone tool results. The injector is
	// created even when MEMORY.md has no structured rules — Reminder is silent
	// then, but the nudge still needs the per-session latch.
	if s.memDir != "" {
		s.injectorMu.Lock()
		inj, ok := s.sessionInjectors[sess.ID]
		if !ok {
			rules := memory.ParseRules(s.memDir)
			if s.homeMemDir != "" {
				rules.Merge(memory.ParseRules(s.homeMemDir))
			}
			inj = memory.NewInjector(rules)
			s.sessionInjectors[sess.ID] = inj
		}
		s.injectorMu.Unlock()
		a.UserInputHook = inj.Reminder
		a.ToolResultHook = inj.SaveNudge
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

	entry := cfg.DefaultEntry()
	provName := firstNonEmpty(flagProvider, os.Getenv("OCTO_PROVIDER"), entry.Provider, "anthropic")
	model := flagModel
	if model == "" {
		model = modelFromEnv(provName)
	}
	if model == "" && provName == entry.Provider {
		model = entry.Model
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
	if apiKey == "" {
		// No API key configured yet — server starts in onboarding mode.
		// Chat endpoints will retry on every request until the user completes
		// setup via the Web UI.
		return nil, model, nil
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
// else the default entry's stored key when it targets the same provider. An
// empty key is returned (not an error) so the server can start in onboarding
// mode and retry once the user completes setup via the Web UI.
func resolveAPIKey(name string, cfg config.Config) (string, error) {
	if !app.IsKnownVendor(name) {
		return "", fmt.Errorf("unknown provider %q", name)
	}
	envVar := app.VendorAPIKeyEnvVar(name)
	apiKey := os.Getenv(envVar)
	if entry := cfg.DefaultEntry(); apiKey == "" && entry.Provider == name {
		apiKey = entry.APIKey
	}
	return apiKey, nil
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
	envVar := strings.ToUpper(provider) + "_MODEL"
	return os.Getenv(envVar)
}

func defaultModelFor(provider string) string {
	return app.VendorDefaultModel(provider)
}

func resolveBaseURL(provider string, cfg config.Config) string {
	if entry := cfg.DefaultEntry(); entry.Provider == provider {
		return entry.BaseURL
	}
	return ""
}

// senderForSession resolves the (sender, model) a turn should run on. A
// session bound to a config entry (ModelConfig) gets that entry's sender from
// the cache, built on first use; everything else — unbound sessions, a
// missing entry (deleted since binding), or a build failure — falls back to
// the server's default sender so a stale binding degrades instead of
// breaking the turn.
func (s *Server) senderForSession(sess *agent.Session) (agent.Sender, string) {
	model := s.model
	if sess.Model != "" {
		model = sess.Model
	}
	if sess.ModelConfig == "" {
		return s.sender, model
	}

	cfg, err := config.Load()
	if err != nil {
		return s.sender, model
	}
	entry, ok := cfg.EntryByName(sess.ModelConfig)
	if !ok {
		return s.sender, model
	}
	if entry.Model != "" {
		model = entry.Model
	}

	sender, err := s.cachedSenderForEntry(entry)
	if err != nil {
		return s.sender, model
	}
	return sender, model
}

// cachedSenderForEntry returns the entry's sender from the cache, building
// and caching it on first use.
func (s *Server) cachedSenderForEntry(entry config.ModelEntry) (agent.Sender, error) {
	s.senderCacheMu.Lock()
	defer s.senderCacheMu.Unlock()
	if cached, ok := s.senderCache[entry.Name]; ok {
		return cached, nil
	}
	sender, err := senderForEntry(entry)
	if err != nil {
		return nil, err
	}
	if s.senderCache == nil {
		s.senderCache = make(map[string]agent.Sender)
	}
	s.senderCache[entry.Name] = sender
	return sender, nil
}

// liteSenderFromConfig resolves the configured lite entry to a (sender,
// model) pair for compaction, or (nil, "") when none is configured or it
// can't be built — the agent then compacts on its primary sender.
func (s *Server) liteSenderFromConfig(cfg config.Config) (agent.Sender, string) {
	entry, ok := cfg.EntryByName(cfg.LiteModel)
	if !ok || entry.Model == "" {
		return nil, ""
	}
	sender, err := s.cachedSenderForEntry(entry)
	if err != nil {
		return nil, ""
	}
	return sender, entry.Model
}

// senderForEntry builds a sender from one config entry: env key first (same
// rule as the default path), then the entry's stored key.
func senderForEntry(entry config.ModelEntry) (agent.Sender, error) {
	apiKey := os.Getenv(app.VendorAPIKeyEnvVar(entry.Provider))
	if apiKey == "" {
		apiKey = entry.APIKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("no API key for entry %q (provider %q)", entry.Name, entry.Provider)
	}
	return app.NewSender(app.SenderOptions{
		Provider:        entry.Provider,
		APIKey:          apiKey,
		BaseURL:         entry.BaseURL,
		ReasoningEffort: entry.ReasoningEffort,
	})
}

// invalidateSenderCache drops every cached per-entry sender. Called on any
// model-config mutation so edited credentials/endpoints take effect on the
// next turn.
func (s *Server) invalidateSenderCache() {
	s.senderCacheMu.Lock()
	s.senderCache = nil
	s.senderCacheMu.Unlock()
}

// resolveAccessKey is a no-op: access-key authentication was removed in
// v0.16.0. The field is kept on Server for backward compatibility but
// always returns an empty string.
func resolveAccessKey(cfgValue string, fileCfg config.Config) string {
	_ = cfgValue
	_ = fileCfg
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
	// Platform-shell guidance (dialect + install/PATH traps), shared with the
	// CLI builder so web sessions get the same orientation.
	b.WriteString(tools.ShellEnvNote())
	return b.String()
}

// ensureSender lazily initialises the sender when the server started in
// onboarding mode (nil sender). It re-reads config on every call so that
// once the user completes onboard and saves the API key, the next chat
// request picks it up without a server restart. Thread-safe via senderMu.
func (s *Server) ensureSender() error {
	if s.sender != nil {
		return nil
	}
	s.senderMu.Lock()
	defer s.senderMu.Unlock()
	if s.sender != nil {
		return nil
	}
	sender, model, err := resolveProviderAndModel(s.cfg.Provider, s.cfg.Model)
	if err != nil {
		return err
	}
	if sender == nil {
		return fmt.Errorf("server not configured: complete setup via the Web UI")
	}
	s.sender = sender
	s.model = model
	if s.cfg.Tools {
		s.enableSubAgentTools()
	}
	return nil
}

// validateAccessKey is a no-op: access-key authentication was removed in
// v0.16.0. Kept as a placeholder so callers do not need to change.
func (s *Server) validateAccessKey(r *http.Request) bool {
	_ = r
	return true
}

// requireAuth is a no-op wrapper: access-key authentication was removed.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return next
}

// ─── WebSocket Asker (ask_user_question bridge) ─────────────────────────────

// ctxKeySessionID is the context key used to pass the active session ID from
// the turn runner down to the wsAsker so it knows which browser tab to prompt.
type ctxKeySessionID struct{}

// wsAsker implements tools.Asker by broadcasting a structured question over
// WebSocket and blocking until the user answers (or the context is cancelled).
// It is safe for concurrent use across sessions because each question gets a
// unique ID and a private channel.
func (s *Server) wsAsker() tools.Asker {
	return wsAsker{s: s}
}

type wsAsker struct {
	s *Server
}

func (a wsAsker) Ask(ctx context.Context, q tools.AskRequest) (tools.AskResponse, error) {
	sessionID, ok := ctx.Value(ctxKeySessionID{}).(string)
	if !ok || sessionID == "" {
		return tools.AskResponse{}, fmt.Errorf("ask_user_question: no active WebSocket session")
	}

	qid := fmt.Sprintf("q_%d", time.Now().UnixNano())
	ch := make(chan tools.AskResponse, 1)

	a.s.questionMu.Lock()
	a.s.questionChans[qid] = ch
	a.s.questionMu.Unlock()

	ev := wsEventRequestUserQuestion{
		Type:        "request_user_question",
		QuestionID:  qid,
		Question:    q.Question,
		Options:     q.Options,
		MultiSelect: q.MultiSelect,
		Header:      q.Header,
	}

	// Record the outstanding question so a tab that (re)subscribes mid-ask —
	// e.g. after a page refresh — gets it replayed instead of a dead spinner.
	a.s.pendingPromptMu.Lock()
	a.s.pendingQuestions[sessionID] = ev
	a.s.pendingPromptMu.Unlock()

	cleanup := func() {
		a.s.questionMu.Lock()
		delete(a.s.questionChans, qid)
		a.s.questionMu.Unlock()
		a.s.pendingPromptMu.Lock()
		delete(a.s.pendingQuestions, sessionID)
		a.s.pendingPromptMu.Unlock()
	}

	a.s.wsHub.broadcast(sessionID, ev)

	select {
	case res := <-ch:
		cleanup()
		return res, nil
	case <-ctx.Done():
		cleanup()
		return tools.AskResponse{Cancelled: true}, nil
	case <-time.After(5 * time.Minute):
		cleanup()
		return tools.AskResponse{}, fmt.Errorf("ask_user_question: timed out waiting for user answer")
	}
}

// handleWSUserQuestionAnswer delivers a user answer from the browser to a
// pending wsAsker.Ask call.
func (s *Server) handleWSUserQuestionAnswer(qid string, choices []string, custom string, cancelled bool) {
	s.questionMu.Lock()
	if ch, ok := s.questionChans[qid]; ok {
		ch <- tools.AskResponse{
			Choices:   choices,
			Custom:    custom,
			Cancelled: cancelled,
		}
	}
	s.questionMu.Unlock()
}

// permissionAskFrom adapts the server's requestConfirmation into an
// app.PermissionAsk so the Web UI can resolve "ask" class policy verdicts
// interactively. remember is always false; a future enhancement could add a
// "Always allow" checkbox to the confirmation modal.
func (s *Server) permissionAskFrom(sessionID string) app.PermissionAsk {
	return func(ctx context.Context, toolName string, toolInput map[string]any) (bool, bool, error) {
		msg := fmt.Sprintf("Allow %s?", toolName)
		result, err := s.requestConfirmation(ctx, sessionID, msg, "yes_no")
		if err != nil {
			return false, false, err
		}
		return result == "yes", false, nil
	}
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

// ─── Channel (IM Bridge) ───────────────────────────────────────────────────

// initChannels loads channel config and creates a manager when channels are
// configured and not disabled via --no-channel.
func (s *Server) initChannels() {
	if s.cfg.NoChannel {
		return
	}
	if s.sender == nil {
		// Skip channel init when the server is in onboarding mode (no API key
		// yet). Channels will be started on the next server restart after the
		// user completes setup.
		return
	}
	chCfg, err := channel.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "octo serve: channel config: %v\n", err)
		return
	}
	platforms := chCfg.EnabledPlatforms()
	if len(platforms) == 0 {
		return
	}

	// Build agent factory that mirrors the server's agent setup. No gate is
	// set here: handleChannelMessage builds a fresh per-turn gate (configured
	// mode + chat-interactive ask), the same shape prepareToolTurn gives web
	// turns. A factory-time gate would freeze one policy snapshot for the
	// session's whole life — and the old `gate, _ :=` form silently ran turns
	// ungated when engine construction failed.
	var memInjection string
	if s.memDir != "" {
		memInjection = memory.RenderInjection(s.memDir, s.homeMemDir)
	}
	factory := func() *agent.Agent {
		a := agent.New(s.sender, s.model)
		a.CWD = s.cwd
		a.MaxTokens = s.cfg.MaxTokens
		a.System = prompt.Compose(s.system, s.cwd, s.envCtx, s.skillsManifest, memInjection, true)
		if cfg, err := config.Load(); err == nil {
			a.LiteSender, a.LiteModel = s.liteSenderFromConfig(cfg)
		}
		return a
	}

	s.channelCfg = chCfg
	s.channelMgr = channel.NewManager(chCfg, factory, channel.BindByChatUser)

	fmt.Fprintf(os.Stderr, "octo serve: channels enabled: %s\n", strings.Join(platforms, ", "))
}

// startChannels launches all enabled channel adapters in background goroutines.
func (s *Server) startChannels() {
	if s.channelMgr == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.channelCancel = cancel

	// For each enabled platform, create adapter and start it.
	for _, name := range s.channelCfg.EnabledPlatforms() {
		pc := s.channelCfg.Platform(name)
		if pc == nil {
			continue
		}
		ctor, err := channel.Find(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "octo serve: channel %s: %v\n", name, err)
			continue
		}
		ad, err := ctor(pc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "octo serve: channel %s adapter: %v\n", name, err)
			continue
		}
		if errs := ad.ValidateConfig(pc); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "octo serve: channel %s config: %s\n", name, e)
			}
			continue
		}

		s.runningAdapters.Store(name, ad)
		go func(a channel.Adapter, platform string) {
			_ = a.Start(ctx, func(ev channel.InboundEvent) {
				ev.Platform = platform
				s.routeChannelEvent(ctx, a, ev)
			})
		}(ad, name)
	}
}

// routeChannelEvent dispatches one inbound IM event. Order matters:
// commands first, so /stop can cancel a turn that is itself blocked on a
// permission ask; then a pending ask claims the message as its answer —
// inline, NOT through the turn path, which would deadlock behind the runMu
// the asking turn still holds; otherwise the message starts an agent turn
// off the adapter's read loop (Session.BeginRun serialises turns per
// session, so concurrent messages in one chat can't interleave).
func (s *Server) routeChannelEvent(ctx context.Context, ad channel.Adapter, ev channel.InboundEvent) {
	if s.handleChannelCommand(ad, ev) {
		return
	}
	// Text-less events (stickers, images, voice) never answer an ask — an
	// empty reply would consume the slot and deny for nothing.
	if strings.TrimSpace(ev.Text) != "" {
		if sess := s.channelMgr.GetSession(ev); sess != nil && sess.DeliverAskReply(ev.ChatID, ev.UserID, ev.Text) {
			return
		}
	}
	go s.handleChannelMessage(ctx, ad, ev)
}

// handleChannelCommand processes slash commands (e.g. /bind, /stop). Returns
// true if the event was a command. Without the "/" guard, plain text like
// "你好" would be parsed as an unknown command and never reach the agent.
func (s *Server) handleChannelCommand(ad channel.Adapter, ev channel.InboundEvent) bool {
	text := strings.TrimSpace(ev.Text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	if reply := s.channelMgr.CommandRouter(ev); reply != "" {
		ad.SendText(ev.ChatID, reply, ev.MessageID)
	}
	return true
}

// handleChannelMessage runs an agent turn for a channel inbound event.
func (s *Server) handleChannelMessage(ctx context.Context, ad channel.Adapter, ev channel.InboundEvent) {
	if err := s.drain.begin(); err != nil {
		// Restart drain in progress. The adapter is still up (it stops only
		// after the drain, so in-flight replies get delivered) — tell the
		// user to retry instead of silently dropping the message.
		ad.SendText(ev.ChatID, "The server is restarting — please send that again in a moment.", ev.MessageID)
		return
	}
	defer s.drain.end()

	sess := s.channelMgr.GetOrCreateSession(ev)
	if sess == nil {
		return
	}

	// Waits for any in-flight turn in this session, then makes this turn
	// cancellable by /stop (Session.Interrupt).
	ctx, done := sess.BeginRun(ctx)
	defer done()

	// Per-turn permission gate, the same shape prepareToolTurn gives web
	// turns: configured mode + an interactive ask that prompts in the chat
	// and consumes the next message as the answer. Built after BeginRun so
	// it can't race the previous turn's gate. An engine failure aborts the
	// turn — running ungated is never an acceptable fallback.
	engine, err := permission.New(permissionConfigPath(), s.cwd, resolvePermissionMode(), s.memDir, s.homeMemDir)
	if err != nil {
		// Generic chat reply — err.Error() can leak local paths into a
		// group chat; the operator gets the detail on the server console.
		fmt.Fprintf(os.Stderr, "octo serve: channel %s: permission engine: %v\n", ev.Platform, err)
		ad.SendText(ev.ChatID, "⚠️ Permission engine unavailable — message not processed. Check the server logs.", ev.MessageID)
		return
	}
	sess.Agent.Gate = app.NewPermissionGate(engine, s.channelPermissionAsk(sess, ad, ev))

	ctrl := channel.NewUIController(ad, ev.ChatID, ev.MessageID)

	var toolDefs []agent.ToolDefinition
	var executor agent.ToolExecutor
	if s.cfg.Tools {
		executor = tools.NewDefaultRegistry()
		toolDefs = tools.DefaultToolsFor(sess.Agent.Model)

		// Per-message sub-agent manager bound to THIS chat's agent —
		// synchronous, since a chat message is request/response with no
		// follow-up channel for an async result.
		spawner := app.NewSpawner(sess.Agent, executor, func() []agent.ToolDefinition {
			return tools.DefaultToolsFor(sess.Agent.Model)
		})
		subMgr := tools.NewSubAgentManager(spawner)
		subMgr.SetSynchronous(true)
		ctx = tools.WithSubAgentManager(ctx, subMgr)
		// The task store lives on the session, so the task list persists
		// across messages in this chat.
		ctx = tools.WithTaskStore(ctx, sess.Tasks)
		// Per-chat background manager; completions are surfaced to the model
		// via the Inbox so an idle-time exit is drained at the start of the
		// next message's turn.
		bgMgr := tools.SessionBackgroundManager("im:" + string(sess.Key))
		bgMgr.SetOnExit(func(e tools.BgExit) {
			sess.Agent.Inbox.Enqueue(tools.FormatBgNote(e))
		})
		ctx = tools.WithBackgroundManager(ctx, bgMgr)
	}

	_, _ = channel.RunAgent(ctx, sess, toolDefs, executor, ctrl, ev.Text)
}

// stopChannels shuts down all channel adapters and their sessions.
func (s *Server) stopChannels() {
	if s.channelCancel != nil {
		s.channelCancel()
	}
	if s.channelMgr != nil {
		_ = s.channelMgr.Stop()
	}
	s.runningAdapters = sync.Map{}
}
