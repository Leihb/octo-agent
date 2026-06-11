package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// Follow-up tools for sub-agents: send a message to a running/completed
// child, read its status, kill it. They complete the sub_agent surface —
// the SubAgentManager always had Send/ContinueSync/Read/Kill, but until
// these tools nothing exposed them to the model, so an async child could
// only be awaited, never steered or queried.
//
// Two ID namespaces converge here, matching what the model actually sees:
//   - async spawns return "agent_N" (manager-tracked) — Send delivers
//     asynchronously and the reply arrives as a notification;
//   - sync spawns tag their reply "[agent <id>]" (spawner-side) — those
//     continue synchronously and return the reply inline.
// sub_agent_send tries the manager first and falls back to a synchronous
// continue, so the model can use whichever ID it has.

// AgentSendTool delivers a follow-up message to an existing sub-agent.
type AgentSendTool struct{}

func (AgentSendTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "sub_agent_send",
		Description: "Send a follow-up message to an existing sub-agent — steer it, ask for more " +
			"detail, or continue its task with its context intact. Accepts either ID form: " +
			"an async sub-agent's id (agent_N, from sub_agent with run_in_background) gets " +
			"the message asynchronously and replies via a notification; a sync sub-agent's " +
			"id (the [agent …] tag on its reply) is continued synchronously and the reply " +
			"returns here directly.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "The sub-agent to message: agent_N for async sub-agents, or the [agent …] tag id from a synchronous sub-agent's reply.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "The follow-up instruction or question. The sub-agent keeps its previous context.",
				},
			},
			"required": []string{"agent_id", "message"},
		},
	}
}

func (AgentSendTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_send: a sub-agent cannot message other sub-agents")
	}
	mgr := resolveSubAgentManager(ctx, nil)
	if mgr == nil {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_send: sub-agent dispatch is not configured for this session")
	}
	id := strings.TrimSpace(stringArg(input, "agent_id"))
	msg := strings.TrimSpace(stringArg(input, "message"))
	if id == "" || msg == "" {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_send: agent_id and message are required")
	}

	err := mgr.Send(id, msg)
	switch {
	case err == nil:
		return agent.ToolResult{
			Text: fmt.Sprintf("Message delivered to %s (queued if it was busy). Its reply will arrive as a notification.", id),
		}, nil
	case strings.Contains(err.Error(), "no sub-agent"):
		// Not manager-tracked — treat the id as a spawner-side (sync) child
		// and continue it synchronously.
		res, cerr := mgr.ContinueSync(ctx, id, msg)
		if cerr != nil {
			return agent.ToolResult{}, fmt.Errorf("sub_agent_send: unknown sub-agent %q (and synchronous continue failed: %v)", id, cerr)
		}
		text := withAgentTag(res.AgentID, res.Reply)
		if res.StopReason == "max_turns" {
			text += "\n\n[INCOMPLETE: this sub-agent hit its turn limit — the result above is partial.]"
		}
		return agent.ToolResult{Text: text}, nil
	default:
		// Exited / pending-message errors are real answers, not routing misses.
		return agent.ToolResult{}, fmt.Errorf("sub_agent_send: %w", err)
	}
}

// AgentStatusTool reports one sub-agent's state or lists the running set.
type AgentStatusTool struct{}

func (AgentStatusTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "sub_agent_status",
		Description: "Check on async sub-agents: with agent_id, report that sub-agent's state " +
			"(running/done/exited) and its latest result; without agent_id, list all currently " +
			"running sub-agents. Synchronous sub-agents return their result inline at spawn " +
			"and are not tracked here.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Optional async sub-agent id (agent_N). Omit to list everything still running.",
				},
			},
		},
	}
}

func (AgentStatusTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	mgr := resolveSubAgentManager(ctx, nil)
	if mgr == nil {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_status: sub-agent dispatch is not configured for this session")
	}

	id := strings.TrimSpace(stringArg(input, "agent_id"))
	if id == "" {
		infos := mgr.ListRunning()
		if len(infos) == 0 {
			return agent.ToolResult{Text: "No sub-agents are currently running."}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d sub-agent(s) running:\n", len(infos))
		for _, in := range infos {
			state := "idle"
			if in.Busy {
				state = "busy"
			}
			fmt.Fprintf(&b, "- %s — %s (%s, started %s ago)\n",
				in.ID, in.Description, state, time.Since(in.Start).Round(time.Second))
		}
		return agent.ToolResult{Text: strings.TrimRight(b.String(), "\n")}, nil
	}

	result, status, found := mgr.Read(id)
	if !found {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_status: unknown sub-agent %q (only async sub-agents are tracked)", id)
	}
	text := fmt.Sprintf("Sub-agent %s: %s", id, status)
	if result != "" {
		text += "\n\nLatest result:\n" + result
	} else {
		text += "\n\n(no result yet)"
	}
	return agent.ToolResult{Text: text}, nil
}

// AgentKillTool terminates an async sub-agent.
type AgentKillTool struct{}

func (AgentKillTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "sub_agent_kill",
		Description: "Terminate a running async sub-agent (agent_N). Use when its task is no " +
			"longer needed or it's clearly stuck. The kill is immediate; partial results " +
			"already reported stay available via sub_agent_status.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "The async sub-agent id (agent_N) to terminate.",
				},
			},
			"required": []string{"agent_id"},
		},
	}
}

func (AgentKillTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_kill: a sub-agent cannot kill other sub-agents")
	}
	mgr := resolveSubAgentManager(ctx, nil)
	if mgr == nil {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_kill: sub-agent dispatch is not configured for this session")
	}
	id := strings.TrimSpace(stringArg(input, "agent_id"))
	if id == "" {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_kill: agent_id is required")
	}
	if !mgr.Kill(id) {
		return agent.ToolResult{}, fmt.Errorf("sub_agent_kill: unknown sub-agent %q", id)
	}
	return agent.ToolResult{Text: fmt.Sprintf("Killed sub-agent %s.", id)}, nil
}
