package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// AgentStatusTool queries the state and latest result of a sub-agent previously
// started with launch_agent. It is synchronous — the result is returned
// immediately, not via notification.
type AgentStatusTool struct {
	mgr *SubAgentManager
}

func (AgentStatusTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "agent_status",
		Description: "Check the status and latest result of a sub-agent you started with launch_agent. " +
			"Returns whether it is running, idle, or exited, along with its most recent output. " +
			"Use this when you want to poll a sub-agent's progress or read its result without waiting for a notification.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Id of the sub-agent to query, as returned by launch_agent (e.g. 'agent_1').",
				},
			},
			"required": []string{"agent_id"},
		},
	}
}

func (t AgentStatusTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		return agent.ToolResult{Text: ""}, fmt.Errorf("agent_status: a sub-agent cannot query another sub-agent")
	}

	agentID := strings.TrimSpace(stringArg(input, "agent_id"))
	if agentID == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("agent_status: agent_id is required")
	}

	mgr := resolveSubAgentManager(ctx, t.mgr)
	if mgr == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("agent_status: sub-agent dispatch is not configured for this session")
	}

	result, status, found := mgr.Read(agentID)
	if !found {
		return agent.ToolResult{Text: ""}, fmt.Errorf("agent_status: no sub-agent %q", agentID)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Status: %s\n", status)
	if result != "" {
		fmt.Fprintf(&sb, "Latest result:\n%s", result)
	}
	return agent.ToolResult{Text: sb.String()}, nil
}
