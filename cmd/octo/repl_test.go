package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

// stubSender is a deterministic Sender for REPL tests — returns a fixed reply
// for every call so tests don't touch the network.
type stubSender struct {
	reply  string
	err    error
	called int
}

func (s *stubSender) SendMessages(_ context.Context, _, _ string, _ []agent.Message, _ int) (agent.Reply, error) {
	s.called++
	return agent.Reply{Content: s.reply, InputTokens: 10, OutputTokens: 5}, s.err
}

func (s *stubSender) StreamMessages(
	_ context.Context,
	_, _ string,
	_ []agent.Message,
	_ int,
	onChunk func(string),
) (agent.Reply, error) {
	s.called++
	if onChunk != nil {
		onChunk(s.reply)
	}
	return agent.Reply{Content: s.reply, InputTokens: 10, OutputTokens: 5}, s.err
}

// makeREPLFixture returns a replConfig wired to a stubSender and in-memory
// buffers. HOME/USERPROFILE is redirected to a temp dir so session files don't
// pollute ~/.octo (USERPROFILE is needed for Windows where os.UserHomeDir()
// ignores HOME).
func makeREPLFixture(t *testing.T, input string) (replConfig, *bytes.Buffer, *bytes.Buffer, *stubSender) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	stub := &stubSender{reply: "pong"}
	a := agent.New(stub, "test-model")
	sess := agent.NewSession("test-model", "")

	var stdout, stderr bytes.Buffer
	cfg := replConfig{
		a:       a,
		session: sess,
		noSave:  false,
		stdin:   strings.NewReader(input),
		stdout:  &stdout,
		stderr:  &stderr,
	}
	return cfg, &stdout, &stderr, stub
}

func TestREPL_SingleTurn(t *testing.T) {
	cfg, stdout, stderr, stub := makeREPLFixture(t, "ping\n/exit\n")

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	if stub.called != 1 {
		t.Errorf("Sender called %d times, want 1", stub.called)
	}
	out := stdout.String()
	if !strings.Contains(out, "pong") {
		t.Errorf("stdout does not contain reply %q:\n%s", "pong", out)
	}
}

func TestREPL_MultiTurn(t *testing.T) {
	cfg, _, stderr, stub := makeREPLFixture(t, "one\ntwo\nthree\n/exit\n")

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	if stub.called != 3 {
		t.Errorf("Sender called %d times, want 3", stub.called)
	}
}

func TestREPL_EmptyLineSkipped(t *testing.T) {
	cfg, _, _, stub := makeREPLFixture(t, "\n\nhello\n/exit\n")

	runREPL(cfg)
	if stub.called != 1 {
		t.Errorf("Sender called %d times, want 1 (empty lines must be skipped)", stub.called)
	}
}

func TestREPL_HelpCommand(t *testing.T) {
	cfg, stdout, _, stub := makeREPLFixture(t, "/help\n/exit\n")

	runREPL(cfg)
	if stub.called != 0 {
		t.Errorf("Sender called %d times for /help, want 0", stub.called)
	}
	if !strings.Contains(stdout.String(), "/exit") {
		t.Error("stdout does not contain help text")
	}
}

func TestREPL_CostCommand(t *testing.T) {
	cfg, stdout, _, _ := makeREPLFixture(t, "hi\n/cost\n/exit\n")

	runREPL(cfg)
	out := stdout.String()
	if !strings.Contains(out, "Tokens:") {
		t.Errorf("stdout does not contain cost line:\n%s", out)
	}
}

func TestREPL_SaveCommand(t *testing.T) {
	cfg, stdout, stderr, _ := makeREPLFixture(t, "hi\n/save\n/exit\n")

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Saved →") {
		t.Errorf("stdout does not contain save confirmation:\n%s", out)
	}
}

func TestREPL_UnknownSlashCommand(t *testing.T) {
	cfg, stdout, _, stub := makeREPLFixture(t, "/bogus\n/exit\n")

	runREPL(cfg)
	if stub.called != 0 {
		t.Errorf("Sender called for unknown command")
	}
	if !strings.Contains(stdout.String(), "Unknown command") {
		t.Error("expected unknown command message")
	}
}

func TestREPL_EOFExitsCleanly(t *testing.T) {
	cfg, _, stderr, _ := makeREPLFixture(t, "") // EOF immediately

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("EOF exit code = %d, stderr: %s", code, stderr.String())
	}
}

func TestREPL_NoSave(t *testing.T) {
	cfg, stdout, _, _ := makeREPLFixture(t, "hi\n/exit\n")
	cfg.noSave = true

	runREPL(cfg)
	if strings.Contains(stdout.String(), "Session saved") {
		t.Error("expected no save message with --no-save")
	}
}

func TestREPL_AutoSaveAfterTurn(t *testing.T) {
	cfg, _, stderr, _ := makeREPLFixture(t, "hi\n/exit\n")

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	// If auto-save failed it writes to stderr.
	if strings.Contains(stderr.String(), "auto-save failed") {
		t.Errorf("auto-save failed: %s", stderr.String())
	}
}

func TestREPL_ResumedSessionShowsTurnCount(t *testing.T) {
	cfg, stdout, stderr, _ := makeREPLFixture(t, "/exit\n")
	// Pre-populate two turns in history to simulate a resumed session.
	cfg.a.History.Append(agent.NewUserMessage("old q"))
	cfg.a.History.Append(agent.NewAssistantMessage("old a"))
	cfg.session.SyncFrom(cfg.a.History)

	code := runREPL(cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Resumed") {
		t.Errorf("expected 'Resumed' in output:\n%s", out)
	}
	if !strings.Contains(out, "1 turn") {
		t.Errorf("expected '1 turn' in output:\n%s", out)
	}
}

// ─── replToolEventHandler tests ─────────────────────────────────────────────

func TestREPLToolEventHandler_TextOnly(t *testing.T) {
	var buf bytes.Buffer
	h := replToolEventHandler(&buf)
	h(agent.AgentEvent{Kind: agent.EventTextDelta, Text: "hello"})
	h(agent.AgentEvent{Kind: agent.EventTextDelta, Text: " world"})
	if got := buf.String(); got != "hello world" {
		t.Errorf("text deltas = %q, want 'hello world'", got)
	}
}

func TestREPLToolEventHandler_ToolStartedAndDone(t *testing.T) {
	var buf bytes.Buffer
	h := replToolEventHandler(&buf)
	h(agent.AgentEvent{Kind: agent.EventTextDelta, Text: "Running now..."})
	h(agent.AgentEvent{
		Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "terminal",
		Input: map[string]any{"command": "ls"},
	})
	h(agent.AgentEvent{
		Kind: agent.EventToolDone, ToolID: "c1", ToolName: "terminal",
	})
	out := buf.String()
	if !strings.Contains(out, "↳ terminal: command=ls") {
		t.Errorf("missing started line:\n%s", out)
	}
	if !strings.Contains(out, "↳ terminal ✓") {
		t.Errorf("missing done line:\n%s", out)
	}
	// "Running now..." should be followed by a newline before the ↳ line —
	// without it the status arrow would butt up against the dot.
	if !strings.Contains(out, "Running now...\n↳") {
		t.Errorf("expected newline between text and ↳:\n%q", out)
	}
}

func TestREPLToolEventHandler_ToolError(t *testing.T) {
	var buf bytes.Buffer
	h := replToolEventHandler(&buf)
	h(agent.AgentEvent{
		Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "write_file",
		Input: map[string]any{"path": "/etc/passwd"},
	})
	h(agent.AgentEvent{
		Kind: agent.EventToolError, ToolID: "c1", ToolName: "write_file",
		Err: "permission denied",
	})
	out := buf.String()
	if !strings.Contains(out, "↳ write_file ✗") {
		t.Errorf("missing error marker:\n%s", out)
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("error message not shown:\n%s", out)
	}
}

func TestREPLToolEventHandler_TurnDoneSilent(t *testing.T) {
	// EventTurnDone is intentionally not rendered — the REPL loop appends
	// its own trailing newline. This test locks that in so a well-meaning
	// future change doesn't accidentally double-print.
	var buf bytes.Buffer
	h := replToolEventHandler(&buf)
	reply := agent.Reply{Content: "all done"}
	h(agent.AgentEvent{Kind: agent.EventTurnDone, Reply: &reply})
	if buf.Len() != 0 {
		t.Errorf("EventTurnDone should be silent; got %q", buf.String())
	}
}

func TestSummariseInput_TruncatesLongValues(t *testing.T) {
	got := summariseInput(map[string]any{
		"path":    "short",
		"content": strings.Repeat("X", 200),
	})
	// Long value should be truncated with ellipsis.
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation marker, got %q", got)
	}
	// Output line itself capped at 120 chars.
	if len(got) > 120 {
		t.Errorf("line not capped: len=%d, got %q", len(got), got)
	}
}

func TestTruncate1Line_CollapsesMultiline(t *testing.T) {
	got := truncate1Line("\n\nfirst real line\nsecond line that gets dropped")
	if got != "first real line" {
		t.Errorf("truncate1Line = %q", got)
	}
}
