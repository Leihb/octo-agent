// Package tools provides built-in tool implementations for the octo agentic
// loop. Each tool implements agent.ToolExecutor and exposes a Definition()
// method that returns the agent.ToolDefinition the LLM sees.
package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// TerminalTimeout is the maximum time a single terminal command may run.
const TerminalTimeout = 30 * time.Second

// TerminalTool is an agent.ToolExecutor that runs shell commands via `sh -c`.
// Stdout and stderr are combined and returned as the tool result. Non-zero
// exit codes are reported as extra metadata in the result text rather than
// as a tool error, so the LLM can see the failure output and adapt.
//
// The LLM-facing tool name is "terminal" — calling it "bash" would imply a
// hard /bin/bash dependency, but the executor actually shells out via
// `sh -c`.
type TerminalTool struct{}

// Definition returns the agent.ToolDefinition the LLM receives in the tools
// list. The JSON Schema describes a single required "command" string parameter.
func (TerminalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "terminal",
		Description: "Run a shell command (via `sh -c`) and return stdout+stderr. Use for file operations, running programs, searching code, etc.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
				},
			},
			"required": []string{"command"},
		},
	}
}

// Execute runs the command and returns combined output. A non-zero exit code
// is appended to the output as `[exit: <error>]` rather than being surfaced
// as an error, giving the LLM visibility into what went wrong.
func (TerminalTool) Execute(ctx context.Context, _ string, input map[string]any) (string, error) {
	command, _ := input["command"].(string)
	if command == "" {
		return "", fmt.Errorf("terminal: command is required")
	}

	ctx, cancel := context.WithTimeout(ctx, TerminalTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Surface as result text (not a tool-level error) so the LLM sees it.
		return strings.TrimSpace(string(out)) + "\n[exit: " + err.Error() + "]", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultRegistry is a simple ToolExecutor registry that contains only
// TerminalTool. It is the default executor used when --tools is enabled.
type DefaultRegistry struct{}

// Execute dispatches to TerminalTool for "terminal" and returns an error for
// unknown tools so the LLM receives a clean error result.
func (DefaultRegistry) Execute(ctx context.Context, name string, input map[string]any) (string, error) {
	switch name {
	case "terminal":
		return TerminalTool{}.Execute(ctx, name, input)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

// DefaultTools returns the set of tool definitions that are active when
// --tools is enabled. Currently this is just the terminal tool.
func DefaultTools() []agent.ToolDefinition {
	return []agent.ToolDefinition{TerminalTool{}.Definition()}
}
