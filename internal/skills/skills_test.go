package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates <root>/<name>/SKILL.md with the given content.
func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// useUserRoot points userSkillsRoot at dir for the duration of the test.
func useUserRoot(t *testing.T, dir string) {
	t.Helper()
	orig := userSkillsRoot
	userSkillsRoot = func() string { return dir }
	t.Cleanup(func() { userSkillsRoot = orig })
}

func TestDiscover_Empty(t *testing.T) {
	useUserRoot(t, filepath.Join(t.TempDir(), "nonexistent"))
	r := Discover(filepath.Join(t.TempDir(), "alsogone"))
	if r.Len() != 0 {
		t.Errorf("Len = %d, want 0", r.Len())
	}
	if m := RenderManifest(r); m != "" {
		t.Errorf("RenderManifest = %q, want empty", m)
	}
}

func TestDiscover_UserSkill(t *testing.T) {
	userRoot := t.TempDir()
	useUserRoot(t, userRoot)
	writeSkill(t, userRoot, "greet", "---\nname: Greeter\ndescription: Say hello nicely\n---\nBe warm and friendly.")

	r := Discover("")
	s, ok := r.Get("greet")
	if !ok {
		t.Fatal("greet not discovered")
	}
	if s.Name != "greet" {
		t.Errorf("Name = %q, want %q (dir name is authoritative, not frontmatter name)", s.Name, "greet")
	}
	if s.Description != "Say hello nicely" {
		t.Errorf("Description = %q", s.Description)
	}
	if s.Body != "Be warm and friendly." {
		t.Errorf("Body = %q", s.Body)
	}
	if s.Source != "user" {
		t.Errorf("Source = %q, want user", s.Source)
	}
}

func TestDiscover_ProjectOverridesUser(t *testing.T) {
	userRoot := t.TempDir()
	useUserRoot(t, userRoot)
	writeSkill(t, userRoot, "deploy", "---\ndescription: user version\n---\nuser body")

	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".octo", "skills"), "deploy", "---\ndescription: project version\n---\nproject body")

	r := Discover(cwd)
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (same name should collapse)", r.Len())
	}
	s, _ := r.Get("deploy")
	if s.Description != "project version" || s.Source != "project" {
		t.Errorf("project did not override user: %+v", s)
	}
}

func TestDiscover_SkipsMalformed(t *testing.T) {
	userRoot := t.TempDir()
	useUserRoot(t, userRoot)

	// No SKILL.md.
	if err := os.MkdirAll(filepath.Join(userRoot, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No frontmatter fence.
	writeSkill(t, userRoot, "nofence", "just a body, no frontmatter")
	// Frontmatter but no description.
	writeSkill(t, userRoot, "nodesc", "---\nname: x\n---\nbody")
	// A regular file (not a directory) at the root — must be ignored.
	if err := os.WriteFile(filepath.Join(userRoot, "loose.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One good skill.
	writeSkill(t, userRoot, "ok", "---\ndescription: works\n---\nbody")

	r := Discover("")
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (only 'ok' is well-formed)", r.Len())
	}
	if _, ok := r.Get("ok"); !ok {
		t.Error("'ok' should be discovered")
	}
}

// TestDiscover_IgnoresNestedMetadata is the Claude Code compatibility guard:
// CC frontmatter carries a nested `metadata:` block. yaml.v3 must parse the
// file and ignore the unmapped block rather than choking on it.
func TestDiscover_IgnoresNestedMetadata(t *testing.T) {
	userRoot := t.TempDir()
	useUserRoot(t, userRoot)
	writeSkill(t, userRoot, "cc", `---
name: cc-skill
description: A Claude Code style skill
allowed-tools: Bash, Read
metadata:
  author: someone
  version: 2
---
Do the thing.`)

	r := Discover("")
	s, ok := r.Get("cc")
	if !ok {
		t.Fatal("cc not discovered — nested metadata block likely broke parsing")
	}
	if s.Description != "A Claude Code style skill" {
		t.Errorf("Description = %q", s.Description)
	}
	if s.Body != "Do the thing." {
		t.Errorf("Body = %q", s.Body)
	}
}

func TestList_ProjectBeforeUserThenByName(t *testing.T) {
	userRoot := t.TempDir()
	useUserRoot(t, userRoot)
	writeSkill(t, userRoot, "zebra", "---\ndescription: u\n---\nb")
	writeSkill(t, userRoot, "alpha", "---\ndescription: u\n---\nb")

	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".octo", "skills"), "yak", "---\ndescription: p\n---\nb")

	got := make([]string, 0)
	for _, s := range Discover(cwd).List() {
		got = append(got, s.Name)
	}
	want := []string{"yak", "alpha", "zebra"} // project first, then user by name
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("List order = %v, want %v", got, want)
	}
}

func TestRenderManifest(t *testing.T) {
	userRoot := t.TempDir()
	useUserRoot(t, userRoot)
	writeSkill(t, userRoot, "greet", "---\ndescription: Say hello\n---\nbody")

	m := RenderManifest(Discover(""))
	if !strings.Contains(m, "# Available skills") {
		t.Error("manifest missing header")
	}
	if !strings.Contains(m, "`skill` tool") {
		t.Error("manifest should tell the model to use the skill tool")
	}
	if !strings.Contains(m, "- greet: Say hello") {
		t.Errorf("manifest missing skill line:\n%s", m)
	}
}
