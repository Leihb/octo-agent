package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/skills"
)

// skillRegFor builds a project-level skill registry from name→SKILL.md content,
// with HOME pointed at an empty dir so no real user skills leak in.
func skillRegFor(t *testing.T, m map[string]string) *skills.Registry {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)

	cwd := t.TempDir()
	for name, content := range m {
		dir := filepath.Join(cwd, ".octo", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, skills.SkillFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return skills.Discover(cwd)
}

func TestSkillTrigger(t *testing.T) {
	reg := skillRegFor(t, map[string]string{
		"greet": "---\ndescription: d\n---\nbody",
		"help":  "---\ndescription: d\n---\nshadow attempt",
	})

	if _, _, ok := skillTrigger(nil, "/greet"); ok {
		t.Error("nil registry should never match")
	}
	if _, _, ok := skillTrigger(reg, "greet"); ok {
		t.Error("non-slash input should not match")
	}
	if _, _, ok := skillTrigger(reg, "/help"); ok {
		t.Error("reserved command /help must not be hijacked by a same-named skill")
	}
	if _, _, ok := skillTrigger(reg, "/nope"); ok {
		t.Error("unknown skill should not match")
	}
	s, args, ok := skillTrigger(reg, "/greet")
	if !ok || s.Name != "greet" || args != "" {
		t.Errorf("/greet → %q, args=%q, ok=%v", s.Name, args, ok)
	}
	s, args, ok = skillTrigger(reg, "/greet  hello world")
	if !ok || args != "hello world" {
		t.Errorf("/greet args → args=%q, ok=%v", args, ok)
	}
}

func TestInlineSkill(t *testing.T) {
	if got := inlineSkill("BODY", ""); got != "BODY" {
		t.Errorf("no args → %q", got)
	}
	if got := inlineSkill("BODY", "x"); got != "BODY\n\nUser input: x" {
		t.Errorf("with args → %q", got)
	}
}

// The /skills listing, /<skill> trigger, and unknown-command behaviour are
// exercised through the TUI's dispatchSlash in tuirepl_slash_test.go now that
// slash commands live there. skillTrigger / inlineSkill remain unit-tested
// above since dispatchSlash relies on them; printSkills (the /skills renderer)
// is tested directly here.

func TestPrintSkills_ListsSkills(t *testing.T) {
	reg := skillRegFor(t, map[string]string{"greet": "---\ndescription: say hi\n---\nbody"})
	var out bytes.Buffer
	printSkills(&out, reg)
	if !strings.Contains(out.String(), "/greet") || !strings.Contains(out.String(), "say hi") {
		t.Errorf("printSkills missing skill:\n%s", out.String())
	}
}

func TestPrintSkills_None(t *testing.T) {
	var out bytes.Buffer
	printSkills(&out, skillRegFor(t, nil))
	if !strings.Contains(out.String(), "No skills found") {
		t.Errorf("expected 'No skills found':\n%s", out.String())
	}
}
