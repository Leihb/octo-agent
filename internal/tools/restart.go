package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// activeRestarter, when non-nil, backs the restart_server tool and gates its
// advertisement in DefaultToolsFor. The server registers it (the restart
// goes through Server.Restart's drain); it stays nil in CLI/TUI/sub-agent
// processes, where there is no supervisor contract to honour.
var activeRestarter func(reason string)

// SetRestarter registers the function the restart_server tool delegates to.
// Pass nil to disable (the tool then doesn't appear in DefaultToolsFor).
// Mirrors SetAsker: process-global, set once at server start.
func SetRestarter(f func(reason string)) { activeRestarter = f }

func restarterEnabled() bool { return activeRestarter != nil }

// RestartServerTool lets the model restart the octo server process — after a
// binary upgrade or a config change that is only read at startup. The
// restart is scheduled, not immediate: the server drains in-flight turns
// (including the one executing this tool), so the model's final reply still
// reaches the user before the process exits and the supervisor respawns it.
type RestartServerTool struct{}

func (RestartServerTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "restart_server",
		Description: "Schedule a graceful restart of the octo server process. Use after replacing the " +
			"octo binary (upgrade) or changing configuration that is only read at startup " +
			"(provider, model, system prompt, channels.yml). The restart happens AFTER the " +
			"current turn completes: in-flight turns drain first, then the supervisor " +
			"respawns the server from the binary on disk. Tell the user the server is " +
			"restarting in your reply — it will be briefly unreachable, and web/IM clients " +
			"reconnect automatically.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": "Short operator-facing reason for the restart, e.g. 'binary upgraded to 0.18.0' or 'config change: switched default model'. Logged on the server console.",
				},
			},
			"required": []string{"reason"},
		},
	}
}

func (RestartServerTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if !restarterEnabled() {
		return agent.ToolResult{}, fmt.Errorf("restart_server: not available in this mode (server only)")
	}
	reason := strings.TrimSpace(stringArg(input, "reason"))
	if reason == "" {
		return agent.ToolResult{}, fmt.Errorf("restart_server: reason is required")
	}

	activeRestarter(reason)

	return agent.ToolResult{
		Text: "Restart scheduled: the server will drain in-flight turns and restart after this turn completes. " +
			"Clients reconnect automatically once the new process is up.",
		UI: map[string]any{"type": "restart", "reason": reason},
	}, nil
}
