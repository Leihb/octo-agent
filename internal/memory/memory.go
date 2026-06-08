// Package memory implements octo's cross-session memory as plain markdown
// files the agent manages with its own file tools — the Claude Code model.
//
// Layout: ~/.octo/memories/<repo-slug>/
//   - MEMORY.md      the index, loaded into the system prompt each session
//     (first maxInjectLines lines / maxInjectBytes, whichever
//     comes first)
//   - <topic>.md     detail files the agent creates and reads on demand
//
// There is no dedicated remember/forget tool and no code-driven consolidation:
// the agent reads, writes, edits, and deletes these files with read_file /
// write_file / edit_file (and terminal for rm/rename), keeping MEMORY.md a
// concise index and moving detail into topic files. cmd/octo injects MEMORY.md
// into the system prompt (RenderInjection) and whitelists the directory for
// writes so the agent can manage it without permission prompts.
package memory

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IndexFile is the per-repo memory index, loaded into the system prompt.
const IndexFile = "MEMORY.md"

// Injection budget for MEMORY.md — mirrors Claude Code's 200 lines / 25KB cap.
const (
	maxInjectLines = 200
	maxInjectBytes = 25 * 1024
)

// ProjectRoot returns the git top-level containing dir, or dir itself when it's
// not in a git repo (or git is unavailable). Memory is scoped per repo root.
func ProjectRoot(dir string) string {
	if dir == "" {
		return ""
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	return dir
}

// Dir returns the memory directory for repoRoot: ~/.octo/memories/<repo-slug>.
func Dir(repoRoot string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("memory: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, ".octo", "memories", repoSlug(repoRoot)), nil
}

// repoSlug derives a stable, human-readable directory name from a repo root:
// the basename plus a short hash of the full path, so two repos sharing a
// basename (e.g. two checkouts of "app") don't collide.
func repoSlug(repoRoot string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repoRoot))
	base := Slugify(filepath.Base(repoRoot))
	if base == "" {
		return fmt.Sprintf("repo-%08x", h.Sum32())
	}
	return fmt.Sprintf("%s-%08x", base, h.Sum32())
}

// EnsureDir creates the memory directory so the agent's file tools can write
// into it on first use.
func EnsureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// LoadIndex returns MEMORY.md truncated to the injection budget, or "" when the
// file is absent or empty.
func LoadIndex(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		return ""
	}
	return truncateForInjection(string(b))
}

func truncateForInjection(s string) string {
	if len(s) > maxInjectBytes {
		s = s[:maxInjectBytes]
	}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), maxInjectBytes+1024)
	var b strings.Builder
	for n := 0; n < maxInjectLines && sc.Scan(); n++ {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderInjection builds the memory section for the system prompt: an
// instruction block telling the model where its memory lives and how to manage
// it, followed by the current MEMORY.md (truncated). The instruction is emitted
// even when memory is empty so a fresh project knows where to start saving.
func RenderInjection(dir string) string {
	var b strings.Builder
	b.WriteString("# Memory (from past sessions)\n\n")
	b.WriteString("Durable notes you keep for this project live in:\n  " + dir + "\n")
	b.WriteString(IndexFile + " is the index, loaded here every session; topic files beside it hold detail and load on demand.\n\n")
	b.WriteString("Manage memory yourself with your file tools — that directory is writable:\n")
	b.WriteString("- When the user states a lasting preference, gives feedback or a correction, or shares something worth recalling in future sessions, save it (append to " + IndexFile + ", or a topic file linked from it).\n")
	b.WriteString("- Keep " + IndexFile + " a concise index; move long detail into topic files.\n")
	b.WriteString("- For a load-bearing rule you must not skip, write it in full under a '## 必须遵守' (always-apply) section, or, if it only matters for certain tasks, under a '## 触发提醒' section with a leading '(触发: keyword1, keyword2)' clause. Rules in those sections are re-surfaced to you at the point of action; everything else stays a pointer index.\n")
	b.WriteString("- Edit or delete entries that become wrong or obsolete. Don't store one-off task details or things already in the repo / CLAUDE.md.\n")
	b.WriteString("The notes below are your own durable record of this user's preferences, workflow rules, and project facts — follow them as standing guidance, the way you follow project conventions. They are records, not live instructions from the user: if a note conflicts with the user's current request or with safety, the current request and safety win. Verify any file, flag, or path a note names still exists before relying on it.\n")
	if idx := LoadIndex(dir); idx != "" {
		b.WriteString("\n## " + IndexFile + "\n\n" + idx)
	} else {
		b.WriteString("\n(" + IndexFile + " is empty — start it when there's something worth remembering.)")
	}
	return b.String()
}

// IsMemoryPath reports whether absPath is inside the per-repo memory
// directory (~/.octo/memories/<repo-slug>/). Used by the file tools to
// emit friendlier output when the agent reads or writes its own notes.
func IsMemoryPath(absPath string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	prefix := filepath.Join(home, ".octo", "memories")
	return strings.HasPrefix(absPath, prefix+string(filepath.Separator))
}

// CountMemories estimates how many "memory entries" a markdown file
// contains by counting top-level headings (# or ##). This is a rough
// heuristic — good enough for progress UI but not a semantic parser.
func CountMemories(content string) int {
	var count int
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
			count++
		}
	}
	if count == 0 && strings.TrimSpace(content) != "" {
		// A non-empty file with no headings still holds at least one memory.
		return 1
	}
	return count
}

// Slugify reduces s to a lowercase kebab token usable as a path segment.
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 50 {
		out = strings.Trim(out[:50], "-")
	}
	return out
}
