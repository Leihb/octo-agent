package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// EditFileTool replaces an exact substring inside an existing file. The
// match must be unique unless replace_all is true. Refuses to create the
// file if it doesn't exist — use write_file for that.
type EditFileTool struct{}

func (EditFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "edit_file",
		Description: "Replace an exact substring in an existing file. old_string must " +
			"appear exactly once (or set replace_all=true to swap every occurrence). " +
			"The file must already exist — use write_file to create. Preserve " +
			"indentation and surrounding context when picking old_string so it stays unique.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (absolute preferred).",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "Exact text to find. Must appear in the file. Include enough surrounding context for it to be unique unless replace_all is set.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "Replacement text. Empty string is allowed (deletes old_string).",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "When true, replace every occurrence instead of requiring a unique match. Defaults to false.",
				},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

func (EditFileTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	path, _ := input["path"].(string)
	if strings.TrimSpace(path) == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: path is required")
	}
	oldStr, ok1 := input["old_string"].(string)
	newStr, ok2 := input["new_string"].(string)
	if !ok1 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string is required")
	}
	if !ok2 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: new_string is required (use empty string to delete)")
	}
	if oldStr == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string must be non-empty")
	}
	if oldStr == newStr {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string and new_string are identical — nothing to do")
	}
	replaceAll, _ := input["replace_all"].(bool)

	abs, err := resolvePath(path)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: read %q: %w", path, err)
	}
	body := string(data)

	// CRLF handling: an LLM that read the file via read_file (which uses
	// bufio.Scanner — strips `\r` from `\r\n` lines) and then copies a
	// substring back into old_string would compare against `\n`-terminated
	// lines, but the on-disk file may have `\r\n`. Match in normalized
	// (LF) space; if the original was CRLF, restore on write so the file's
	// line-ending convention isn't silently flipped.
	hasCRLF := strings.Contains(body, "\r\n")
	bodyForMatch := body
	oldForMatch := oldStr
	newForReplace := newStr
	if hasCRLF {
		bodyForMatch = strings.ReplaceAll(body, "\r\n", "\n")
		oldForMatch = strings.ReplaceAll(oldStr, "\r\n", "\n")
		newForReplace = strings.ReplaceAll(newStr, "\r\n", "\n")
	}

	count := strings.Count(bodyForMatch, oldForMatch)
	if count == 0 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if count > 1 && !replaceAll {
		return agent.ToolResult{Text: ""}, fmt.Errorf(
			"edit_file: old_string matches %d times — either include more context to make it unique, or set replace_all=true",
			count,
		)
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(bodyForMatch, oldForMatch, newForReplace)
	} else {
		updated = strings.Replace(bodyForMatch, oldForMatch, newForReplace, 1)
	}
	if hasCRLF {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}

	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: write %q: %w", path, err)
	}

	if replaceAll {
		return agent.ToolResult{Text: fmt.Sprintf("Replaced %d occurrence(s) in %s", count, abs)}, nil
	}
	return agent.ToolResult{Text: fmt.Sprintf("Replaced 1 occurrence in %s", abs)}, nil
}
