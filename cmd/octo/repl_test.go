package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/memory"
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

// Slash commands (/help, /cost, /save, unknown-command handling, skill
// triggers) now live exclusively in the TUI — see dispatchSlash and its tests
// in tuirepl_slash_test.go. The plain REPL keeps only /exit (+ /quit alias),
// covered by the conversation-loop tests above.

func TestREPL_PlainSlashIsJustText(t *testing.T) {
	// In the plain REPL every leading-"/" line except /exit is ordinary message
	// text, so it reaches the model rather than being intercepted.
	cfg, _, _, stub := makeREPLFixture(t, "/cost\n/exit\n")

	runREPL(cfg)
	if stub.called != 1 {
		t.Errorf("Sender called %d times; /cost should be sent as a message in plain mode, want 1", stub.called)
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
	h := replToolEventHandler(&buf, false)
	h(agent.AgentEvent{Kind: agent.EventTextDelta, Text: "hello"})
	h(agent.AgentEvent{Kind: agent.EventTextDelta, Text: " world"})
	if got := buf.String(); got != "hello world" {
		t.Errorf("text deltas = %q, want 'hello world'", got)
	}
}

func TestREPLToolEventHandler_ToolStartedAndDone(t *testing.T) {
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
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
	h := replToolEventHandler(&buf, false)
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
	h := replToolEventHandler(&buf, false)
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

func TestREPLToolEventHandler_EditFilePlainOneLiner(t *testing.T) {
	// Cards are TUI-only now (design decision #8): the plain / non-TTY path
	// renders edit_file as a terse ↳ status line like any other tool — no
	// Update() diff card.
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
	input := map[string]any{
		"path":       "/tmp/nope-this-file-does-not-exist.go",
		"old_string": "alpha",
		"new_string": "beta",
	}
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "edit_file", Input: input})
	h(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "edit_file"})

	out := buf.String()
	if !strings.Contains(out, "↳ edit_file:") {
		t.Errorf("expected the started ↳ line for edit_file:\n%s", out)
	}
	if !strings.Contains(out, "↳ edit_file ✓") {
		t.Errorf("expected the done ↳ line for edit_file:\n%s", out)
	}
	if strings.Contains(out, "Update(") {
		t.Errorf("plain path must NOT render a diff card (cards are TUI-only):\n%s", out)
	}
}

func TestREPLToolEventHandler_ToolProgressRendersBetweenStartedAndDone(t *testing.T) {
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
	h(agent.AgentEvent{
		Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "terminal",
		Input: map[string]any{"command": "ls"},
	})
	h(agent.AgentEvent{Kind: agent.EventToolProgress, ToolID: "c1", ToolName: "terminal", Chunk: "first line"})
	h(agent.AgentEvent{Kind: agent.EventToolProgress, ToolID: "c1", ToolName: "terminal", Chunk: "second line"})
	h(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "terminal"})

	out := buf.String()
	if !strings.Contains(out, "│ first line\n│ second line\n") {
		t.Errorf("expected progress chunks rendered with │ prefix:\n%s", out)
	}
	// Order: started → progress → done.
	startedIdx := strings.Index(out, "↳ terminal:")
	progressIdx := strings.Index(out, "│ first line")
	doneIdx := strings.Index(out, "↳ terminal ✓")
	if !(startedIdx < progressIdx && progressIdx < doneIdx) {
		t.Errorf("wrong event ordering: started=%d progress=%d done=%d\n%s",
			startedIdx, progressIdx, doneIdx, out)
	}
}

func TestREPLToolEventHandler_PlainPathUniformNoCards(t *testing.T) {
	// With cards now TUI-only, the plain path treats edit_file like any tool:
	// progress chunks render with the │ prefix and there is no diff card.
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
	input := map[string]any{
		"path": "/tmp/x.go", "old_string": "a", "new_string": "b",
	}
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "edit_file", Input: input})
	h(agent.AgentEvent{Kind: agent.EventToolProgress, ToolID: "c1", ToolName: "edit_file", Chunk: "noisy line"})
	h(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "edit_file"})

	out := buf.String()
	if !strings.Contains(out, "│ noisy line") {
		t.Errorf("progress should render in the plain path (no card to defer to):\n%s", out)
	}
	if strings.Contains(out, "Update(") {
		t.Errorf("plain path must NOT render a diff card:\n%s", out)
	}
}

func TestREPLToolEventHandler_InputDeltaEmitsTypingIndicator(t *testing.T) {
	// A few input_delta events before tool_started should emit a typing
	// indicator line that closes when tool_started fires.
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
	for i := 0; i < 3; i++ {
		h(agent.AgentEvent{Kind: agent.EventToolInputDelta, ToolID: "c1", ToolName: "terminal", InputDelta: "{frag}"})
	}
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "terminal", Input: map[string]any{"command": "ls"}})

	out := buf.String()
	if !strings.HasPrefix(out, "⋯ ···\n") {
		t.Errorf("expected typing indicator before ↳ line, got:\n%q", out)
	}
	if !strings.Contains(out, "↳ terminal: command=ls") {
		t.Errorf("expected ↳ line after typing indicator:\n%s", out)
	}
}

func TestREPLToolEventHandler_InputDeltaCappedAtDotsLimit(t *testing.T) {
	// Pump in more deltas than the visual cap — only inputDotsCap dots
	// should ever be emitted before tool_started closes the line.
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
	for i := 0; i < inputDotsCap+50; i++ {
		h(agent.AgentEvent{Kind: agent.EventToolInputDelta, ToolID: "c1", ToolName: "terminal", InputDelta: "x"})
	}
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "terminal", Input: nil})

	out := buf.String()
	dotCount := strings.Count(out, "·")
	if dotCount != inputDotsCap {
		t.Errorf("dotCount = %d, want %d (cap)", dotCount, inputDotsCap)
	}
}

func TestREPLToolEventHandler_InputDeltaResetsBetweenCalls(t *testing.T) {
	// After tool_started closes the dots, a NEXT tool's input deltas
	// should start a fresh indicator (not append to the previous one).
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, false)
	h(agent.AgentEvent{Kind: agent.EventToolInputDelta, ToolID: "c1", ToolName: "terminal", InputDelta: "a"})
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "terminal", Input: nil})
	h(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "terminal"})
	h(agent.AgentEvent{Kind: agent.EventToolInputDelta, ToolID: "c2", ToolName: "terminal", InputDelta: "b"})
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c2", ToolName: "terminal", Input: nil})

	out := buf.String()
	// Two separate typing-indicator lines should appear (one dot each).
	if strings.Count(out, "⋯ ·\n") != 2 {
		t.Errorf("expected two independent typing indicators, got:\n%q", out)
	}
}

func TestREPLToolEventHandler_PlainForcesEditFileToStatusLine(t *testing.T) {
	// With plain=true, even edit_file should use the ↳ status path so
	// users on constrained terminals (no truecolor, log scraping, etc.)
	// get the terse output.
	var buf bytes.Buffer
	h := replToolEventHandler(&buf, true)
	input := map[string]any{
		"path":       "/tmp/whatever.go",
		"old_string": "alpha",
		"new_string": "beta",
	}
	h(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "edit_file", Input: input})
	h(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "edit_file"})

	out := buf.String()
	if !strings.Contains(out, "↳ edit_file:") {
		t.Errorf("plain mode should emit ↳ started line for edit_file:\n%s", out)
	}
	if !strings.Contains(out, "↳ edit_file ✓") {
		t.Errorf("plain mode should emit ↳ done line for edit_file:\n%s", out)
	}
	if strings.Contains(out, "Update(") {
		t.Errorf("plain mode should NOT render a card:\n%s", out)
	}
}

func TestREPL_QuietMode_SuppressesChrome(t *testing.T) {
	cfg, stdout, _, _ := makeREPLFixture(t, "ping\n/exit\n")
	cfg.verbosity = verbosityQuiet

	if code := runREPL(cfg); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	// Quiet mode strips: the "Starting session ..." banner, the "to quit"
	// hint, and the "Session saved → ..." footer. The model reply itself
	// must still come through (otherwise quiet would be useless).
	for _, banned := range []string{"Starting session", "to quit", "Session saved"} {
		if strings.Contains(out, banned) {
			t.Errorf("quiet mode should not emit %q:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "pong") {
		t.Errorf("quiet mode must still print the model reply; got:\n%s", out)
	}
}

func TestREPL_VerboseMode_PrintsModelAndHints(t *testing.T) {
	cfg, stdout, _, _ := makeREPLFixture(t, "ping\n/exit\n")
	cfg.verbosity = verbosityVerbose

	if code := runREPL(cfg); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	// Verbose mode adds a `model:` line under the banner. permissions/tools
	// lines only render when configured (not in this fixture).
	if !strings.Contains(out, "model: test-model") {
		t.Errorf("verbose mode should print model line; got:\n%s", out)
	}
}

// capturingSender records the messages each Send call received, so tests
// can inspect what was actually sent over the wire (e.g., whether the
// memory-nudge reminder was appended to the user input).
type capturingSender struct {
	stubSender
	lastMessages []agent.Message
}

func (c *capturingSender) SendMessages(ctx context.Context, sys, model string, msgs []agent.Message, maxTokens int) (agent.Reply, error) {
	c.lastMessages = msgs
	return c.stubSender.SendMessages(ctx, sys, model, msgs, maxTokens)
}

func (c *capturingSender) StreamMessages(ctx context.Context, sys, model string, msgs []agent.Message, maxTokens int, onChunk func(string)) (agent.Reply, error) {
	c.lastMessages = msgs
	return c.stubSender.StreamMessages(ctx, sys, model, msgs, maxTokens, onChunk)
}

// lastUserContent returns the textual Content of the last user message
// sent — that's where the memory-nudge reminder lands when active.
func (c *capturingSender) lastUserContent() string {
	for i := len(c.lastMessages) - 1; i >= 0; i-- {
		if c.lastMessages[i].Role == agent.RoleUser {
			return c.lastMessages[i].Content
		}
	}
	return ""
}

func TestREPL_MemoryNudge_AppendedWhenMemoryAndToolsActive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	cap := &capturingSender{stubSender: stubSender{reply: "ok"}}
	a := agent.New(cap, "test-model")

	// Active memory store + a non-empty tools list = nudge should fire.
	store := memory.NewStoreAt(t.TempDir())

	var stdout, stderr bytes.Buffer
	cfg := replConfig{
		a:        a,
		session:  agent.NewSession("test-model", ""),
		stdin:    strings.NewReader("hi\n/exit\n"),
		stdout:   &stdout,
		stderr:   &stderr,
		memStore: store,
		// A dummy tool def is enough to satisfy `len(cfg.tools) > 0`.
		tools:    []agent.ToolDefinition{{Name: "dummy", Description: "x", Parameters: map[string]any{"type": "object"}}},
		executor: dummyExecutor{},
	}
	if code := runREPL(cfg); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	got := cap.lastUserContent()
	if !strings.Contains(got, "Memory hygiene check") {
		t.Errorf("expected nudge in user message, got:\n%s", got)
	}
	if !strings.Contains(got, "hi") {
		t.Errorf("user input lost from message body:\n%s", got)
	}
}

func TestREPL_MemoryNudge_AbsentWhenMemoryDisabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	cap := &capturingSender{stubSender: stubSender{reply: "ok"}}
	a := agent.New(cap, "test-model")

	var stdout, stderr bytes.Buffer
	cfg := replConfig{
		a:        a,
		session:  agent.NewSession("test-model", ""),
		stdin:    strings.NewReader("hi\n/exit\n"),
		stdout:   &stdout,
		stderr:   &stderr,
		memStore: nil, // ← memory off
		tools:    []agent.ToolDefinition{{Name: "dummy", Description: "x", Parameters: map[string]any{"type": "object"}}},
		executor: dummyExecutor{},
	}
	if code := runREPL(cfg); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := cap.lastUserContent(); strings.Contains(got, "Memory hygiene check") {
		t.Errorf("nudge should be absent when memory is off; got:\n%s", got)
	}
}

func TestREPL_MemoryNudge_AbsentWhenToolsOff(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	cap := &capturingSender{stubSender: stubSender{reply: "ok"}}
	a := agent.New(cap, "test-model")

	store := memory.NewStoreAt(t.TempDir())

	var stdout, stderr bytes.Buffer
	cfg := replConfig{
		a:        a,
		session:  agent.NewSession("test-model", ""),
		stdin:    strings.NewReader("hi\n/exit\n"),
		stdout:   &stdout,
		stderr:   &stderr,
		memStore: store,
		// tools empty: the remember tool can't be called, so the nudge
		// is pointless and should be omitted.
	}
	if code := runREPL(cfg); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := cap.lastUserContent(); strings.Contains(got, "Memory hygiene check") {
		t.Errorf("nudge should be absent when no tools are advertised; got:\n%s", got)
	}
}

// dummyExecutor satisfies the agent.ToolExecutor interface for tests
// that need cfg.tools to be non-empty but don't actually invoke any tools.
type dummyExecutor struct{}

func (dummyExecutor) Execute(_ context.Context, name string, _ map[string]any) (string, error) {
	return "ok:" + name, nil
}
