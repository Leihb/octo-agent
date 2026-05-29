package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/permission"
)

func newGate(t *testing.T, mode permission.Mode, stdin string) (*cliPermissionGate, *bytes.Buffer) {
	t.Helper()
	eng, err := permission.New("", "/work", mode)
	if err != nil {
		t.Fatalf("permission.New: %v", err)
	}
	var out bytes.Buffer
	view := newPlainView(newScannerLineReader(strings.NewReader(stdin), &out), &out, &out, verbosityNormal, false)
	g := &cliPermissionGate{
		engine: eng,
		ask:    view,
	}
	return g, &out
}

func TestCLIGate_AllowPassesThrough(t *testing.T) {
	g, out := newGate(t, permission.ModeInteractive, "")
	ok, reason := g.Check(context.Background(), "terminal", map[string]any{"command": "ls"})
	if !ok {
		t.Errorf("ls should be allowed; reason=%q", reason)
	}
	if out.Len() != 0 {
		t.Errorf("allow should not prompt; got output %q", out.String())
	}
}

func TestCLIGate_DenyReturnsReason(t *testing.T) {
	g, out := newGate(t, permission.ModeInteractive, "")
	ok, reason := g.Check(context.Background(), "terminal", map[string]any{"command": "rm -rf /"})
	if ok {
		t.Error("rm -rf / must be denied")
	}
	if !strings.Contains(reason, "permission_denied") {
		t.Errorf("expected structured denial reason, got %q", reason)
	}
	if out.Len() != 0 {
		t.Errorf("deny should not prompt; got %q", out.String())
	}
}

func TestCLIGate_AskPromptYes(t *testing.T) {
	g, out := newGate(t, permission.ModeInteractive, "y\n")
	ok, _ := g.Check(context.Background(), "terminal", map[string]any{"command": "sudo apt update"})
	if !ok {
		t.Error("answering 'y' should allow")
	}
	if !strings.Contains(out.String(), "allow?") {
		t.Errorf("expected a prompt to be printed; got %q", out.String())
	}
}

func TestCLIGate_AskPromptNoByDefault(t *testing.T) {
	// Empty answer (just Enter) → deny.
	g, _ := newGate(t, permission.ModeInteractive, "\n")
	ok, reason := g.Check(context.Background(), "terminal", map[string]any{"command": "sudo apt update"})
	if ok {
		t.Error("empty answer should deny (capital N is the default)")
	}
	if !strings.Contains(reason, "declined") {
		t.Errorf("reason should note the user declined, got %q", reason)
	}
}

func TestCLIGate_AskPromptAlwaysRemembers(t *testing.T) {
	// Answer "a" once; the engine should remember and a SECOND identical
	// call should allow without prompting.
	g, out := newGate(t, permission.ModeInteractive, "a\n")
	input := map[string]any{"command": "sudo apt update"}

	if ok, _ := g.Check(context.Background(), "terminal", input); !ok {
		t.Fatal("answering 'a' should allow")
	}
	promptLenAfterFirst := out.Len()

	// Second call: no new prompt, still allowed.
	if ok, _ := g.Check(context.Background(), "terminal", input); !ok {
		t.Error("remembered decision should allow on repeat")
	}
	if out.Len() != promptLenAfterFirst {
		t.Errorf("second call should not prompt again; extra output %q",
			out.String()[promptLenAfterFirst:])
	}
}

func TestCLIGate_StrictModeNeverPrompts(t *testing.T) {
	// In strict mode the engine collapses ask → deny, so a command that
	// would normally prompt is denied without reading stdin.
	g, out := newGate(t, permission.ModeStrict, "y\n") // stdin "y" must be ignored
	ok, reason := g.Check(context.Background(), "terminal", map[string]any{"command": "sudo apt update"})
	if ok {
		t.Error("strict mode must deny ask-class commands")
	}
	if out.Len() != 0 {
		t.Errorf("strict mode must not prompt; got %q", out.String())
	}
	if !strings.Contains(reason, "permission_denied") {
		t.Errorf("expected denial reason, got %q", reason)
	}
}

func TestResolvePermissionMode(t *testing.T) {
	if resolvePermissionMode("strict") != permission.ModeStrict {
		t.Error("strict should map to ModeStrict")
	}
	if resolvePermissionMode("interactive") != permission.ModeInteractive {
		t.Error("interactive should map to ModeInteractive")
	}
	// Unknown falls back to interactive (chat.go validates before this is
	// reached, but the helper itself defaults safely).
	if resolvePermissionMode("garbage") != permission.ModeInteractive {
		t.Error("unknown should fall back to ModeInteractive")
	}
}
