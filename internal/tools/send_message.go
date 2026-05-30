package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// SendMessageTool sends a message to a sub-agent previously started with
// launch_agent. The message is delivered asynchronously; the reply arrives via
// a system notification when the sub-agent responds.
type SendMessageTool struct {
	mgr *SubAgentManager
}

func (t SendMessageTool) manager() *SubAgentManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultSubAgentMgr
}

func (SendMessageTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "send_message",
		Description: "Send a message to a sub-agent you previously started with launch_agent. " +
			"The message is delivered asynchronously; you will receive the reply via " +
			"a system notification when the sub-agent responds. " +
			"The sub-agent remembers everything it did before. " +
			"The agent_id comes from the launch_agent result. " +
			"If the agent is no longer alive, start a new one with launch_agent.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Id of the sub-agent to message, as returned by launch_agent (e.g. 'agent_1').",
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

func (t SendMessageTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		// Symmetric with launch_agent's recursion guard: a sub-agent must not
		// be able to wake another sub-agent. Defense in depth on top of the
		// Spawner dropping send_message from every child's tool list.
		return agent.ToolResult{Text: ""}, fmt.Errorf("send_message: a sub-agent cannot message another sub-agent")
	}

	agentID := strings.TrimSpace(stringArg(input, "agent_id"))
	message := strings.TrimSpace(stringArg(input, "message"))
	if agentID == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("send_message: agent_id is required")
	}
	if message == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("send_message: message is required")
	}

	mgr := t.manager()
	if mgr == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("send_message: sub-agent dispatch is not configured for this session")
	}

	if err := mgr.Send(agentID, message); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("send_message: %w", err)
	}
	return agent.ToolResult{
		Text: fmt.Sprintf("Message sent to %s. You will be notified when it replies.", agentID),
	}, nil
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
