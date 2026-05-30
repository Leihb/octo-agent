package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/skills"
)

// discoverSkills builds a registry from a temp project dir holding the given
// skill (name → SKILL.md content) and restores the package state after.
func setSkillsFor(t *testing.T, name, content string) {
	t.Helper()
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".octo", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skills.SkillFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	SetSkills(skills.Discover(cwd))
	t.Cleanup(func() { SetSkills(nil) })
}

func TestSkillTool_Execute(t *testing.T) {
	setSkillsFor(t, "greet", "---\ndescription: say hi\n---\nStep 1: be nice.")

	out, err := SkillTool{}.Execute(context.Background(), "skill", map[string]any{"name": "greet"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Text != "Step 1: be nice." {
		t.Errorf("body = %q", out.Text)
	}
}

func TestSkillTool_Errors(t *testing.T) {
	SetSkills(nil)
	t.Cleanup(func() { SetSkills(nil) })

	// No name.
	if _, err := (SkillTool{}).Execute(context.Background(), "skill", map[string]any{}); err == nil {
		t.Error("expected error for missing name")
	}
	// Name given but no skills configured.
	if _, err := (SkillTool{}).Execute(context.Background(), "skill", map[string]any{"name": "x"}); err == nil || !strings.Contains(err.Error(), "no skills") {
		t.Errorf("expected 'no skills' error, got %v", err)
	}

	// Unknown skill when some exist.
	setSkillsFor(t, "real", "---\ndescription: d\n---\nbody")
	if _, err := (SkillTool{}).Execute(context.Background(), "skill", map[string]any{"name": "ghost"}); err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Errorf("expected 'unknown skill' error, got %v", err)
	}
}

func TestDefaultTools_SkillToolGatedOnRegistry(t *testing.T) {
	SetSkills(nil)
	t.Cleanup(func() { SetSkills(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "skill" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("skill tool should be absent when no skills are discovered")
	}

	setSkillsFor(t, "x", "---\ndescription: d\n---\nbody")
	if !has() {
		t.Error("skill tool should be present once a skill is discovered")
	}
}
