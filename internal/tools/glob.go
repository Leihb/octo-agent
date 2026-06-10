package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools/rgembed"
)

// GlobMaxResults caps the number of paths a single glob call returns. The
// LLM rarely needs more than this, and a missing cap could surface tens of
// thousands of paths on large repos (node_modules, vendor/) and blow up
// context.
const GlobMaxResults = 200

// globNoiseDirs are directory names glob always excludes, on top of whatever
// .gitignore already removes. They're high-noise and almost never the user's
// intent; keeping the explicit list means glob behaves the same in a repo that
// hasn't gitignored them (or isn't a git repo at all).
var globNoiseDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, ".venv": {},
}

// GlobTool returns paths matching a glob pattern, sorted by modification time
// descending so the most recently touched files appear first.
//
// File enumeration is delegated to ripgrep (`rg --files`), which respects
// .gitignore / .ignore and prunes ignored subtrees efficiently — the same
// engine grep uses, so the two tools agree on what's "in" the project. The
// glob pattern itself is then matched in-process against each path so the
// semantics below are exactly preserved regardless of ripgrep's own glob rules.
//
// Supports `**` for "any directory depth" (e.g. `src/**/*.go`). Other
// segments use the standard path.Match semantics: `*` matches any run of
// non-separator characters, `?` matches a single character, character
// classes via `[…]`.
type GlobTool struct{}

func (GlobTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "glob",
		Description: "Find files matching a glob pattern. Supports `**` for any " +
			"directory depth (e.g. `src/**/*.go`). Returns up to 200 paths sorted " +
			"by modification time descending (most recently changed first). " +
			"Respects .gitignore (uses ripgrep to enumerate), and always skips " +
			"`.git`, `node_modules`, `vendor`, and `.venv`. To see ignored files, " +
			"point grep/read_file at an explicit path instead. Requires `rg`.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern. Use `**` for recursive matching, e.g. `**/*.go`.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Starting directory. Defaults to the current working directory.",
				},
			},
			"required": []string{"pattern"},
		},
	}
}

func (GlobTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	pattern, _ := input["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("glob: pattern is required")
	}

	root := "."
	if p, ok := input["path"].(string); ok && p != "" {
		root = p
	}
	absRoot, err := resolvePath(root)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	files, err := listProjectFiles(ctx, absRoot)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("glob: %w", err)
	}

	type match struct {
		path  string
		mtime int64
	}
	var matches []match

	for _, p := range files {
		rel, relErr := filepath.Rel(absRoot, p)
		if relErr != nil {
			rel = p
		}
		if hasNoiseDirSegment(rel) {
			continue
		}
		ok, mErr := globMatch(pattern, rel)
		if mErr != nil {
			return agent.ToolResult{Text: ""}, fmt.Errorf("glob: bad pattern %q: %w", pattern, mErr)
		}
		if !ok {
			continue
		}
		var mtime int64
		if info, infoErr := os.Stat(p); infoErr == nil {
			mtime = info.ModTime().UnixNano()
		}
		matches = append(matches, match{path: p, mtime: mtime})
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].mtime > matches[j].mtime })

	totalMatches := len(matches)
	truncated := false
	if totalMatches > GlobMaxResults {
		matches = matches[:GlobMaxResults]
		truncated = true
	}

	var out strings.Builder
	for _, m := range matches {
		out.WriteString(m.path)
		out.WriteByte('\n')
	}

	// UI payload: entries are relative to the search root to keep the card
	// (and the session JSON it persists in) compact.
	entries := make([]map[string]any, 0, min(len(matches), 20))
	for _, m := range matches[:min(len(matches), 20)] {
		name := m.path
		if rel, relErr := filepath.Rel(absRoot, m.path); relErr == nil {
			name = rel
		}
		entries = append(entries, map[string]any{"name": name})
	}
	ui := map[string]any{
		"type":    "file_list",
		"path":    absRoot,
		"entries": entries,
		"total":   totalMatches,
	}

	if out.Len() == 0 {
		return agent.ToolResult{Text: fmt.Sprintf("(no matches for %q under %s)", pattern, absRoot), UI: ui}, nil
	}
	if truncated {
		fmt.Fprintf(&out, "\n[truncated to first %d of %d matches]\n", GlobMaxResults, totalMatches)
	}
	return agent.ToolResult{Text: out.String(), UI: ui}, nil
}

// listProjectFiles enumerates every file under root that ripgrep would search,
// as absolute paths. `--files` lists candidates respecting .gitignore/.ignore
// and ripgrep's built-in VCS pruning; `--hidden` re-includes dotfiles so glob
// can still find e.g. `.octorules`; `--null` separates paths with NUL so a
// newline inside a filename can't split one path into two. A clean "no files"
// (ripgrep exit code 1) returns an empty slice, not an error.
func listProjectFiles(ctx context.Context, root string) ([]string, error) {
	rgPath, err := rgembed.Path()
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, rgPath, "--files", "--hidden", "--null", root).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil // no files under root
		}
		return nil, fmt.Errorf("rg --files: %w", err)
	}
	trimmed := strings.Trim(string(out), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\x00"), nil
}

// hasNoiseDirSegment reports whether any path segment of rel names a directory
// in globNoiseDirs. Applied after ripgrep so the exclusion holds even when the
// project hasn't gitignored these dirs.
func hasNoiseDirSegment(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if _, ok := globNoiseDirs[seg]; ok {
			return true
		}
	}
	return false
}

// globMatch matches a pattern against a relative path, supporting `**`
// across path separators (which path.Match by itself does not).
//
// Strategy: split pattern on `/`, then walk path segments and pattern
// segments in lockstep. When a `**` segment is encountered, recurse over
// every possible tail-position the rest of the pattern could match at.
func globMatch(pattern, path string) (bool, error) {
	// Normalise to forward slashes so the same patterns work cross-platform.
	pp := strings.Split(filepath.ToSlash(pattern), "/")
	sp := strings.Split(filepath.ToSlash(path), "/")
	return matchSegments(pp, sp)
}

func matchSegments(pp, sp []string) (bool, error) {
	for i, pat := range pp {
		if pat == "**" {
			// `**` matches zero or more path segments. Try every tail.
			rest := pp[i+1:]
			if len(rest) == 0 {
				return true, nil
			}
			for j := 0; j <= len(sp); j++ {
				ok, err := matchSegments(rest, sp[j:])
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}
		if len(sp) == 0 {
			return false, nil
		}
		ok, err := filepath.Match(pat, sp[0])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		sp = sp[1:]
	}
	return len(sp) == 0, nil
}
