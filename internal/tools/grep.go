package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// GrepTool is a thin wrapper over `ripgrep` (`rg`). It accepts a regex
// pattern and one of three output modes: full content lines, file paths
// only, or per-file match counts.
//
// We deliberately depend on a working `rg` binary rather than reimplement
// in pure Go. Ripgrep handles .gitignore, binary detection, and parallel
// I/O — duplicating those is months of work, and `rg` is a near-universal
// install on developer machines.
type GrepTool struct{}

func (GrepTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "grep",
		Description: "Search file contents with ripgrep (rg). Pattern is a regex. " +
			"Use mode='files_with_matches' for path-only output, mode='count' for " +
			"per-file counts, or the default mode='content' to see matching lines. " +
			"Set context_lines (or before/after) to include surrounding lines. " +
			"Respects .gitignore. Matching lines over 500 chars are truncated " +
			"with an [Omitted long matching line] marker. Requires `rg` on PATH.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Regex pattern (Rust regex syntax — same as ripgrep).",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Where to search. Defaults to the current working directory.",
				},
				"include": map[string]any{
					"type":        "string",
					"description": "Restrict to files matching this glob, e.g. '*.go' or 'src/**/*.ts'.",
				},
				"mode": map[string]any{
					"type":        "string",
					"description": "'content' (default) | 'files_with_matches' | 'count'.",
				},
				"context_lines": map[string]any{
					"type":        "integer",
					"description": "Lines of context around each match (sets both before and after). Only applies to mode='content'.",
				},
				"before": map[string]any{
					"type":        "integer",
					"description": "Lines BEFORE each match. Overrides context_lines on that side.",
				},
				"after": map[string]any{
					"type":        "integer",
					"description": "Lines AFTER each match. Overrides context_lines on that side.",
				},
				"case_insensitive": map[string]any{
					"type":        "boolean",
					"description": "Case-insensitive match (-i). Defaults to false.",
				},
			},
			"required": []string{"pattern"},
		},
	}
}

func (GrepTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf(
			"grep: ripgrep (`rg`) is not installed or not on PATH. " +
				"Install it from https://github.com/BurntSushi/ripgrep",
		)
	}

	pattern, _ := input["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("grep: pattern is required")
	}

	mode, _ := input["mode"].(string)
	if mode == "" {
		mode = "content"
	}

	// --max-columns 500 truncates any single matching line longer than 500
	// chars and emits a "[Omitted long matching line]" marker. Without this,
	// a hit on a minified bundle / base64 blob can flood the LLM's context
	// with one line. Matches the cap Claude Code's GrepTool uses.
	args := []string{"--color=never", "--max-columns", "500"}
	switch mode {
	case "content":
		// default rg output
	case "files_with_matches":
		args = append(args, "--files-with-matches")
	case "count":
		args = append(args, "--count-matches")
	default:
		return agent.ToolResult{Text: ""}, fmt.Errorf("grep: unknown mode %q (use content | files_with_matches | count)", mode)
	}

	if ci, _ := input["case_insensitive"].(bool); ci {
		args = append(args, "-i")
	}
	if inc, _ := input["include"].(string); inc != "" {
		args = append(args, "--glob", inc)
	}

	// Context lines only make sense for mode=content.
	if mode == "content" {
		if c := intArg(input, "context_lines", 0); c > 0 {
			args = append(args, "-C", strconv.Itoa(c))
		}
		if b := intArg(input, "before", 0); b > 0 {
			args = append(args, "-B", strconv.Itoa(b))
		}
		if a := intArg(input, "after", 0); a > 0 {
			args = append(args, "-A", strconv.Itoa(a))
		}
	}

	args = append(args, "--", pattern)
	if p, _ := input["path"].(string); p != "" {
		abs, err := resolvePath(p)
		if err != nil {
			return agent.ToolResult{Text: ""}, err
		}
		args = append(args, abs)
	}

	out, err := exec.CommandContext(ctx, "rg", args...).Output()
	if err != nil {
		// ripgrep exits 1 when nothing matched. That's not an error from
		// the LLM's perspective — surface it as a normal result.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return agent.ToolResult{Text: "(no matches)"}, nil
		}
		return agent.ToolResult{Text: ""}, fmt.Errorf("grep: rg failed: %w", err)
	}
	if len(out) == 0 {
		return agent.ToolResult{Text: "(no matches)"}, nil
	}
	return agent.ToolResult{Text: strings.TrimRight(string(out), "\n")}, nil
}
