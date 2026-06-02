package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// gitWorktrees implements conductor.Worktrees with real git worktrees. Each
// unit runs in its own worktree on its own branch off the current trunk HEAD;
// a green unit is committed in its worktree and merged back into the trunk by
// the conductor (serially). A worker's file edits and shell thus never collide
// with a sibling worker's, and a failed unit's mess stays off the trunk.
//
// All operations are mutex-serialised: `git worktree add/remove` and trunk
// merges mutate shared git state, and the conductor may dispatch several units
// concurrently.
type gitWorktrees struct {
	mu   sync.Mutex
	root string            // trunk repo top level
	base string            // parent dir holding the per-unit worktrees
	dirs map[string]string // branch → worktree dir (for commit-before-merge)
}

// newGitWorktrees returns a manager rooted at the current repo's top level, or
// an error if the cwd isn't inside a git work tree (the caller then falls back
// to running workers in the main tree).
func newGitWorktrees() (*gitWorktrees, error) {
	root, err := gitTopLevel()
	if err != nil {
		return nil, err
	}
	// Hold worktrees OUTSIDE the repo so the trunk's `git status` stays clean
	// (an in-repo base would show up as an untracked dir). A per-repo name
	// derived from the path keeps concurrent repos from colliding; randHex
	// per-worktree dirs keep units within a repo distinct.
	base := filepath.Join(os.TempDir(), "octo-conduct-wt", filepath.Base(root)+"-"+randHex(3))
	return &gitWorktrees{
		root: root,
		base: base,
		dirs: map[string]string{},
	}, nil
}

func gitTopLevel() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git work tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Create adds a fresh worktree for unitID on a new branch off trunk HEAD.
func (g *gitWorktrees) Create(ctx context.Context, unitID int) (branch, dir string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	suffix := randHex(4)
	branch = fmt.Sprintf("octo-conduct/u%d-%s", unitID, suffix)
	dir = filepath.Join(g.base, fmt.Sprintf("u%d-%s", unitID, suffix))
	if err := os.MkdirAll(g.base, 0o755); err != nil {
		return "", "", err
	}
	if out, e := g.git(ctx, g.root, "worktree", "add", "-b", branch, dir, "HEAD"); e != nil {
		return "", "", fmt.Errorf("worktree add: %v: %s", e, strings.TrimSpace(out))
	}
	g.dirs[branch] = dir
	return branch, dir, nil
}

// Merge commits whatever the worker changed in its worktree, then merges the
// branch into the trunk. A merge conflict aborts and returns an error (the
// conductor surfaces it to the worker as a retry: rebase + resolve).
func (g *gitWorktrees) Merge(ctx context.Context, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	dir := g.dirs[branch]
	if dir != "" {
		// Stage + commit the worker's output. Skip the commit when the tree is
		// clean (the worker may have already committed, or made no changes).
		_, _ = g.git(ctx, dir, "add", "-A")
		if g.hasStagedChanges(ctx, dir) {
			if out, e := g.git(ctx, dir, "commit", "-m", "conduct: "+branch); e != nil {
				return fmt.Errorf("commit worktree: %v: %s", e, strings.TrimSpace(out))
			}
		}
	}

	if out, e := g.git(ctx, g.root, "merge", "--no-ff", "--no-edit", branch); e != nil {
		_, _ = g.git(ctx, g.root, "merge", "--abort")
		return fmt.Errorf("merge into trunk failed:\n%s", strings.TrimSpace(out))
	}
	return nil
}

// Cleanup removes the worktree and deletes its (now-merged) branch.
func (g *gitWorktrees) Cleanup(ctx context.Context, branch, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if dir == "" {
		dir = g.dirs[branch]
	}
	if dir != "" {
		_, _ = g.git(ctx, g.root, "worktree", "remove", "--force", dir)
	}
	_, _ = g.git(ctx, g.root, "branch", "-D", branch)
	delete(g.dirs, branch)
	return nil
}

// hasStagedChanges reports whether `dir` has anything staged (git diff
// --cached --quiet exits 1 when there are staged changes).
func (g *gitWorktrees) hasStagedChanges(ctx context.Context, dir string) bool {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	cmd.Dir = dir
	return cmd.Run() != nil
}

func (g *gitWorktrees) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
