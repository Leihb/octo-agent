package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/tools"
)

// buildEnvContext renders a snapshot of the working environment for the
// system prompt: working directory, git branch + dirty/clean state, today's
// date, and the OS/arch. The model otherwise has no idea where it's running.
//
// Taken once at session start (the composed prompt is frozen for the
// session), so git state is a snapshot — fresh enough to orient the model
// without re-rendering per turn and busting the prompt cache.
func buildEnvContext(cwd string) string {
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	if cwd != "" {
		fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
	}
	if branch, dirty, ok := gitState(cwd); ok {
		state := "clean"
		if dirty {
			state = "uncommitted changes"
		}
		fmt.Fprintf(&b, "- Git branch: %s (%s)\n", branch, state)
	}
	fmt.Fprintf(&b, "- Today's date: %s\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(&b, "- OS/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	// Platform-shell guidance (PowerShell dialect + the install/PATH/UAC traps
	// on Windows, sudo/PATH/CLT traps on macOS) lives in internal/tools next
	// to the shell selection itself, shared with the server's context builder.
	b.WriteString(tools.ShellEnvNote())
	// Which common developer tools are actually on PATH — so the agent doesn't
	// discover python/node is missing by failing a command. Pairs with the
	// install guidance above.
	b.WriteString(tools.ToolchainNote())
	return b.String()
}

// gitState returns the current branch and whether the working tree has
// uncommitted changes. ok is false when cwd isn't a git repo or git is
// unavailable — callers should omit the git line in that case. A short
// timeout keeps a slow/hung git from stalling startup.
func gitState(cwd string) (branch string, dirty, ok bool) {
	if cwd == "" {
		return "", false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Verify cwd is actually inside a git repo, not just a subdir of one
	// reached by git's upward traversal.
	topLevel, err := gitRun(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false, false
	}
	// Normalize paths for comparison (git returns abs paths; cwd may be ".").
	// Use filepath.Clean to normalize separators so the comparison works on
	// Windows where git may return forward slashes.
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, false
	}
	absCwd = filepath.Clean(absCwd)
	// Resolve symlinks so worktrees on macOS (/tmp → /private/tmp) compare
	// correctly with git's --show-toplevel output.
	absCwd, _ = filepath.EvalSymlinks(absCwd)
	absTop := filepath.Clean(strings.TrimSpace(topLevel))
	if absTop == "" {
		return "", false, false
	}
	absTop, _ = filepath.EvalSymlinks(absTop)
	// cwd must be inside or equal to the repo root.
	if !strings.HasPrefix(absCwd, absTop) {
		return "", false, false
	}

	branchOut, err := gitRun(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", false, false
	}
	branch = strings.TrimSpace(branchOut)
	if branch == "" {
		return "", false, false
	}

	statusOut, err := gitRun(ctx, cwd, "status", "--porcelain")
	if err != nil {
		// We know the branch; report it as clean rather than dropping it.
		return branch, false, true
	}
	return branch, strings.TrimSpace(statusOut) != "", true
}

func gitRun(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	return string(out), err
}
