package main

import (
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

func TestREPL_SkillsCommand(t *testing.T) {
	cfg, stdout, _, stub := makeREPLFixture(t, "/skills\n/exit\n")
	cfg.skillReg = skillRegFor(t, map[string]string{"greet": "---\ndescription: say hi\n---\nbody"})

	runREPL(cfg)
	if stub.called != 0 {
		t.Errorf("/skills should not call the sender, got %d", stub.called)
	}
	out := stdout.String()
	if !strings.Contains(out, "/greet") || !strings.Contains(out, "say hi") {
		t.Errorf("/skills output missing skill:\n%s", out)
	}
}

func TestREPL_SkillsCommand_None(t *testing.T) {
	cfg, stdout, _, _ := makeREPLFixture(t, "/skills\n/exit\n")
	cfg.skillReg = skillRegFor(t, nil)

	runREPL(cfg)
	if !strings.Contains(stdout.String(), "No skills found") {
		t.Errorf("expected 'No skills found':\n%s", stdout.String())
	}
}

func TestREPL_SkillTriggerRunsTurn(t *testing.T) {
	cfg, stdout, stderr, stub := makeREPLFixture(t, "/greet\n/exit\n")
	cfg.skillReg = skillRegFor(t, map[string]string{"greet": "---\ndescription: d\n---\nSay hello warmly."})

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	if stub.called != 1 {
		t.Errorf("/greet should run one turn, sender called %d times", stub.called)
	}
	if !strings.Contains(stdout.String(), "Running skill /greet") {
		t.Errorf("missing skill-run notice:\n%s", stdout.String())
	}
}

func TestREPL_UnknownSlashStillUnknown(t *testing.T) {
	// A /command that is neither reserved nor a skill keeps the old behaviour.
	cfg, stdout, _, stub := makeREPLFixture(t, "/bogus\n/exit\n")
	cfg.skillReg = skillRegFor(t, map[string]string{"greet": "---\ndescription: d\n---\nbody"})

	runREPL(cfg)
	if stub.called != 0 {
		t.Errorf("unknown command should not call sender, got %d", stub.called)
	}
	if !strings.Contains(stdout.String(), "Unknown command") {
		t.Errorf("expected unknown-command message:\n%s", stdout.String())
	}
}
