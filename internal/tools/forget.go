package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// ForgetTool deletes a remembered fact by name — the inverse of RememberTool.
// It shares the package-level store (SetMemoryStore) and the memoryEnabled gate,
// so it's advertised only when memory is on. Only individual (not-yet-
// consolidated) entries are addressable by name; facts already folded into the
// summary live as prose and can't be forgotten this way.
type ForgetTool struct{}

func (ForgetTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "forget",
		Description: "Delete a remembered fact by its name. Names are shown in the " +
			"\"Memory (from past sessions)\" section of the system prompt and by /memory. " +
			"Use when the user asks to forget something, or a remembered fact has become " +
			"wrong or obsolete. Only individual (not-yet-consolidated) entries can be " +
			"forgotten by name.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The memory's name, exactly as shown in the memory list.",
				},
			},
			"required": []string{"name"},
		},
	}
}

func (ForgetTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	name := strings.TrimSpace(stringArg(input, "name"))
	if name == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("forget: name is required")
	}
	if !memoryEnabled() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("forget: memory is disabled for this session")
	}
	if err := activeMemory.Delete(name); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("forget: %w", err)
	}
	return agent.ToolResult{Text: "Forgot: " + name}, nil
}
