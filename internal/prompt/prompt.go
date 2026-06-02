// Package prompt assembles the agent's system prompt from layered sources.
//
// The composed prompt is meant to be built ONCE per session and frozen: the
// provider caches the system+tools prefix (see internal/provider/anthropic),
// and recomputing the prompt mid-session would invalidate that cache for
// every subsequent turn. Compose at session start, set it on Agent.System,
// and don't touch it again.
package prompt

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// base is the built-in foundation prompt: octo's identity plus the
// tool-use, permission, and read-before-write norms. Embedded so it ships
// in the single binary.
//
//go:embed base.md
var base string

// coauthorRule is injected right after base when coauthor mode is enabled.
// It instructs the agent to append a Co-authored-by trailer to every git
// commit message it writes, following Git trailer format (RFC 822-style
// headers at the end of the message, separated by a blank line).
const coauthorRule = `## Git commits

When you run ` + "`git commit`" + ` (via the terminal tool or any other path), append the following Co-authored-by trailer to the commit message:

  Co-authored-by: octo-agent <no-reply@octo-agent.dev>

The trailer must be placed at the very end of the commit message, separated from the body by a blank line, following standard Git trailer format. Do not add this trailer if the user explicitly tells you not to.`

// ProjectContextFile is the per-repo conventions file Compose looks for in
// the working directory. It carries project-specific rules the agent should
// follow (the human-facing counterpart to CLAUDE.md).
const ProjectContextFile = ".octorules"

// maxIncludeDepth caps @include nesting so a deep (or accidentally recursive)
// chain can't blow the stack or balloon the prompt. Cycles are caught
// separately; this guards pathological-but-acyclic depth.
const maxIncludeDepth = 5

// userRulesPath returns the absolute path of the per-user global conventions
// file (~/.octo/octorules.md) — the cross-project counterpart of the per-repo
// ProjectContextFile. It's a var so tests can point it at a temp file. Returns
// "" when the home directory can't be resolved.
var userRulesPath = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".octo", "octorules.md")
}

// Compose assembles the session system prompt from up to nine layers, in
// order of increasing specificity:
//
//  1. base    — embedded octo foundation (always present)
//  2. soul    — ~/.octo/soul.md, if present (agent identity & behavior)
//  3. env     — environment snapshot (cwd, git, date, OS) the caller renders
//  4. skills  — the available-skills manifest the caller renders, if any
//  5. memory  — cross-session memory the caller renders, if any (C9)
//  6. profile — ~/.octo/user.md, if present (who the user is)
//  7. user    — ~/.octo/octorules.md, if present (cross-project user rules)
//  8. project — ProjectContextFile in cwd, if present (repo conventions)
//  9. system  — the --system value, if any (highest-priority override, last)
//
// Empty layers are skipped. Later layers appear later in the text, which is
// the conventional way to let more specific instructions take precedence —
// project rules override the user's global rules, and --system overrides all.
//
// soul sits right after base: it reshapes persona/behavior, but base's tool
// and safety norms still precede it. skills is the already-rendered manifest
// (see skills.RenderManifest), passed in rather than discovered here so this
// package keeps a one-directional dep (prompt does not import skills) and the
// prefix stays stable across turns. soul/profile/user/project are read here
// (single files), like octorules.
//
// The user and project files may pull in other files with @include directives
// (see expandIncludes). env is passed in rather than computed here so this
// package stays pure (no os/exec, no git); the caller snapshots it once at
// session start, which keeps the cached prompt prefix stable across turns.
//
// coauthor, when true, injects a rule into the system prompt instructing the
// agent to append a Co-authored-by trailer to every git commit message it writes.
func Compose(userSystem, cwd, env, skills, memory string, coauthor bool) string {
	layers := []string{strings.TrimSpace(base)}

	if coauthor {
		layers = append(layers, coauthorRule)
	}

	if s := readSoul(); s != "" {
		layers = append(layers, "# Agent identity (~/.octo/soul.md)\n\n"+s)
	}
	if e := strings.TrimSpace(env); e != "" {
		layers = append(layers, e)
	}
	if s := strings.TrimSpace(skills); s != "" {
		layers = append(layers, s)
	}
	if m := strings.TrimSpace(memory); m != "" {
		layers = append(layers, m)
	}
	if p := readUserProfile(); p != "" {
		layers = append(layers, "# User profile (~/.octo/user.md)\n\n"+p)
	}
	if u := readUserContext(); u != "" {
		layers = append(layers, "# User conventions (~/.octo/octorules.md)\n\n"+u)
	}
	if proj := readProjectContext(cwd); proj != "" {
		layers = append(layers, "# Project conventions ("+ProjectContextFile+")\n\n"+proj)
	}
	if u := strings.TrimSpace(userSystem); u != "" {
		layers = append(layers, u)
	}

	return strings.Join(layers, "\n\n---\n\n")
}

// soulPath and userProfilePath return the per-user identity files
// (~/.octo/soul.md, ~/.octo/user.md), or "" when the home dir can't be
// resolved. They're vars so tests can point them at temp files, mirroring
// userRulesPath.
var soulPath = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".octo", "soul.md")
}

var userProfilePath = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".octo", "user.md")
}

// readSoul returns the trimmed, include-expanded contents of ~/.octo/soul.md
// (agent identity & behavior), or "" if it's absent/unreadable/empty.
func readSoul() string {
	if p := soulPath(); p != "" {
		return readContextFile(p)
	}
	return ""
}

// readUserProfile returns the trimmed, include-expanded contents of
// ~/.octo/user.md (who the user is), or "" if it's absent/unreadable/empty.
func readUserProfile() string {
	if p := userProfilePath(); p != "" {
		return readContextFile(p)
	}
	return ""
}

// readUserContext returns the trimmed, include-expanded contents of the
// per-user global rules file, or "" if it's absent/unreadable/empty.
func readUserContext() string {
	p := userRulesPath()
	if p == "" {
		return ""
	}
	return readContextFile(p)
}

// readProjectContext returns the trimmed, include-expanded contents of
// ProjectContextFile in cwd, or "" if it's absent/unreadable/empty. A missing
// file is not an error — most directories won't have one.
func readProjectContext(cwd string) string {
	if cwd == "" {
		return ""
	}
	return readContextFile(filepath.Join(cwd, ProjectContextFile))
}

// readContextFile reads a context file and expands its @include directives,
// returning the trimmed result (or "" when the file is absent/unreadable).
func readContextFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// Seed the cycle set with the root file so it can't transitively include
	// itself.
	expanded := expandIncludes(string(b), filepath.Dir(abs), 0, map[string]bool{abs: true})
	return strings.TrimSpace(expanded)
}

// expandIncludes inlines @include directives. A line whose first non-space
// character is '@' is an include: the remainder (trimmed) is a file path,
// resolved as ~/… (home), an absolute path, or relative to baseDir (the
// directory of the file currently being expanded). The referenced file is
// read and recursively expanded in place.
//
// Failures degrade to an inline HTML comment rather than aborting the prompt:
//   - unreadable/missing →  <!-- missing include: PATH -->
//   - already in the ancestor chain (cycle) →  <!-- include cycle: PATH -->
//   - past maxIncludeDepth →  <!-- include depth exceeded: PATH -->
//
// seen tracks the current ancestor chain (added before recursing, removed
// after) so a diamond include — the same file reached via two sibling
// branches — is allowed, while a true cycle is not.
func expandIncludes(content, baseDir string, depth int, seen map[string]bool) string {
	var out strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < 2 || trimmed[0] != '@' {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}

		raw := strings.TrimSpace(trimmed[1:])
		resolved := resolveIncludePath(raw, baseDir)
		abs, err := filepath.Abs(resolved)
		if err != nil {
			abs = resolved
		}

		switch {
		case depth+1 > maxIncludeDepth:
			out.WriteString("<!-- include depth exceeded: " + raw + " -->\n")
			continue
		case seen[abs]:
			out.WriteString("<!-- include cycle: " + raw + " -->\n")
			continue
		}

		b, err := os.ReadFile(abs)
		if err != nil {
			out.WriteString("<!-- missing include: " + raw + " -->\n")
			continue
		}

		seen[abs] = true
		nested := expandIncludes(string(b), filepath.Dir(abs), depth+1, seen)
		delete(seen, abs)

		out.WriteString(strings.TrimRight(nested, "\n"))
		out.WriteByte('\n')
	}
	return out.String()
}

// resolveIncludePath turns an @include target into a filesystem path: ~/… is
// expanded against the home dir, absolute paths pass through, and everything
// else is taken relative to baseDir (the including file's directory).
func resolveIncludePath(raw, baseDir string) string {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
		}
	}
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(baseDir, raw)
}
