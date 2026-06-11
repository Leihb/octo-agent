package channel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tasks"
)

// BindingMode determines how an inbound message maps to an agent session.
type BindingMode string

const (
	// BindByChatUser creates one session per (chat, user) pair.
	// Best for group chats where multiple users interact independently.
	BindByChatUser BindingMode = "chat_user"

	// BindByChat creates one session per chat (group or DM).
	// Best for DMs or when all users in a group share one context.
	BindByChat BindingMode = "chat"

	// BindByUser creates one session per user across all chats.
	// Best when a single user wants continuity across multiple groups.
	BindByUser BindingMode = "user"
)

// SessionKey uniquely identifies a conversation session.
type SessionKey string

// sessionKeyFor returns the session key for an inbound event based on the binding mode.
func sessionKeyFor(mode BindingMode, ev InboundEvent) SessionKey {
	switch mode {
	case BindByChatUser:
		return SessionKey(ev.Platform + ":" + ev.ChatID + ":" + ev.UserID)
	case BindByUser:
		return SessionKey(ev.Platform + ":" + ev.UserID)
	default: // BindByChat
		return SessionKey(ev.Platform + ":" + ev.ChatID)
	}
}

// Session holds the agent and its binding state for one conversation.
type Session struct {
	Key   SessionKey
	Agent *agent.Agent
	// Tasks is the conversation's task store. It lives as long as the session,
	// so task_* state persists across messages within one chat (a fresh
	// per-message store would reset the list every turn). Stamped into the turn
	// ctx by the IM handler; *tasks.Store satisfies tools.TaskStore.
	Tasks   *tasks.Store
	ChatID  string
	UserID  string
	BoundAt time.Time

	// runMu serialises agent turns within this session: the IM dispatcher runs
	// each message in its own goroutine, and two interleaved turns would
	// corrupt the agent's user/assistant history alternation. runCancel
	// (guarded by cancelMu, not runMu — /stop must not queue behind the very
	// turn it is trying to cancel) aborts the in-flight turn.
	runMu     sync.Mutex
	cancelMu  sync.Mutex
	runCancel context.CancelFunc

	// pendingAsk, while non-nil, claims the session's next plain message as
	// the answer to an interactive permission prompt (see ask.go). Guarded by
	// askMu, not runMu — the reply arrives while the asking turn holds runMu.
	askMu      sync.Mutex
	pendingAsk chan string
}

// BeginRun prepares one agent turn: it blocks until any previous turn in this
// session finishes, then returns a cancellable ctx for the run and a done
// func that releases the session. Always call done (typically deferred).
func (s *Session) BeginRun(ctx context.Context) (context.Context, func()) {
	s.runMu.Lock()
	ctx, cancel := context.WithCancel(ctx)
	s.cancelMu.Lock()
	s.runCancel = cancel
	s.cancelMu.Unlock()
	return ctx, func() {
		s.cancelMu.Lock()
		s.runCancel = nil
		s.cancelMu.Unlock()
		cancel()
		s.runMu.Unlock()
	}
}

// Interrupt cancels the session's in-flight agent turn, if any. It reports
// whether a turn was actually running.
func (s *Session) Interrupt() bool {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.runCancel == nil {
		return false
	}
	s.runCancel()
	s.runCancel = nil
	return true
}

// AgentFactory creates a new agent.Agent for a session.
type AgentFactory func() *agent.Agent

// newSession builds a Session with a fresh agent and per-conversation task
// store. Centralised so every binding path (explicit /bind, auto-create on
// first message, GetOrCreateSession) wires sessions identically.
func (m *Manager) newSession(key SessionKey, ev InboundEvent) *Session {
	return &Session{
		Key:     key,
		Agent:   m.factory(),
		Tasks:   tasks.New(),
		ChatID:  ev.ChatID,
		UserID:  ev.UserID,
		BoundAt: time.Now(),
	}
}

// Manager owns the lifecycle of adapters and their bound sessions.
type Manager struct {
	cfg     *Config
	factory AgentFactory
	mode    BindingMode

	// adapters holds running platform adapters keyed by platform name.
	adapters sync.Map // string -> Adapter

	// sessions holds active sessions keyed by SessionKey.
	sessions sync.Map // SessionKey -> *Session

	// mu guards the running flag.
	mu      sync.RWMutex
	running bool
	cancel  context.CancelFunc
}

// NewManager creates a Manager. If mode is empty it defaults to BindByChatUser.
func NewManager(cfg *Config, factory AgentFactory, mode BindingMode) *Manager {
	if mode == "" {
		mode = BindByChatUser
	}
	return &Manager{
		cfg:     cfg,
		factory: factory,
		mode:    mode,
	}
}

// Start launches all enabled adapters and begins listening for messages.
// It blocks until Stop is called or the context is cancelled.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return fmt.Errorf("manager already running")
	}
	m.running = true
	ctx, m.cancel = context.WithCancel(ctx)
	m.mu.Unlock()

	for _, name := range m.cfg.EnabledPlatforms() {
		pc := m.cfg.Platform(name)
		if pc == nil {
			continue
		}
		ctor, err := Find(name)
		if err != nil {
			continue // adapter not registered, skip
		}
		ad, err := ctor(pc)
		if err != nil {
			continue // construction failed, skip
		}
		if errs := ad.ValidateConfig(pc); len(errs) > 0 {
			continue // invalid config, skip
		}
		m.adapters.Store(name, ad)

		go func(a Adapter, platform string) {
			_ = a.Start(ctx, func(ev InboundEvent) {
				ev.Platform = platform
				m.handleInbound(ctx, ev)
			})
		}(ad, name)
	}

	<-ctx.Done()
	return ctx.Err()
}

// Stop signals all adapters to shut down.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	m.adapters.Range(func(_, value any) bool {
		if ad, ok := value.(Adapter); ok {
			_ = ad.Stop()
		}
		return true
	})
	return nil
}

// IsRunning reports whether the manager is currently active.
func (m *Manager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// handleInbound routes an inbound event: commands go to commandRouter,
// everything else goes to the session handler.
func (m *Manager) handleInbound(ctx context.Context, ev InboundEvent) {
	text := strings.TrimSpace(ev.Text)
	if strings.HasPrefix(text, "/") {
		reply := m.CommandRouter(ev)
		if reply != "" {
			m.sendReply(ev, reply)
		}
		return
	}
	m.handleSessionMessage(ctx, ev)
}

// CommandRouter processes slash commands and returns a reply text.
func (m *Manager) CommandRouter(ev InboundEvent) string {
	parts := strings.Fields(strings.TrimSpace(ev.Text))
	if len(parts) == 0 {
		return ""
	}
	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	switch cmd {
	case "/bind":
		return m.cmdBind(ev, args)
	case "/stop":
		return m.cmdStop(ev)
	case "/unbind":
		return m.cmdUnbind(ev)
	case "/status":
		return m.cmdStatus(ev)
	case "/list":
		return m.cmdList()
	default:
		return fmt.Sprintf("Unknown command: %s. Available: /bind, /stop, /unbind, /status, /list", cmd)
	}
}

// cmdBind explicitly binds the current chat/user to a new session.
func (m *Manager) cmdBind(ev InboundEvent, args []string) string {
	key := sessionKeyFor(m.mode, ev)
	if _, loaded := m.sessions.Load(key); loaded {
		m.sessions.Delete(key)
	}
	sess := m.newSession(key, ev)
	m.sessions.Store(key, sess)
	modeStr := string(m.mode)
	if modeStr == "" {
		modeStr = string(BindByChatUser)
	}
	return fmt.Sprintf("Session bound (%s). Key: %s", modeStr, key)
}

// cmdStop interrupts the session's in-flight agent turn (mirrors the Ctrl-C /
// web "interrupt" semantics; history is repaired by finishInterrupted and the
// session stays usable).
func (m *Manager) cmdStop(ev InboundEvent) string {
	key := sessionKeyFor(m.mode, ev)
	val, loaded := m.sessions.Load(key)
	if !loaded {
		return "No active session for this context."
	}
	if val.(*Session).Interrupt() {
		return "Task interrupted."
	}
	return "No task is running."
}

// cmdUnbind deletes the current session and its history.
func (m *Manager) cmdUnbind(ev InboundEvent) string {
	key := sessionKeyFor(m.mode, ev)
	if _, loaded := m.sessions.LoadAndDelete(key); !loaded {
		return "No active session for this context."
	}
	return "Session unbound and history cleared."
}

// cmdStatus reports the current session state.
func (m *Manager) cmdStatus(ev InboundEvent) string {
	key := sessionKeyFor(m.mode, ev)
	val, ok := m.sessions.Load(key)
	if !ok {
		return "No active session. Send a message or use /bind to start one."
	}
	sess := val.(*Session)
	inTok, outTok := sess.Agent.SessionTokens()
	return fmt.Sprintf("Session active since %s. Input: %d tokens, Output: %d tokens.",
		sess.BoundAt.Format("15:04:05"), inTok, outTok)
}

// cmdList returns all active sessions.
func (m *Manager) cmdList() string {
	var count int
	m.sessions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count == 0 {
		return "No active sessions."
	}
	return fmt.Sprintf("Active sessions: %d", count)
}

// handleSessionMessage routes a non-command message to the appropriate session,
// creating one automatically if needed.
func (m *Manager) handleSessionMessage(ctx context.Context, ev InboundEvent) {
	key := sessionKeyFor(m.mode, ev)

	val, loaded := m.sessions.Load(key)
	if !loaded {
		// Auto-create session on first message.
		sess := m.newSession(key, ev)
		val, _ = m.sessions.LoadOrStore(key, sess)
	}
	sess := val.(*Session)

	// Notify the adapter that we're processing (typing indicator).
	m.sendTyping(ev)

	// The actual agent run is delegated to the UI controller callback.
	// The manager itself does not run the agent — it only manages sessions.
	// The caller (CLI wiring) provides the event handler that bridges
	// agent events back to the adapter.
	_ = ctx
	_ = sess
}

// sendReply sends a text reply back to the chat where the event originated.
func (m *Manager) sendReply(ev InboundEvent, text string) {
	val, ok := m.adapters.Load(ev.Platform)
	if !ok {
		return
	}
	ad := val.(Adapter)
	ad.SendText(ev.ChatID, text, ev.MessageID)
}

// sendTyping sends a typing indicator to the chat.
func (m *Manager) sendTyping(ev InboundEvent) {
	val, ok := m.adapters.Load(ev.Platform)
	if !ok {
		return
	}
	ad := val.(Adapter)
	// Best-effort; ignore errors.
	_ = ad.SendTyping(ev.ChatID, ev.ContextToken)
}

// GetSession returns the session for the given inbound event, or nil if none exists.
func (m *Manager) GetSession(ev InboundEvent) *Session {
	key := sessionKeyFor(m.mode, ev)
	val, ok := m.sessions.Load(key)
	if !ok {
		return nil
	}
	return val.(*Session)
}

// GetOrCreateSession returns the existing session or creates a new one.
func (m *Manager) GetOrCreateSession(ev InboundEvent) *Session {
	key := sessionKeyFor(m.mode, ev)
	val, loaded := m.sessions.Load(key)
	if !loaded {
		sess := m.newSession(key, ev)
		val, _ = m.sessions.LoadOrStore(key, sess)
	}
	return val.(*Session)
}

// AdapterFor returns the adapter for a platform name.
func (m *Manager) AdapterFor(platform string) Adapter {
	val, ok := m.adapters.Load(platform)
	if !ok {
		return nil
	}
	return val.(Adapter)
}

// SessionCount returns the number of active sessions.
func (m *Manager) SessionCount() int {
	var count int
	m.sessions.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
