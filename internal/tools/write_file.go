package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// WriteFileTool writes (or overwrites) a file with the given content. Parent
// directories are created with mkdir -p semantics. File permissions default
// to 0644.
type WriteFileTool struct{}

func (WriteFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "write_file",
		Description: "Write (or overwrite) a file. Parent directories are created " +
			"automatically. Use edit_file for partial edits — write_file replaces " +
			"the whole file. Absolute paths are preferred.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (absolute preferred).",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full file content to write. Empty string is allowed.",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (WriteFileTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	path, _ := input["path"].(string)
	if strings.TrimSpace(path) == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("write_file: path is required")
	}
	content, ok := input["content"].(string)
	if !ok {
		// Distinguish "not provided" from "empty string". JSON null → not a string.
		return agent.ToolResult{Text: ""}, fmt.Errorf("write_file: content is required (string)")
	}
	if secret := scanForSecrets(content); secret != "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("write_file: refusing to write content that contains a %s. "+
			"If this is genuinely intended (e.g. a test fixture), remove the live-credential "+
			"shape or create the file outside the agent.", secret)
	}
	abs, err := resolvePath(path)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("write_file: mkdir parent: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("write_file: write %q: %w", path, err)
	}

	lineCount := strings.Count(content, "\n")
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		lineCount++
	}
	return agent.ToolResult{Text: fmt.Sprintf("Wrote %d bytes (%d lines) to %s", len(content), lineCount, abs)}, nil
}
