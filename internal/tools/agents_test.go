package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useAgentsRoot points userAgentsRoot at dir for the test's duration.
func useAgentsRoot(t *testing.T, dir string) {
	t.Helper()
	orig := userAgentsRoot
	userAgentsRoot = func() string { return dir }
	t.Cleanup(func() { userAgentsRoot = orig })
}

func writeAgentFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverAgents_LoadsFromDisk(t *testing.T) {
	root := t.TempDir()
	useAgentsRoot(t, root)

	writeAgentFile(t, root, "security-review.md", "---\nname: Security Review\ndescription: Review code for security issues\nread_only: true\n---\nYou are a security reviewer. Find vulnerabilities.")

	discoverAgents()
	p, ok := lookupAgentPreset("security-review")
	if !ok {
		t.Fatal("security-review not discovered")
	}
	if p.name != "security-review" {
		t.Errorf("name = %q, want security-review", p.name)
	}
	if p.description != "Review code for security issues" {
		t.Errorf("description = %q", p.description)
	}
	if !p.readOnly {
		t.Error("readOnly should be true")
	}
	if !strings.Contains(p.persona, "security reviewer") {
		t.Errorf("persona = %q", p.persona)
	}
}

func TestDiscoverAgents_UserOverridesBuiltIn(t *testing.T) {
	root := t.TempDir()
	useAgentsRoot(t, root)

	writeAgentFile(t, root, "explore.md", "---\ndescription: My custom explore\nread_only: false\n---\nCustom persona here")

	discoverAgents()
	p, ok := lookupAgentPreset("explore")
	if !ok {
		t.Fatal("explore not found")
	}
	if p.description != "My custom explore" {
		t.Errorf("description = %q, want override", p.description)
	}
	if p.persona != "Custom persona here" {
		t.Errorf("persona = %q", p.persona)
	}
	if p.readOnly {
		t.Error("readOnly should be false from override")
	}
}

func TestDiscoverAgents_SkipsMalformed(t *testing.T) {
	root := t.TempDir()
	useAgentsRoot(t, root)

	// No frontmatter.
	writeAgentFile(t, root, "nofence.md", "just body")
	// Frontmatter but no description.
	writeAgentFile(t, root, "nodesc.md", "---\nname: x\n---\nbody")
	// Not a .md file.
	writeAgentFile(t, root, "readme.txt", "---\ndescription: d\n---\nbody")

	discoverAgents()
	if _, ok := lookupAgentPreset("nofence"); ok {
		t.Error("nofence should be skipped")
	}
	if _, ok := lookupAgentPreset("nodesc"); ok {
		t.Error("nodesc should be skipped")
	}
	if _, ok := lookupAgentPreset("readme"); ok {
		t.Error("readme.txt should be skipped")
	}
}

func TestDiscoverAgents_MissingRootIsNoOp(t *testing.T) {
	useAgentsRoot(t, filepath.Join(t.TempDir(), "nonexistent"))
	discoverAgents()

	// Built-ins should still work.
	p, ok := lookupAgentPreset("general")
	if !ok {
		t.Fatal("built-in general should still be found")
	}
	if p.name != "general" {
		t.Errorf("name = %q", p.name)
	}
}

func TestListPresetNames_IncludesUserAndBuiltIn(t *testing.T) {
	root := t.TempDir()
	useAgentsRoot(t, root)

	writeAgentFile(t, root, "custom.md", "---\ndescription: c\n---\nbody")

	names := listPresetNames()
	if !strings.Contains(names, "custom") {
		t.Errorf("listPresetNames missing custom: %q", names)
	}
	if !strings.Contains(names, "explore") {
		t.Errorf("listPresetNames missing built-in explore: %q", names)
	}
	if !strings.Contains(names, "general") {
		t.Errorf("listPresetNames missing built-in general: %q", names)
	}
}

func TestListPresetNames_NoDuplicatesWhenOverridden(t *testing.T) {
	root := t.TempDir()
	useAgentsRoot(t, root)

	writeAgentFile(t, root, "explore.md", "---\ndescription: override\n---\nbody")

	names := listPresetNames()
	count := strings.Count(names, "explore")
	if count != 1 {
		t.Errorf("explore appears %d times in %q", count, names)
	}
}

func TestParseAgentFile_Valid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.md")
	content := "---\nname: Test\ndescription: A test agent\nread_only: true\n---\nBe helpful.\n\nAlways verify."
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	p, ok := parseAgentFile(path)
	if !ok {
		t.Fatal("parseAgentFile returned false")
	}
	if p.description != "A test agent" {
		t.Errorf("description = %q", p.description)
	}
	if !p.readOnly {
		t.Error("readOnly should be true")
	}
	if !strings.Contains(p.persona, "Be helpful.") {
		t.Errorf("persona = %q", p.persona)
	}
}

func TestParseAgentFile_Invalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(path, []byte("no frontmatter here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := parseAgentFile(path); ok {
		t.Error("expected false for file without frontmatter")
	}
}
