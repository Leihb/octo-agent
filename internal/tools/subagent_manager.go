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
// finishes a task (launch_agent) or replies to a message (send_message).
type SubAgentNotification struct {
	AgentID      string // e.g. "agent_1"
	Description  string // human-readable label from launch_agent
	Kind         string // "spawn_done" | "message_reply"
	Result       string // final reply text
	InputTokens  int
	OutputTokens int
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

func (a *asyncSubAgent) setDone(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.exited = true
	}
	a.exitErr = err
	a.busy = false
}

func (a *asyncSubAgent) setExited(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.exited = true
	a.exitErr = err
	a.busy = false
}

func (a *asyncSubAgent) readState() (result, status string, busy, exited bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result = a.result
	busy = a.busy
	exited = a.exited
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

// SetDefaultSubAgentManager registers the global manager used by LaunchAgentTool
// and SendMessageTool when no local manager is set.
func SetDefaultSubAgentManager(m *SubAgentManager) { defaultSubAgentMgr = m }

// SubAgentManager owns the set of async sub-agents for a session.
// Methods are safe for concurrent use.
type SubAgentManager struct {
	mu      sync.Mutex
	agents  map[string]*asyncSubAgent
	seq     int
	spawner Spawner
	onExit  func(SubAgentNotification)
}

// NewSubAgentManager returns an empty manager.
func NewSubAgentManager(spawner Spawner) *SubAgentManager {
	return &SubAgentManager{
		agents:  map[string]*asyncSubAgent{},
		spawner: spawner,
	}
}

// SetOnExit registers a completion hook fired once per sub-agent when it
// finishes processing. Pass nil to clear.
func (m *SubAgentManager) SetOnExit(fn func(SubAgentNotification)) {
	m.mu.Lock()
	m.onExit = fn
	m.mu.Unlock()
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

	go func() {
		res, err := m.spawner.Spawn(ctx, req)
		if err == nil {
			agent.mu.Lock()
			agent.backingID = res.AgentID // remember the resumable child so Send can reach it
			agent.mu.Unlock()
			agent.setResult(res.Reply, res.InputTokens, res.OutputTokens)
		}
		agent.setDone(err)

		m.mu.Lock()
		hook := m.onExit
		m.mu.Unlock()
		if hook != nil {
			result, _, _, _ := agent.readState()
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
	agent.mu.Unlock()

	// Continue addresses the child by its Spawner-side id, not the manager's
	// agent_N handle — the two id spaces are distinct.
	res, err := m.spawner.Continue(ctx, backingID, message)
	if err == nil {
		agent.setResult(res.Reply, res.InputTokens, res.OutputTokens)
	}
	agent.setDone(err)

	m.mu.Lock()
	hook := m.onExit
	m.mu.Unlock()
	if hook != nil {
		result, _, _, _ := agent.readState()
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
	result, status, _, _ = agent.readState()
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
		_, _, busy, done := a.readState()
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
