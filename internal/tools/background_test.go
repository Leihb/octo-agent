package tools

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// waitFor polls fn until it returns true or the deadline passes.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBackgroundManager_RunsAndReportsExit(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start("echo hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var out, status string
	waitFor(t, "process to exit", func() bool {
		var found bool
		o, s, f := m.Read(id)
		found = f
		if o != "" {
			out += o // accumulate across reads (cursor advances)
		}
		status = s
		return found && strings.HasPrefix(s, "exited")
	})

	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q, want it to contain 'hello'", out)
	}
	if status != "exited: 0" {
		t.Errorf("status = %q, want 'exited: 0'", status)
	}
}

func TestBackgroundManager_IncrementalRead(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start("echo one; sleep 0.3; echo two")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// First chunk: "one" before "two" is emitted.
	waitFor(t, "first line", func() bool {
		o, _, _ := m.Read(id)
		return strings.Contains(o, "one")
	})
	// A read right after consuming "one" should not re-return it.
	o, _, _ := m.Read(id)
	if strings.Contains(o, "one") {
		t.Errorf("second read re-returned old output: %q", o)
	}
	// Eventually "two" arrives as new output.
	waitFor(t, "second line", func() bool {
		o, _, _ := m.Read(id)
		return strings.Contains(o, "two")
	})
}

func TestBackgroundManager_Kill(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start("sleep 30")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.Kill(id) {
		t.Fatal("Kill returned false for a live process")
	}
	waitFor(t, "killed process to report exit", func() bool {
		_, s, _ := m.Read(id)
		return strings.HasPrefix(s, "exited")
	})

	if m.Kill("bg_does_not_exist") {
		t.Error("Kill of unknown id should return false")
	}
}

func TestTerminalTool_BackgroundLaunch(t *testing.T) {
	m := NewBackgroundManager()
	tool := TerminalTool{mgr: m}
	resTool, err := tool.Execute(context.Background(), "terminal", map[string]any{
		"command":    "echo hi",
		"background": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resTool.Text, "bg_1") {
		t.Errorf("result = %q, want it to mention the bg id", resTool.Text)
	}
	// The terminal guard (e.g. in-place sed) still applies to background launches.
	if _, err := tool.Execute(context.Background(), "terminal", map[string]any{
		"command":    "sed -i 's/a/b/' file.txt",
		"background": true,
	}); err == nil {
		t.Error("guarded command should be refused even in background mode")
	}
}

func TestTerminalOutputTool(t *testing.T) {
	m := NewBackgroundManager()
	term := TerminalTool{mgr: m}
	outTool := TerminalOutputTool{mgr: m}

	if _, err := term.Execute(context.Background(), "terminal", map[string]any{
		"command":    "echo from-bg",
		"background": true,
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	var res string
	waitFor(t, "terminal_output to show exit", func() bool {
		rTool, err := outTool.Execute(context.Background(), "terminal_output", map[string]any{"id": "bg_1"})
		if err != nil {
			t.Fatalf("terminal_output: %v", err)
		}
		res += rTool.Text
		return strings.Contains(res, "exited")
	})
	if !strings.Contains(res, "from-bg") {
		t.Errorf("terminal_output = %q, want it to contain 'from-bg'", res)
	}

	// Unknown id is an error.
	if _, err := outTool.Execute(context.Background(), "terminal_output", map[string]any{"id": "bg_99"}); err == nil {
		t.Error("terminal_output of unknown id should error")
	}
	// Missing id is an error.
	if _, err := outTool.Execute(context.Background(), "terminal_output", map[string]any{}); err == nil {
		t.Error("terminal_output without id should error")
	}
}

func TestKillShellTool(t *testing.T) {
	m := NewBackgroundManager()
	term := TerminalTool{mgr: m}
	killTool := KillShellTool{mgr: m}

	if _, err := term.Execute(context.Background(), "terminal", map[string]any{
		"command":    "sleep 30",
		"background": true,
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	resKill, err := killTool.Execute(context.Background(), "kill_shell", map[string]any{"id": "bg_1"})
	if err != nil {
		t.Fatalf("kill_shell: %v", err)
	}
	if !strings.Contains(resKill.Text, "killed") {
		t.Errorf("result = %q, want it to note the kill", resKill.Text)
	}

	// Unknown id is an error.
	if _, err := killTool.Execute(context.Background(), "kill_shell", map[string]any{"id": "bg_nope"}); err == nil {
		t.Error("kill_shell on unknown id should error")
	}
}

// TestTerminalTool_TimeoutPromotesToBackground verifies that a synchronous
// command which exceeds TerminalTimeout is killed and automatically restarted
// as a background process. The agent receives partial output plus a bg id.
func TestTerminalTool_TimeoutPromotesToBackground(t *testing.T) {
	// Use a short timeout so the test doesn't take 30 s.  500 ms is enough
	// for POSIX `sh` and Windows PowerShell to start and emit a line, while
	// keeping the test fast.
	oldTimeout := TerminalTimeout
	TerminalTimeout = 500 * time.Millisecond
	defer func() { TerminalTimeout = oldTimeout }()

	m := NewBackgroundManager()
	term := TerminalTool{mgr: m}

	// Use `echo` (fast, cross-platform) piped to a long sleep.  On POSIX
	// `sleep` is available; on Windows we use `Start-Sleep` via PowerShell.
	cmd := "echo partial && sleep 1"
	if runtime.GOOS == "windows" {
		cmd = "Write-Output partial; Start-Sleep -Seconds 1"
	}
	res, err := term.Execute(context.Background(), "terminal", map[string]any{
		"command": cmd,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should contain the partial output.
	if !strings.Contains(res.Text, "partial") {
		t.Errorf("result should contain partial output, got: %q", res.Text)
	}
	// Should mention the timeout and background promotion.
	if !strings.Contains(res.Text, "timeout") {
		t.Errorf("result should mention timeout, got: %q", res.Text)
	}
	if !strings.Contains(res.Text, "bg_1") {
		t.Errorf("result should mention bg id, got: %q", res.Text)
	}
	// Should contain the anti-polling instruction.
	if !strings.Contains(res.Text, "DO NOT poll") {
		t.Errorf("result should warn against polling, got: %q", res.Text)
	}

	// The background process should eventually finish (sleep 0.5 + margin).
	// Use a longer deadline than the default waitFor because CI under -race
	// can be very slow.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, s, _ := m.Read("bg_1")
		if strings.HasPrefix(s, "exited") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for background process to exit")
}

func TestTerminalOutputTool_ReadOnly(t *testing.T) {
	m := NewBackgroundManager()
	term := TerminalTool{mgr: m}
	outTool := TerminalOutputTool{mgr: m}

	if _, err := term.Execute(context.Background(), "terminal", map[string]any{
		"command":    "sleep 30",
		"background": true,
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// terminal_output no longer kills: a "kill" key is ignored and the process
	// keeps running.
	resOut, err := outTool.Execute(context.Background(), "terminal_output", map[string]any{
		"id":   "bg_1",
		"kill": true, // ignored
	})
	if err != nil {
		t.Fatalf("terminal_output: %v", err)
	}
	if strings.Contains(resOut.Text, "killed") {
		t.Errorf("terminal_output must not kill, got %q", resOut.Text)
	}
	if _, status, _ := m.Read("bg_1"); status != "running" {
		t.Errorf("process should still be running after terminal_output, status=%q", status)
	}
	// When the process is running and there is no new output, the result must
	// contain a strong anti-polling warning.
	if !strings.Contains(resOut.Text, "STOP POLLING") {
		t.Errorf("terminal_output on running process with no new output should warn against polling, got %q", resOut.Text)
	}
}
