package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// SendMessageTool continues a sub-agent previously started with launch_agent,
// sending it a new message and returning its next reply. Only the top-level
// parent agent gets this tool — the Spawner filters it out of every child's
// toolbelt (same rule as launch_agent), so a sub-agent can't message another.
type SendMessageTool struct{}

func (SendMessageTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "send_message",
		Description: "Continue a sub-agent you previously started with launch_agent by sending " +
			"it a new message, and get its next reply. The sub-agent remembers everything it did " +
			"before — use this when a delegated sub-task needs multiple rounds, or to give a " +
			"follow-up instruction based on the sub-agent's earlier reply (instead of launching a " +
			"fresh one that starts from scratch). The agent_id comes from the launch_agent result " +
			"(the value in its '[agent <id>]' tag). If the reply says the agent is no longer alive, " +
			"start a new one with launch_agent.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Id of the sub-agent to continue, as returned by launch_agent (the value in its '[agent <id>]' tag).",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "The new message for the sub-agent. It can't see this conversation, so keep it self-contained.",
				},
			},
			"required": []string{"agent_id", "message"},
		},
	}
}

func (SendMessageTool) Execute(ctx context.Context, _ string, input map[string]any) (string, error) {
	if !spawnerEnabled() {
		return "", fmt.Errorf("send_message: sub-agent dispatch is not configured for this session")
	}
	if IsSubAgent(ctx) {
		// Symmetric with launch_agent's recursion guard: a sub-agent must not
		// be able to wake another sub-agent. Defense in depth on top of the
		// Spawner dropping send_message from every child's tool list.
		return "", fmt.Errorf("send_message: a sub-agent cannot message another sub-agent")
	}

	agentID := strings.TrimSpace(stringArg(input, "agent_id"))
	message := strings.TrimSpace(stringArg(input, "message"))
	if agentID == "" {
		return "", fmt.Errorf("send_message: agent_id is required")
	}
	if message == "" {
		return "", fmt.Errorf("send_message: message is required")
	}

	res, err := activeSpawner.Continue(ctx, agentID, message)
	if err != nil {
		return "", fmt.Errorf("send_message: %w", err)
	}
	reply := strings.TrimSpace(res.Reply)
	if reply == "" {
		return "(sub-agent " + agentID + " produced no reply)", nil
	}
	return withAgentTag(res.AgentID, reply), nil
}

// withAgentTag prefixes a sub-agent reply with "[agent <id>] " so the parent
// model has a stable handle to address in a follow-up send_message. An empty
// id (continuation unsupported) yields the bare reply unchanged.
func withAgentTag(id, reply string) string {
	if id == "" {
		return reply
	}
	return "[agent " + id + "] " + reply
}
