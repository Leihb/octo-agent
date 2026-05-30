package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

func TestTerminalTool_Definition(t *testing.T) {
	def := TerminalTool{}.Definition()
	if def.Name != "terminal" {
		t.Errorf("Name = %q, want terminal", def.Name)
	}
	if def.Description == "" {
		t.Error("Description should not be empty")
	}
	if def.Parameters["type"] != "object" {
		t.Errorf("Parameters.type = %v", def.Parameters["type"])
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters.properties is not a map")
	}
	if _, ok := props["command"]; !ok {
		t.Error("Parameters.properties.command should exist")
	}
	req, ok := def.Parameters["required"].([]string)
	if !ok {
		t.Fatal("Parameters.required is not []string")
	}
	if len(req) != 1 || req[0] != "command" {
		t.Errorf("required = %v, want [command]", req)
	}
}

func TestTerminalTool_Execute_Echo(t *testing.T) {
	result, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(result.Text) != "hello" {
		t.Errorf("result = %q, want 'hello'", result.Text)
	}
}

func TestTerminalTool_Execute_Multiline(t *testing.T) {
	result, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "echo line1 && echo line2",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "line1") || !strings.Contains(result.Text, "line2") {
		t.Errorf("result = %q, want line1 and line2", result.Text)
	}
}

func TestTerminalTool_Execute_NonZeroExit(t *testing.T) {
	// A failing command should return output + exit info as result text,
	// NOT as a Go error, so the LLM can read it. `echo …; exit 1` is valid in
	// both POSIX sh and PowerShell, so this stays platform-agnostic.
	result, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "echo oops; exit 1",
	})
	if err != nil {
		t.Fatalf("Execute should not return a Go error for non-zero exit: %v", err)
	}
	if !strings.Contains(result.Text, "oops") {
		t.Errorf("result should contain stdout: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[exit:") {
		t.Errorf("result should contain [exit:...]: %q", result.Text)
	}
}

func TestTerminalTool_Execute_EmptyCommand(t *testing.T) {
	_, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "",
	})
	if err == nil {
		t.Error("empty command should return error")
	}
}

func TestTerminalTool_Execute_NoCommandKey(t *testing.T) {
	_, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{})
	if err == nil {
		t.Error("missing command key should return error")
	}
}

func TestDefaultRegistry_Execute(t *testing.T) {
	r := DefaultRegistry{}

	result, err := r.Execute(context.Background(), "terminal", map[string]any{
		"command": "echo registry",
	})
	if err != nil {
		t.Fatalf("Execute terminal: %v", err)
	}
	if strings.TrimSpace(result.Text) != "registry" {
		t.Errorf("result = %q", result.Text)
	}
}

// Broader DefaultRegistry / DefaultTools assertions live in registry_test.go
// now that the registry hosts multiple tools.

// ─── ExecuteStream tests ────────────────────────────────────────────────────

func TestTerminalTool_ExecuteStream_LineByLine(t *testing.T) {
	var got []string
	progress := func(line string) { got = append(got, line) }

	result, err := TerminalTool{}.ExecuteStream(context.Background(), "terminal", map[string]any{
		"command": "echo line1 && echo line2 && echo line3",
	}, progress)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	if len(got) != 3 {
		t.Errorf("expected 3 progress callbacks, got %d: %v", len(got), got)
	}
	for i, want := range []string{"line1", "line2", "line3"} {
		if i < len(got) && got[i] != want {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want)
		}
	}
	if result.Text != "line1\nline2\nline3" {
		t.Errorf("aggregated result = %q", result.Text)
	}
}

func TestTerminalTool_ExecuteStream_MergedStdoutAndStderr(t *testing.T) {
	var got []string
	progress := func(line string) { got = append(got, line) }

	// Write to stdout AND stderr; both should reach progress in the order
	// the shell flushes them. (sh tends to be unbuffered enough for this
	// test to be stable but we don't assert ordering — just that both are
	// present.)
	_, err := TerminalTool{}.ExecuteStream(context.Background(), "terminal", map[string]any{
		"command": "echo from-stdout; echo from-stderr 1>&2",
	}, progress)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "from-stdout") || !strings.Contains(joined, "from-stderr") {
		t.Errorf("both streams should reach progress; got: %s", joined)
	}
}

func TestTerminalTool_ExecuteStream_NilProgressIsExecute(t *testing.T) {
	// Calling Execute (which internally passes nil) must produce the same
	// aggregated result as ExecuteStream with a non-nil progress.
	streamResult, _ := TerminalTool{}.ExecuteStream(context.Background(), "terminal",
		map[string]any{"command": "echo hi"}, func(string) {})
	execResult, _ := TerminalTool{}.Execute(context.Background(), "terminal",
		map[string]any{"command": "echo hi"})
	if streamResult.Text != execResult.Text {
		t.Errorf("stream=%q exec=%q — should match", streamResult.Text, execResult.Text)
	}
}

func TestTerminalTool_ExecuteStream_NonZeroExitPreservesContract(t *testing.T) {
	// Exit code != 0 must still surface as result text, not Go error,
	// so the LLM can read what happened.
	result, err := TerminalTool{}.ExecuteStream(context.Background(), "terminal",
		map[string]any{"command": "echo before-fail; exit 1"}, nil)
	if err != nil {
		t.Fatalf("non-zero exit should NOT be a Go error: %v", err)
	}
	if !strings.Contains(result.Text, "before-fail") {
		t.Errorf("output should include pre-exit stdout: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[exit:") {
		t.Errorf("output should include exit annotation: %q", result.Text)
	}
}

func TestTerminalTool_ContextCancel_KillsChild(t *testing.T) {
	// When the context is cancelled mid-run (e.g. user pressed Esc in the TUI),
	// the subprocess must be killed rather than left running.
	ctx, cancel := context.WithCancel(context.Background())

	// Start a long-running command that would sleep for 30s.
	go func() {
		// Cancel after a short delay so the test doesn't hang forever if the
		// fix is broken.
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result, err := TerminalTool{}.Execute(ctx, "terminal", map[string]any{
		"command": "sleep 30",
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute should not error on cancellation: %v", err)
	}
	// The result should mention the signal/kill, not a clean exit.
	if !strings.Contains(result.Text, "[exit:") {
		t.Errorf("result should contain [exit:...] after kill; got: %q", result.Text)
	}
	// Must finish well before 30s — the cancellation killed it.
	if elapsed > 2*time.Second {
		t.Errorf("took %s — context cancellation did not kill the child promptly", elapsed)
	}
}

// Compile-time assertion that TerminalTool satisfies StreamingToolExecutor —
// catches a regression at build time, not runtime.
var _ agent.StreamingToolExecutor = TerminalTool{}
