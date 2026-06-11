package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if _, ok := props["stdin"]; !ok {
		t.Error("Parameters.properties.stdin should exist — the executor reads it, so the schema must declare it or the model never sends it")
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

// ─── TerminalInputTool tests ────────────────────────────────────────────────

func TestTerminalInputTool_Definition(t *testing.T) {
	def := TerminalInputTool{}.Definition()
	if def.Name != "terminal_input" {
		t.Errorf("Name = %q, want terminal_input", def.Name)
	}
	req, ok := def.Parameters["required"].([]string)
	if !ok {
		t.Fatal("required is not []string")
	}
	if len(req) != 2 || req[0] != "id" || req[1] != "input" {
		t.Errorf("required = %v, want [id input]", req)
	}
}

func TestTerminalInputTool_SendInput(t *testing.T) {
	// Background commands run through the platform shell (sandbox.go's
	// shellCommand). On POSIX that's `sh -c`, which execs the binary so it
	// inherits stdin directly and reads the piped line deterministically.
	// On Windows it's `pwsh -NonInteractive -Command`, which consumes the
	// redirected stdin into PowerShell's own $input stream and does not
	// reliably forward it to the spawned native process — so the child's
	// blocking read sometimes never sees the input and the test flakes with
	// an empty result. The interactive-stdin path is only reliable on POSIX
	// shells, so assert it there and skip on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("terminal_input stdin delivery is non-deterministic through the pwsh -Command wrapper; POSIX-only assertion")
	}

	mgr := NewBackgroundManager()
	inputTool := TerminalInputTool{mgr: mgr}
	killTool := KillShellTool{mgr: mgr}

	// Use a small Go program as the test subject so it works on every OS
	// (Windows CI doesn't have head/sed). The program reads a line from
	// stdin and prints it back with a prefix.
	//
	// We compile it first with "go build" and then run the binary directly.
	// "go run" triggers a compilation on every launch; on Windows CI that
	// takes 1-3 s, which is longer than the 200 ms sleep below, so the
	// input arrives before the process is ready and is silently dropped.
	tmpDir := t.TempDir()
	prog := filepath.Join(tmpDir, "stdin_echo.go")
	src := `package main
import (
	"bufio"
	"fmt"
	"os"
)
func main() {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Printf("got: %s", line)
}
`
	if err := os.WriteFile(prog, []byte(src), 0644); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	bin := filepath.Join(tmpDir, "stdin_echo")
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	buildOut, err := exec.Command("go", "build", "-o", bin, prog).CombinedOutput()
	if err != nil {
		t.Fatalf("go build helper: %v\n%s", err, buildOut)
	}

	id, err := mgr.Start(bin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the program a moment to start and open its stdin reader.
	time.Sleep(200 * time.Millisecond)

	// Send input including a newline so the scanner can proceed.
	res, err := inputTool.Execute(context.Background(), "terminal_input", map[string]any{
		"id":    id,
		"input": "hello-world\n",
	})
	if err != nil {
		t.Fatalf("terminal_input: %v", err)
	}
	if !strings.Contains(res.Text, "Sent to") {
		t.Errorf("unexpected result: %q", res.Text)
	}

	// Wait for the process to finish, accumulating output as we poll.
	// Read is cursor-advancing: each call returns only the chunk produced
	// since the previous call, so the chunks must be concatenated — keeping
	// only the last one drops output that arrived before the exit was seen.
	var out string
	for i := 0; i < 100; i++ {
		chunk, status, _, _ := mgr.Read(id)
		out += chunk
		if strings.HasPrefix(status, "exited") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !strings.Contains(out, "got: hello-world") {
		t.Errorf("output should contain 'got: hello-world'; got: %q", out)
	}

	// Clean up (no-op if already exited).
	killTool.Execute(context.Background(), "kill_shell", map[string]any{"id": id})
}

func TestTerminalInputTool_ExitedProcess(t *testing.T) {
	mgr := NewBackgroundManager()
	inputTool := TerminalInputTool{mgr: mgr}

	// Start a trivial command that exits immediately.
	id, err := mgr.Start("echo done")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for it to exit.
	for i := 0; i < 50; i++ {
		_, status, _, _ := mgr.Read(id)
		if strings.HasPrefix(status, "exited") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Sending input to an exited process should error.
	_, err = inputTool.Execute(context.Background(), "terminal_input", map[string]any{
		"id":    id,
		"input": "too late\n",
	})
	if err == nil {
		t.Error("expected error when writing to exited process")
	}
	if !strings.Contains(err.Error(), "already exited") {
		t.Errorf("error should mention 'already exited'; got: %v", err)
	}
}

func TestTerminalInputTool_UnknownID(t *testing.T) {
	inputTool := TerminalInputTool{}
	_, err := inputTool.Execute(context.Background(), "terminal_input", map[string]any{
		"id":    "bg_99999",
		"input": "hello\n",
	})
	if err == nil {
		t.Error("expected error for unknown id")
	}
	if !strings.Contains(err.Error(), "no background process") {
		t.Errorf("error should mention 'no background process'; got: %v", err)
	}
}

func TestTerminalInputTool_EmptyInput(t *testing.T) {
	inputTool := TerminalInputTool{}
	_, err := inputTool.Execute(context.Background(), "terminal_input", map[string]any{
		"id":    "bg_1",
		"input": "",
	})
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestTerminalInputTool_MissingID(t *testing.T) {
	inputTool := TerminalInputTool{}
	_, err := inputTool.Execute(context.Background(), "terminal_input", map[string]any{
		"input": "hello\n",
	})
	if err == nil {
		t.Error("expected error for missing id")
	}
}

func TestTerminalTool_Stdin_PipedVerbatim(t *testing.T) {
	// PowerShell's Get-Content (aliased as cat) requires -Path when no
	// pipeline input. stdin delivery through the pwsh -Command wrapper
	// does not reliably reach the spawned child on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("stdin piping through PowerShell is non-deterministic; POSIX-only assertion")
	}

	// stdin text containing backticks, quotes, and special chars
	// must reach the process verbatim without shell interpretation.
	stdinBody := "line with `backticks` and \"double quotes\"\nsecond line\n"

	result, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "cat", // echoes stdin to stdout
		"stdin":   stdinBody,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != strings.TrimRight(stdinBody, "\n") {
		t.Errorf("stdin should reach process verbatim (trailing newline stripped by output trim).\ngot:  %q\nwant: %q", result.Text, strings.TrimRight(stdinBody, "\n"))
	}
}

func TestTerminalTool_Stdin_Background(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdin piping through PowerShell is non-deterministic; POSIX-only assertion")
	}

	mgr := NewBackgroundManager()
	term := TerminalTool{mgr: mgr}
	kill := KillShellTool{mgr: mgr}

	stdinBody := "hello stdin\n"

	res, err := term.ExecuteStream(context.Background(), "terminal", map[string]any{
		"command":           "cat",
		"run_in_background": true,
		"stdin":             stdinBody,
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteStream bg: %v", err)
	}
	if !strings.Contains(res.Text, "Started background process") {
		t.Fatalf("expected background start, got: %s", res.Text)
	}
	// Extract the process id from "Started background process <id>."
	id := strings.TrimPrefix(res.Text, "Started background process ")
	id = strings.SplitN(id, ".", 2)[0]

	// Wait for the process to finish, accumulating output as we poll —
	// Read is cursor-advancing, so each call returns only the new chunk.
	var out string
	exited := false
	for i := 0; i < 100; i++ {
		chunk, status, _, _ := mgr.Read(id)
		out += chunk
		if strings.HasPrefix(status, "exited") {
			exited = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !exited {
		t.Fatal("background cat did not exit — stdin EOF likely never delivered")
	}
	if out != stdinBody {
		t.Errorf("bg stdin should reach process verbatim.\ngot:  %q\nwant: %q", out, stdinBody)
	}

	kill.Execute(context.Background(), "kill_shell", map[string]any{"id": id})
}
