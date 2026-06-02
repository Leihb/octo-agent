package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitExec(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitExec(t, repo, "init", "-q")
	gitExec(t, repo, "config", "user.email", "t@example.com")
	gitExec(t, repo, "config", "user.name", "tester")
	gitExec(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, repo, "add", "-A")
	gitExec(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func newTestWorktrees(t *testing.T, repo string) *gitWorktrees {
	t.Helper()
	return &gitWorktrees{
		root: repo,
		base: filepath.Join(t.TempDir(), "wt"), // outside the repo, like production
		dirs: map[string]string{},
	}
}

// A worker's new file in its worktree integrates into the trunk on Merge.
func TestGitWorktreesMergeIntegratesWork(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	g := newTestWorktrees(t, repo)

	branch, dir, err := g.Create(ctx, 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("from worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.Merge(ctx, branch); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// The trunk working tree should now contain the worker's file.
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatalf("merged file missing on trunk: %v", err)
	}
	if err := g.Cleanup(ctx, branch, dir); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree dir not removed after cleanup")
	}
}

// A worktree that makes no change merges cleanly (empty commit skipped).
func TestGitWorktreesMergeNoChange(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	g := newTestWorktrees(t, repo)
	branch, dir, err := g.Create(ctx, 2)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := g.Merge(ctx, branch); err != nil {
		t.Fatalf("Merge (no change): %v", err)
	}
	_ = g.Cleanup(ctx, branch, dir)
}

// A conflicting change surfaces as a Merge error (the conductor turns this into
// a retry telling the worker to rebase).
func TestGitWorktreesMergeConflict(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	g := newTestWorktrees(t, repo)

	branch, dir, err := g.Create(ctx, 3)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Worker edits README in its worktree…
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("worker change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// …while the trunk diverges on the same line.
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("trunk change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, repo, "commit", "-q", "-am", "trunk diverges")

	if err := g.Merge(ctx, branch); err == nil {
		t.Fatal("Merge succeeded, want conflict error")
	}
	// Trunk must be left clean (merge aborted), not mid-conflict.
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
	if len(out) != 0 {
		t.Errorf("trunk not clean after aborted merge:\n%s", out)
	}
	_ = g.Cleanup(ctx, branch, dir)
}
