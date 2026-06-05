package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// maxSubAgentResultBytes caps how much result text is retained per async sub-agent.
const maxSubAgentResultBytes = 1 << 20 // 1 MiB

// SubAgentNotification is delivered to the onExit hook when a sub-agent
// finishes a task (spawn) or replies to a message (continue).
type SubAgentNotification struct {
	AgentID      string // e.g. "agent_1"
	Description  string // human-readable label from sub_agent
	Kind         string // "spawn_done" | "message_reply"
	Result       string // final reply text
	InputTokens  int
	OutputTokens int
	// StopReason is empty for normal completion, "max_turns" when the sub-agent
	// hit its loop budget. The parent uses this to decide whether to continue.
	StopReason string
}

// SubAgentInfo is a snapshot of a sub-agent for listing.
type SubAgentInfo struct {
	ID          string
	Description string
	Start       time.Time
	Busy        bool
}

// asyncSubAgent tracks one detached sub-agent launched via SubAgentManager.
type asyncSubAgent struct {
	id          string
	description string
	cancel      context.CancelFunc
	start       time.Time

	mu           sync.Mutex
	busy         bool   // true while processing a Spawn or Continue
	pending      string // queued message waiting to be sent
	backingID    string // Spawner-side id (the resumable child's handle); set once Spawn returns. Continue/Send address the child by this, not by the manager's agent_N.
	result       string // latest result (truncated to maxSubAgentResultBytes)
	exited       bool   // true if the sub-agent context was cancelled / killed
	exitErr      error
	stopReason   string // "max_turns" when the loop budget was exhausted
	inputTokens  int
	outputTokens int
}

func (a *asyncSubAgent) setResult(result string, inputTokens, outputTokens int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.result = truncateSubAgentResult(result)
	a.inputTokens = inputTokens
	a.outputTokens = outputTokens
}

func (a *asyncSubAgent) setBusy(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.busy = v
}

func (a *asyncSubAgent) setDone(err error, stopReason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.exited = true
	}
	a.exitErr = err
	a.stopReason = stopReason
	a.busy = false
}

func (a *asyncSubAgent) setExited(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.exited = true
	a.exitErr = err
	a.busy = false
}

func (a *asyncSubAgent) readState() (result, status string, busy, exited bool, stopReason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result = a.result
	busy = a.busy
	exited = a.exited
	stopReason = a.stopReason
	if a.exited {
		if a.exitErr != nil {
			status = "exited: " + a.exitErr.Error()
		} else {
			status = "exited: 0"
		}
	} else if a.busy {
		status = "running"
	} else {
		status = "idle"
	}
	return
}

func truncateSubAgentResult(s string) string {
	if len(s) <= maxSubAgentResultBytes {
		return s
	}
	return s[:maxSubAgentResultBytes] + "\n...[truncated]"
}

// defaultSubAgentMgr is the process-wide manager used by the built-in tools
// when no manager is injected.
var defaultSubAgentMgr *SubAgentManager

// SetDefaultSubAgentManager registers the global manager used by AgentTool
// when no local manager is set.
func SetDefaultSubAgentManager(m *SubAgentManager) { defaultSubAgentMgr = m }

// subAgentManagerEnabled reports whether a SubAgentManager is registered,
// which is required for the Agent tool.
func subAgentManagerEnabled() bool { return defaultSubAgentMgr != nil }

// SubAgentManager owns the set of async sub-agents for a session.
// Methods are safe for concurrent use.
type SubAgentManager struct {
	mu          sync.Mutex
	agents      map[string]*asyncSubAgent
	seq         int
	spawner     Spawner
	onExit      func(SubAgentNotification)
	onEvent     func(SubAgentEvent)
	synchronous bool
}

// NewSubAgentManager returns an empty manager.
func NewSubAgentManager(spawner Spawner) *SubAgentManager {
	return &SubAgentManager{
		agents:  map[string]*asyncSubAgent{},
		spawner: spawner,
	}
}

// SetSynchronous selects the sub_agent dispatch model. The default (false)
// is the interactive async path: Start returns immediately and the reply
// arrives via onExit, which the REPL/TUI re-injects as a follow-up turn. A
// request/response transport (HTTP server, IM bridge) has no follow-up-turn
// channel, so it sets this true: sub_agent then blocks the turn on RunSync
// and returns the child's reply directly as the tool_result. Set once at
// startup, before any turn runs.
func (m *SubAgentManager) SetSynchronous(v bool) {
	m.mu.Lock()
	m.synchronous = v
	m.mu.Unlock()
}

// Synchronous reports whether the manager runs sub-agents inline (see
// SetSynchronous).
func (m *SubAgentManager) Synchronous() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.synchronous
}

// subAgentsSynchronous reports whether the process-global manager runs
// sub-agents inline. In synchronous mode a sub-agent completes before
// sub_agent returns, so agent_status / kill_agent have nothing to act on —
// DefaultToolsFor withholds them there rather than advertise dead tools.
func subAgentsSynchronous() bool {
	return defaultSubAgentMgr != nil && defaultSubAgentMgr.Synchronous()
}

// RunSync spawns a sub-agent and blocks until it completes, returning its
// reply. Used by the synchronous sub_agent path; the spawner stamps the
// sub-agent marker and keeps the child resumable for a later ContinueSync.
func (m *SubAgentManager) RunSync(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	if m.spawner == nil {
		return SpawnResult{}, fmt.Errorf("subagent: no spawner configured")
	}
	return m.spawner.Spawn(ctx, req)
}

// ContinueSync re-runs a still-alive sub-agent (addressed by its spawner-side
// id) with a new message and blocks until it replies. The synchronous
// counterpart of Send.
func (m *SubAgentManager) ContinueSync(ctx context.Context, agentID, message string) (SpawnResult, error) {
	if m.spawner == nil {
		return SpawnResult{}, fmt.Errorf("subagent: no spawner configured")
	}
	return m.spawner.Continue(ctx, agentID, message)
}

// subAgentManagerCtxKey carries a per-turn SubAgentManager so the sub-agent
// tools dispatch to it instead of the process-global defaultSubAgentMgr. A
// request/response transport (server, IM) builds a fresh manager bound to the
// turn's agent and stamps it here; the interactive CLI leaves it unset and
// falls through to the global.
type subAgentManagerCtxKeyType struct{}

var subAgentManagerCtxKey = subAgentManagerCtxKeyType{}

// WithSubAgentManager returns ctx carrying mgr for the sub-agent tools to find.
func WithSubAgentManager(ctx context.Context, mgr *SubAgentManager) context.Context {
	return context.WithValue(ctx, subAgentManagerCtxKey, mgr)
}

// subAgentManagerFromContext returns the ctx-scoped manager, or nil.
func subAgentManagerFromContext(ctx context.Context) *SubAgentManager {
	m, _ := ctx.Value(subAgentManagerCtxKey).(*SubAgentManager)
	return m
}

// resolveSubAgentManager picks the manager a tool should dispatch to: the
// ctx-scoped one (per-turn, server/IM) first, then a tool-local override, then
// the process-global default (CLI). Returns nil when none is configured.
func resolveSubAgentManager(ctx context.Context, local *SubAgentManager) *SubAgentManager {
	if m := subAgentManagerFromContext(ctx); m != nil {
		return m
	}
	if local != nil {
		return local
	}
	return defaultSubAgentMgr
}

// SetOnExit registers a completion hook fired once per sub-agent when it
// finishes processing. Pass nil to clear.
func (m *SubAgentManager) SetOnExit(fn func(SubAgentNotification)) {
	m.mu.Lock()
	m.onExit = fn
	m.mu.Unlock()
}

// SetOnEvent registers a runtime-event hook fired as a sub-agent works
// (started + per-tool activity), for live display. Pass nil to clear. Distinct
// from onExit, which fires once on completion.
func (m *SubAgentManager) SetOnEvent(fn func(SubAgentEvent)) {
	m.mu.Lock()
	m.onEvent = fn
	m.mu.Unlock()
}

// eventSink builds the per-agent sink stamped into the spawn context. Returns
// nil when no onEvent hook is set, so the spawner skips streaming entirely.
func (m *SubAgentManager) eventSink(id, description string) func(SubAgentEvent) {
	m.mu.Lock()
	onEvent := m.onEvent
	m.mu.Unlock()
	if onEvent == nil {
		return nil
	}
	return func(ev SubAgentEvent) {
		ev.AgentID = id
		if ev.Description == "" {
			ev.Description = description
		}
		onEvent(ev)
	}
}

// Start creates a new sub-agent and runs it asynchronously.
// Returns the agent_id immediately; result arrives via onExit.
func (m *SubAgentManager) Start(req SpawnRequest) (string, error) {
	if m.spawner == nil {
		return "", fmt.Errorf("subagent: no spawner configured")
	}

	ctx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("agent_%d", m.seq)
	agent := &asyncSubAgent{
		id:          id,
		description: req.Description,
		cancel:      cancel,
		start:       time.Now(),
		busy:        true,
	}
	m.agents[id] = agent
	m.mu.Unlock()

	// Stream runtime events (started + per-tool) for live display. nil sink =>
	// no onEvent hook => the spawner runs without streaming.
	if sink := m.eventSink(id, req.Description); sink != nil {
		ctx = WithSubAgentEventSink(ctx, sink)
		sink(SubAgentEvent{Kind: "started"})
	}

	go func() {
		res, err := m.spawner.Spawn(ctx, req)
		stopReason := ""
		if err == nil {
			agent.mu.Lock()
			agent.backingID = res.AgentID // remember the resumable child so Send can reach it
			agent.mu.Unlock()
			agent.setResult(res.Reply, res.InputTokens, res.OutputTokens)
			stopReason = res.StopReason
		}
		agent.setDone(err, stopReason)

		m.mu.Lock()
		hook := m.onExit
		m.mu.Unlock()
		if hook != nil {
			result, _, _, _, sr := agent.readState()
			if err != nil {
				result = err.Error()
			}
			agent.mu.Lock()
			inTok := agent.inputTokens
			outTok := agent.outputTokens
			agent.mu.Unlock()
			hook(SubAgentNotification{
				AgentID:      id,
				Description:  req.Description,
				Kind:         "spawn_done",
				Result:       result,
				InputTokens:  inTok,
				OutputTokens: outTok,
				StopReason:   sr,
			})
		}
	}()

	return id, nil
}

// Send delivers a message to an existing sub-agent asynchronously.
// Returns immediately; reply arrives via onExit.
func (m *SubAgentManager) Send(agentID, message string) error {
	m.mu.Lock()
	agent := m.agents[agentID]
	m.mu.Unlock()
	if agent == nil {
		return fmt.Errorf("subagent: no sub-agent %q", agentID)
	}

	agent.mu.Lock()
	if agent.exited {
		agent.mu.Unlock()
		return fmt.Errorf("subagent: %s has exited", agentID)
	}
	if agent.busy {
		if agent.pending != "" {
			agent.mu.Unlock()
			return fmt.Errorf("subagent: %s already has a pending message", agentID)
		}
		agent.pending = message
		agent.mu.Unlock()
		return nil // queued, will be sent when current processing done
	}
	agent.busy = true
	agent.mu.Unlock()

	go m.runContinue(agentID, message)
	return nil
}

func (m *SubAgentManager) runContinue(agentID, message string) {
	agent := m.agents[agentID]
	if agent == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Replace the agent's cancel so Kill targets the current operation.
	agent.mu.Lock()
	agent.cancel = cancel
	backingID := agent.backingID
	desc := agent.description
	agent.mu.Unlock()

	// Stream runtime events for this round too, so the live panel re-shows the
	// sub-agent as running while it handles the message.
	if sink := m.eventSink(agentID, desc); sink != nil {
		ctx = WithSubAgentEventSink(ctx, sink)
		sink(SubAgentEvent{Kind: "started"})
	}

	// Continue addresses the child by its Spawner-side id, not the manager's
	// agent_N handle — the two id spaces are distinct.
	res, err := m.spawner.Continue(ctx, backingID, message)
	stopReason := ""
	if err == nil {
		agent.setResult(res.Reply, res.InputTokens, res.OutputTokens)
		stopReason = res.StopReason
	}
	agent.setDone(err, stopReason)

	m.mu.Lock()
	hook := m.onExit
	m.mu.Unlock()
	if hook != nil {
		result, _, _, _, sr := agent.readState()
		if err != nil {
			result = err.Error()
		}
		agent.mu.Lock()
		inTok := agent.inputTokens
		outTok := agent.outputTokens
		agent.mu.Unlock()
		hook(SubAgentNotification{
			AgentID:      agentID,
			Description:  agent.description,
			Kind:         "message_reply",
			Result:       result,
			InputTokens:  inTok,
			OutputTokens: outTok,
			StopReason:   sr,
		})
	}

	// Process pending message, if any.
	agent.mu.Lock()
	pending := agent.pending
	agent.pending = ""
	if pending != "" && !agent.exited {
		agent.busy = true
		agent.mu.Unlock()
		go m.runContinue(agentID, pending)
		return
	}
	agent.mu.Unlock()
}

// Read returns the latest result and status for an agent.
// found is false when id is unknown.
func (m *SubAgentManager) Read(id string) (result, status string, found bool) {
	m.mu.Lock()
	agent := m.agents[id]
	m.mu.Unlock()
	if agent == nil {
		return "", "", false
	}
	result, status, _, _, _ = agent.readState()
	return result, status, true
}

// Kill terminates the sub-agent for id. Returns false when id is unknown.
func (m *SubAgentManager) Kill(id string) bool {
	m.mu.Lock()
	agent := m.agents[id]
	m.mu.Unlock()
	if agent == nil {
		return false
	}
	agent.setExited(context.Canceled)
	agent.cancel()
	return true
}

// KillAll terminates every tracked sub-agent. Called on session shutdown.
func (m *SubAgentManager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		agent.cancel()
	}
}

// ListRunning returns the agents that haven't exited yet, oldest first.
func (m *SubAgentManager) ListRunning() []SubAgentInfo {
	m.mu.Lock()
	agents := make([]*asyncSubAgent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, a)
	}
	m.mu.Unlock()

	var out []SubAgentInfo
	for _, a := range agents {
		_, _, busy, done, _ := a.readState()
		if !done {
			out = append(out, SubAgentInfo{
				ID:          a.id,
				Description: a.description,
				Start:       a.start,
				Busy:        busy,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}
