package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain neutralizes the default-skills root for the whole package so tests
// never read the real ~/.octo/skills-default (which an installed binary
// populates). Tests that exercise defaults opt in via useDefaultRoot.
func TestMain(m *testing.M) {
	tmp, _ := os.MkdirTemp("", "octo-skills-default-empty")
	defaultSkillsRoot = func() string { return tmp }
	code := m.Run()
	if tmp != "" {
		_ = os.RemoveAll(tmp)
	}
	os.Exit(code)
}

// useDefaultRoot points defaultSkillsRoot at dir for the test's duration.
func useDefaultRoot(t *testing.T, dir string) {
	t.Helper()
	orig := defaultSkillsRoot
	defaultSkillsRoot = func() string { return dir }
	t.Cleanup(func() { defaultSkillsRoot = orig })
}

func TestMaterializeDefaults_WritesEmbeddedAndStamps(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills-default")
	useDefaultRoot(t, root)

	if err := MaterializeDefaults("v1"); err != nil {
		t.Fatalf("MaterializeDefaults: %v", err)
	}
	// The shipped worktree-isolate skill must land on disk.
	if _, err := os.Stat(filepath.Join(root, "worktree-isolate", "SKILL.md")); err != nil {
		t.Fatalf("expected worktree-isolate/SKILL.md materialized: %v", err)
	}
	// Stamp records the version.
	b, err := os.ReadFile(filepath.Join(root, defaultStampFile))
	if err != nil || string(b) != "v1" {
		t.Fatalf("stamp = %q, %v; want v1", string(b), err)
	}
}

func TestMaterializeDefaults_NoOpWhenCurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills-default")
	useDefaultRoot(t, root)
	if err := MaterializeDefaults("v1"); err != nil {
		t.Fatal(err)
	}
	// Drop a sentinel; a no-op (same version) must not wipe the dir.
	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeDefaults("v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("same-version call should be a no-op, but the dir was rewritten")
	}

	// A version bump (or UpdateDefaults force) re-materializes, wiping stale files.
	if err := MaterializeDefaults("v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("version bump should wipe-and-rewrite the default root")
	}
}

func TestDiscover_DefaultSkillSurfacesAndIsOverridable(t *testing.T) {
	defaultRoot := filepath.Join(t.TempDir(), "skills-default")
	useDefaultRoot(t, defaultRoot)
	if err := MaterializeDefaults("v1"); err != nil {
		t.Fatal(err)
	}

	userRoot := t.TempDir()
	useUserRoot(t, userRoot)

	// Default-only: worktree-isolate is discovered with source "default".
	reg := Discover("")
	s, ok := reg.Get("worktree-isolate")
	if !ok {
		t.Fatal("default worktree-isolate not discovered")
	}
	if s.Source != "default" {
		t.Errorf("source = %q, want default", s.Source)
	}

	// A user skill of the same name overrides the default.
	dir := filepath.Join(userRoot, "worktree-isolate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "SKILL.md"), "---\nname: worktree-isolate\ndescription: my override\n---\nbody")

	reg = Discover("")
	s, _ = reg.Get("worktree-isolate")
	if s.Source != "user" || s.Description != "my override" {
		t.Errorf("user skill should override default, got source=%q desc=%q", s.Source, s.Description)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
