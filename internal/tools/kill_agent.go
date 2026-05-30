package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// KillAgentTool terminates a running sub-agent. The sub-agent's context is
// cancelled and it is marked as exited. Any pending message is dropped.
type KillAgentTool struct {
	mgr *SubAgentManager
}

func (t KillAgentTool) manager() *SubAgentManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultSubAgentMgr
}

func (KillAgentTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "kill_agent",
		Description: "Terminate a sub-agent you previously started with launch_agent. " +
			"Use this when a sub-agent is stuck, no longer needed, or you want to free resources. " +
			"The sub-agent's context is cancelled and it is marked as exited.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Id of the sub-agent to kill, as returned by launch_agent (e.g. 'agent_1').",
				},
			},
			"required": []string{"agent_id"},
		},
	}
}

func (t KillAgentTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_agent: a sub-agent cannot kill another sub-agent")
	}

	agentID := strings.TrimSpace(stringArg(input, "agent_id"))
	if agentID == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_agent: agent_id is required")
	}

	mgr := t.manager()
	if mgr == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_agent: sub-agent dispatch is not configured for this session")
	}

	if !mgr.Kill(agentID) {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_agent: no sub-agent %q", agentID)
	}
	return agent.ToolResult{
		Text: fmt.Sprintf("Sub-agent %s has been terminated.", agentID),
	}, nil
}
