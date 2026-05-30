package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// GlobMaxResults caps the number of paths a single glob call returns. The
// LLM rarely needs more than this, and a missing cap could surface tens of
// thousands of paths on large repos (node_modules, vendor/) and blow up
// context.
const GlobMaxResults = 200

// GlobTool walks a directory tree and returns paths matching a glob
// pattern, sorted by modification time descending so the most recently
// touched files appear first.
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
			"Skips `.git`, `node_modules`, `vendor`, and `.venv` at the directory " +
			"level — pass an explicit path under those if you need to look there.",
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

func (GlobTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
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

	type match struct {
		path  string
		mtime int64
	}
	var matches []match

	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip permission-denied subtrees rather than aborting the whole walk.
			if os.IsPermission(walkErr) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			// Skip a few high-noise directories that are almost never the
			// user's intent. Saves walk time and result-list space.
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(absRoot, p)
		if relErr != nil {
			rel = p
		}
		ok, mErr := globMatch(pattern, rel)
		if mErr != nil {
			return mErr
		}
		if !ok {
			return nil
		}
		info, infoErr := d.Info()
		var mtime int64
		if infoErr == nil {
			mtime = info.ModTime().UnixNano()
		}
		matches = append(matches, match{path: p, mtime: mtime})
		return nil
	})
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("glob: walk %q: %w", root, err)
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
	if out.Len() == 0 {
		return agent.ToolResult{Text: fmt.Sprintf("(no matches for %q under %s)", pattern, absRoot)}, nil
	}
	if truncated {
		fmt.Fprintf(&out, "\n[truncated to first %d of %d matches]\n", GlobMaxResults, totalMatches)
	}
	return agent.ToolResult{Text: out.String()}, nil
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
