# Async Sub-Agent Design

## Status

Draft — pending implementation (M10+).

## Problem

The current `launch_agent` tool is **synchronous**: the parent agent blocks until the sub-agent completes and returns its final reply in the tool result. This means:

1. The parent cannot do other work while the sub-agent runs.
2. The user cannot interact with the parent until the sub-agent finishes.
3. Parallel sub-agent calls in one batch still block the parent on the slowest one.

We want sub-agents to run **asynchronously** — fire-and-forget, with results delivered via notification when ready.

## Goals

- `launch_agent` starts a sub-agent and returns immediately with an `agent_id`.
- `send_message` sends a message to a sub-agent and returns immediately.
- Sub-agent results/replies are delivered asynchronously via a notification mechanism.
- The parent agent can continue processing user input or run other tools while sub-agents work.
- Design reuses the proven `BackgroundManager` pattern from the `terminal` tool.

## Non-Goals

- Sub-agents spawning sub-agents (recursion stays forbidden).
- Persistent sub-agents across sessions (sub-agents live only within the current session).
- Bidirectional streaming between parent and sub-agent (notifications are one-shot, not streams).

## Design

### Overview

```
┌─────────────────────────────────────────────────────────────┐
│  Parent Agent (REPL session)                                │
│                                                             │
│  launch_agent ──► SubAgentManager.Start() ──► agent_1      │
│       │                           │                         │
│       │                    [goroutine]                      │
│       │                           │                         │
│       │                      spawner.Spawn()                │
│       │                           │                         │
│       │                      [sub-agent runs]               │
│       │                           │                         │
│       │                      onExit(notification)           │
│       │                           │                         │
│       │                           ▼                         │
│       └───────────────────► injected into conversation      │
│                                                             │
│  send_message ──► SubAgentManager.Send() ──► agent_1       │
│       │                           │                         │
│       │                    [goroutine]                      │
│       │                           │                         │
│       │                      spawner.Continue()             │
│       │                           │                         │
│       │                      [sub-agent replies]            │
│       │                           │                         │
│       │                      onExit(notification)           │
│       │                           │                         │
│       │                           ▼                         │
│       └───────────────────► injected into conversation      │
│                                                             │
│  agent_status(agent_1) ──► query state/result (optional)   │
│  kill_agent(agent_1)     ──► terminate                      │
└─────────────────────────────────────────────────────────────┘
```

### SubAgentManager

Analogous to `BackgroundManager` (`internal/tools/background.go`), but for sub-agents instead of shell processes.

```go
package tools

// SubAgentNotification is delivered to the onExit hook when a sub-agent
// finishes a task or replies to a message.
type SubAgentNotification struct {
    AgentID      string // e.g. "agent_1"
    Description  string // human-readable label from launch_agent
    Kind         string // "spawn_done" | "message_reply"
    Result       string // final reply text
    InputTokens  int
    OutputTokens int
}

// SubAgentManager owns the set of async sub-agents for a session.
type SubAgentManager struct {
    mu     sync.Mutex
    agents map[string]*asyncSubAgent
    seq    int
    spawner Spawner
    onExit func(SubAgentNotification)
}

type asyncSubAgent struct {
    id          string
    description string
    cancel      context.CancelFunc
    start       time.Time

    mu       sync.Mutex
    busy     bool        // true while processing a Spawn or Continue
    result   string      // latest result
    done     bool        // true if the sub-agent has exited
    exitErr  error
    inputTokens  int
    outputTokens int
}
```

Key methods:

```go
// Start creates a new sub-agent and runs it asynchronously.
// Returns the agent_id immediately; result arrives via onExit.
func (m *SubAgentManager) Start(req SpawnRequest) (string, error)

// Send sends a message to an existing sub-agent asynchronously.
// Returns immediately; reply arrives via onExit.
func (m *SubAgentManager) Send(agentID, message string) error

// Read returns the latest result/status for an agent.
func (m *SubAgentManager) Read(id string) (result, status string, found bool)

// Kill terminates a sub-agent.
func (m *SubAgentManager) Kill(id string) bool

// ListRunning returns agents that haven't exited yet.
func (m *SubAgentManager) ListRunning() []SubAgentInfo
```

### Tool Changes

#### `launch_agent` — always async

Remove the synchronous path. `Execute` always calls `mgr.Start()` and returns immediately.

```go
func (LaunchAgentTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
    // ... validation ...
    id, err := mgr.Start(SpawnRequest{...})
    if err != nil { return ..., err }
    return agent.ToolResult{
        Text: fmt.Sprintf("Started sub-agent %s. You will be notified when it completes.", id),
    }, nil
}
```

LLM-facing description:

```
Spawn an autonomous sub-agent to handle a focused sub-task.
The sub-agent starts immediately and runs in the background.
You will receive its result via a system notification when it completes.
While the sub-agent runs, you can continue with other tasks.
```

#### `send_message` — always async

Same pattern: send and forget, reply arrives via notification.

```go
func (SendMessageTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
    // ... validation ...
    err := mgr.Send(agentID, message)
    if err != nil { return ..., err }
    return agent.ToolResult{
        Text: fmt.Sprintf("Message sent to %s. You will be notified when it replies.", agentID),
    }, nil
}
```

LLM-facing description:

```
Send a message to a sub-agent you previously started with launch_agent.
The message is delivered asynchronously; you will receive the reply via
a system notification when the sub-agent responds.
```

#### `agent_status` — new tool

Query the state of an async sub-agent. Optional — LLM should not poll.

```go
type AgentStatusTool struct{}

func (AgentStatusTool) Definition() agent.ToolDefinition {
    return agent.ToolDefinition{
        Name: "agent_status",
        Description: "Read the current status and latest result from a sub-agent. " +
            "You do NOT need to poll this tool. Sub-agent results are delivered " +
            "automatically via notification when ready. Only use this if you need " +
            "to check progress mid-run or if the user explicitly asks.",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "agent_id": map[string]any{
                    "type":        "string",
                    "description": "The sub-agent id (e.g. \"agent_1\").",
                },
            },
            "required": []string{"agent_id"},
        },
    }
}
```

#### `kill_agent` — new tool

Terminate a running sub-agent.

```go
type KillAgentTool struct{}

func (KillAgentTool) Definition() agent.ToolDefinition {
    return agent.ToolDefinition{
        Name: "kill_agent",
        Description: "Terminate a sub-agent started by launch_agent and return its final result (if any).",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "agent_id": map[string]any{
                    "type":        "string",
                    "description": "The sub-agent id to terminate (e.g. \"agent_1\").",
                },
            },
            "required": []string{"agent_id"},
        },
    }
}
```

### Notification Delivery

When a sub-agent completes (Spawn or Continue), the `SubAgentManager` fires the `onExit` hook. The REPL wires this hook to inject a system message into the conversation.

```go
// In cmd/octo REPL setup:
subAgentMgr := tools.NewSubAgentManager(spawner)
subAgentMgr.SetOnExit(func(ev tools.SubAgentNotification) {
    // Format: "[agent agent_1] Task completed: <result>"
    // or:     "[agent agent_1] Reply: <result>"
    msg := formatSubAgentNotification(ev)
    repl.injectSystemMessage(msg)
})
```

The injected message becomes part of the conversation history. On the next LLM call, the model sees the notification and can act on it.

**Open question**: If the parent is idle (no active LLM call) when the notification arrives, how is the next turn triggered?

Options:
1. **Inbox queue**: Push into the existing inbox mechanism. The next user message or auto-triggered turn will pick it up.
2. **Auto-trigger**: The REPL automatically starts a new LLM turn when a notification arrives while idle. This may be surprising — the agent would "speak up" unprompted.
3. **Hybrid**: Notifications accumulate in a queue. The REPL shows a "N pending notifications" indicator. The user can ask "what's new?" or the next natural turn processes them.

**Recommendation**: Start with option 1 (inbox queue). It reuses existing machinery and doesn't introduce unprompted agent speech. Evaluate option 3 if the UX feels laggy.

### Concurrency Model

A sub-agent can only process **one request at a time** (one Spawn or one Continue). If `Send()` is called while the sub-agent is `busy`:

- **Queue the message** (preferred): Store it and send after current processing completes.
- **Return error**: Tell the LLM "agent_1 is busy, try again later."

Recommendation: **Queue with a small buffer** (e.g., 1 pending message). If the queue is full, return an error. This lets the LLM pipeline a follow-up message without waiting for the first reply.

```go
func (m *SubAgentManager) Send(agentID, message string) error {
    // ... find agent ...
    agent.mu.Lock()
    if agent.busy {
        if agent.pending != "" {
            agent.mu.Unlock()
            return fmt.Errorf("agent %s: already has a pending message", agentID)
        }
        agent.pending = message
        agent.mu.Unlock()
        return nil // queued, will be sent when current processing done
    }
    agent.busy = true
    agent.mu.Unlock()
    // ... start goroutine for Continue ...
}
```

### State Transitions

```
                    launch_agent
                         │
                         ▼
                   ┌──────────┐
                   │  idle    │◄─────────────────┐
                   │ (ready)  │                  │
                   └────┬─────┘                  │
                        │ send_message            │
                        │ or pending dequeued     │
                        ▼                         │
                   ┌──────────┐                   │
         ┌────────►│  busy    │──────┐            │
         │         │(processing)     │            │
         │         └──────────┘      │ onExit     │
         │                │          │            │
    kill_agent            │          ▼            │
         │                │    ┌──────────┐       │
         │                └───►│  done    │───────┘
         │                     │(result   │   new send_message
         └────────────────────►│ available)│   (if not killed)
                               └──────────┘
```

### LLM Prompt Updates

Add to the base prompt (`internal/prompt/base.md`):

```markdown
## Sub-Agent Async Model

When you use `launch_agent`, the sub-agent runs in the background. You will
NOT receive its result immediately. Instead, the system will automatically
notify you when the sub-agent completes, carrying its final result.

Similarly, `send_message` delivers your message asynchronously. The sub-agent's
reply will arrive via notification.

While sub-agents run, you can:
- Continue answering the user's other questions
- Launch more sub-agents
- Run other tools

Do NOT call `agent_status` to poll for results. Only use `agent_status` if:
- The user explicitly asks for a status update
- You need to check progress mid-run before deciding next steps

Notifications appear as system messages in your context, prefixed with
`[agent <id>]`. You can reference the agent id in follow-up `send_message`
calls.
```

## Implementation Plan

### Phase 1: Core Infrastructure

1. **Create `internal/tools/subagent_manager.go`**
   - `SubAgentManager` struct and methods
   - `asyncSubAgent` internal state machine
   - `SubAgentNotification` and `SubAgentInfo` types
   - Unit tests with mock spawner

2. **Modify `internal/tools/launch_agent.go`**
   - Add `mgr *SubAgentManager` field to `LaunchAgentTool`
   - Change `Execute` to always call `mgr.Start()`
   - Update `Definition` description for async semantics
   - Update tests

3. **Modify `internal/tools/send_message.go`**
   - Add `mgr *SubAgentManager` field to `SendMessageTool`
   - Change `Execute` to call `mgr.Send()`
   - Update `Definition` description
   - Update tests

### Phase 2: New Tools

4. **Create `internal/tools/agent_status.go`**
   - `AgentStatusTool` implementation
   - Tests

5. **Create `internal/tools/kill_agent.go`**
   - `KillAgentTool` implementation
   - Tests

### Phase 3: Integration

6. **Modify `cmd/octo/chat.go` (or REPL setup)**
   - Initialize `SubAgentManager` with the real spawner
   - Wire `onExit` hook to inject notifications into conversation
   - Pass manager to tool constructors

7. **Modify `internal/tools/registry.go`**
   - Register new tools
   - Wire `SubAgentManager` into `LaunchAgentTool` and `SendMessageTool`

8. **Update `internal/prompt/base.md`**
   - Add sub-agent async model section

### Phase 4: Validation

9. **Integration tests**
   - Launch async sub-agent, verify immediate return
   - Verify notification delivery on completion
   - Send message to running sub-agent, verify async reply
   - Kill sub-agent mid-run

10. **Manual REPL testing**
    - Launch multiple sub-agents in one batch
    - Interact with user while sub-agents run
    - Verify notifications appear in subsequent turns

## Backwards Compatibility

This is a **breaking change** to `launch_agent` and `send_message` semantics:

- Existing prompts that expect synchronous sub-agent results will break.
- The tool schema changes (no `background` param, but behavior changes).
- The LLM must be re-prompted with the async model.

Mitigation:
- Clear documentation in the prompt.
- The `agent_status` tool provides an escape hatch for code that needs to wait.

## Open Questions

1. **Notification auto-trigger**: Should the REPL automatically start a new LLM turn when a notification arrives while idle? (Currently: no, use inbox queue.)
2. **Message queue depth**: How many pending messages per sub-agent? (Currently: 1.)
3. **Sub-agent timeout**: Should async sub-agents have a global timeout? (Currently: no, rely on spawner/implementation.)
4. **Result retention**: How long to keep sub-agent results after delivery? (Currently: until session end or `kill_agent`.)

## Related Work

- `internal/tools/background.go` — `BackgroundManager` pattern (direct model).
- `internal/taskgraph/` — DAG scheduler for `octo task` (separate concern, but shares the async execution concept).
- `dev-docs/tui-input-modes-design.md` — inbox mechanism for mid-turn injection.
