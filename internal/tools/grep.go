package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools/rgembed"
)

// GrepMaxLines caps how many output lines a single grep call returns.
// Without it a broad pattern (e.g. `func`) on a large repo floods the LLM
// context with thousands of hits. Past the cap the output is truncated
// with a marker naming the total, so the model narrows the pattern instead
// of assuming it saw everything. Mirrors GlobMaxResults' role for glob.
const GrepMaxLines = 200

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
			"Respects .gitignore. Returns at most 200 output lines — narrow the " +
			"pattern or set include/path if you hit the cap. Matching lines over " +
			"500 chars are truncated with a preview of the first 500 bytes. " +
			"Output includes line numbers. " +
			"Requires `rg` on PATH.",
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
	rgPath, err := rgembed.Path()
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("grep: %w", err)
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
	// chars. Without this, a hit on a minified bundle / base64 blob can flood
	// the LLM's context with one line. --max-columns-preview shows the first
	// 500 bytes instead of replacing the entire line with an unhelpful
	// "[Omitted long matching line]" marker, which previously caused the LLM
	// to think the result was incomplete and retry in a loop.
	args := []string{"--color=never", "--line-number", "--max-columns", "500", "--max-columns-preview"}
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
	hasContext := false
	if mode == "content" {
		if c := intArg(input, "context_lines", 0); c > 0 {
			args = append(args, "-C", strconv.Itoa(c))
			hasContext = true
		}
		if b := intArg(input, "before", 0); b > 0 {
			args = append(args, "-B", strconv.Itoa(b))
			hasContext = true
		}
		if a := intArg(input, "after", 0); a > 0 {
			args = append(args, "-A", strconv.Itoa(a))
			hasContext = true
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

	out, err := exec.CommandContext(ctx, rgPath, args...).Output()
	if err != nil {
		// ripgrep exits 1 when nothing matched. That's not an error from
		// the LLM's perspective — surface it as a normal result.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return agent.ToolResult{Text: "(no matches)", UI: grepUIEmpty(pattern)}, nil
		}
		return agent.ToolResult{Text: ""}, fmt.Errorf("grep: rg failed: %w", err)
	}
	if len(out) == 0 {
		return agent.ToolResult{Text: "(no matches)", UI: grepUIEmpty(pattern)}, nil
	}

	text := strings.TrimRight(string(out), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > GrepMaxLines {
		// Truncate and tell the model the total so it doesn't mistake a
		// capped result for the complete set and re-run the same search.
		// "lines" not "matches" — in content mode context lines and `--`
		// separators count too.
		kept := strings.Join(lines[:GrepMaxLines], "\n")
		return agent.ToolResult{
			Text: fmt.Sprintf(
				"%s\n\n[truncated to first %d of %d lines — narrow the pattern, add include='*.ext', or set a more specific path]",
				kept, GrepMaxLines, len(lines)),
			UI: grepUI(pattern, mode, hasContext, kept),
		}, nil
	}
	return agent.ToolResult{Text: text, UI: grepUI(pattern, mode, hasContext, text)}, nil
}

// grepUIMatchRe splits an rg content line "path:lineNo:content". The
// non-greedy path group still survives Windows drive prefixes ("C:\…")
// because the engine settles on the first ":<digits>:" boundary.
var grepUIMatchRe = regexp.MustCompile(`^(.+?):(\d+):(.*)$`)

// grepUIEmpty is the zero-match "search" payload.
func grepUIEmpty(pattern string) map[string]any {
	return map[string]any{
		"type":               "search",
		"pattern":            pattern,
		"matches":            []map[string]any{},
		"total_matches":      0,
		"files_with_matches": 0,
	}
}

// grepUI builds the "search" UI payload from rg content-mode output.
// Context runs and non-content modes produce line shapes that would
// mis-parse, so they get no payload (the raw text still renders). Returns
// nil too when nothing parses — e.g. a single-file search, where rg omits
// the leading path. The return type is `any` (not the map type) so a nil
// result stays a true nil inside ToolResult.UI.
func grepUI(pattern, mode string, hasContext bool, text string) any {
	if mode != "content" || hasContext {
		return nil
	}
	var matches []map[string]any
	files := make(map[string]struct{})
	total := 0
	for _, line := range strings.Split(text, "\n") {
		m := grepUIMatchRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		total++
		files[m[1]] = struct{}{}
		if len(matches) < 10 {
			lineNo, _ := strconv.Atoi(m[2])
			matches = append(matches, map[string]any{
				"file":    m[1],
				"line_no": lineNo,
				"line":    uiHead(strings.TrimSpace(m[3]), 1, 200),
			})
		}
	}
	if total == 0 {
		return nil
	}
	return map[string]any{
		"type":               "search",
		"pattern":            pattern,
		"matches":            matches,
		"total_matches":      total,
		"files_with_matches": len(files),
	}
}
