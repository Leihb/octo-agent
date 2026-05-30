//go:build darwin || linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Leihb/octo-agent/internal/sandbox"
)

// TestTerminal_SandboxConfinesIO verifies the wiring: once SetSandbox is active,
// the terminal tool runs commands confined — writes land inside the project
// root but are denied outside it. Skipped where the OS can't enforce a sandbox.
func TestTerminal_SandboxConfinesIO(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("no OS sandbox available on this host")
	}
	cwd := t.TempDir()
	p := sandbox.DefaultPolicy(cwd)
	SetSandbox(&p)
	defer SetSandbox(nil)

	reg := NewDefaultRegistry()
	ctx := context.Background()

	// Write INSIDE the project root succeeds.
	inside := filepath.Join(cwd, "inside.txt")
	out, err := reg.Execute(ctx, "terminal", map[string]any{
		"command": "echo hi > " + inside + " && echo OK || echo FAIL:$?",
	})
	t.Logf("inside cmd → err=%v out=%q", err, out.Text)
	if err != nil {
		t.Fatalf("terminal execute (inside): %v", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("inside.txt should exist under the project root: %v", err)
	}

	// Write OUTSIDE the roots (under $HOME) is blocked by the sandbox.
	home, _ := os.UserHomeDir()
	outside := filepath.Join(home, ".octo_tools_sbx_probe")
	defer os.Remove(outside)
	if _, err := reg.Execute(ctx, "terminal", map[string]any{
		"command": "echo x > " + outside,
	}); err != nil {
		t.Fatalf("terminal execute (outside) returned a Go error: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("write outside the sandbox roots must be denied (file was created)")
	}
}
