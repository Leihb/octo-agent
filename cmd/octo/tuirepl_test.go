package main

import (
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

// fakeProg records messages the sink sends, standing in for *tea.Program.
type fakeProg struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (f *fakeProg) Send(m tea.Msg) {
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()
}

func newTestModel() *tuiModel {
	a := agent.New(&stubSender{reply: "ok"}, "m")
	m := newTUIModel(replConfig{a: a, noSave: true})
	m.sink = &tuiSink{prog: &fakeProg{}}
	return m
}

func setInput(m *tuiModel, s string) {
	m.ta.SetValue(s)
	m.ta.CursorEnd()
}

func TestTUI_IdleEnterStartsTurn(t *testing.T) {
	m := newTestModel()
	setInput(m, "hello")
	_, _ = m.submit()
	if !m.turnRunning {
		t.Fatal("idle Enter should start a turn")
	}
	if m.ta.Value() != "" {
		t.Errorf("input should clear after submit, got %q", m.ta.Value())
	}
}

func TestTUI_RunningEnterEnqueues(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	setInput(m, "also handle errors")
	_, _ = m.submit() // Enter
	if !m.a.Inbox.HasPending() {
		t.Fatal("Enter while running should enqueue to inbox")
	}
	items := m.a.Inbox.Drain()
	if len(items) != 1 || items[0].Text != "also handle errors" {
		t.Errorf("inbox = %v", items)
	}
	if len(m.queue) != 0 {
		t.Errorf("inbox must not enqueue to queue, queue = %+v", m.queue)
	}
}

func TestTUI_CtrlXUnqueuesMostRecent(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	m.queue = []pendingItem{{text: "first"}, {text: "second"}}

	// Ctrl+X drops the most-recently queued item.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	if len(m.queue) != 1 || m.queue[0].text != "first" {
		t.Fatalf("Ctrl+X should drop the last queued item, got %+v", m.queue)
	}
	// Repeat clears the rest.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	if len(m.queue) != 0 {
		t.Fatalf("repeated Ctrl+X should empty the queue, got %+v", m.queue)
	}
	// Ctrl+X on an empty queue is a harmless no-op.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	if len(m.queue) != 0 {
		t.Errorf("Ctrl+X on empty queue should be a no-op")
	}
}

// Idle, the queue key behaves exactly like Enter (design §7): nothing would
// ever drain a queue with no running turn, so the message must start a turn
// immediately instead of being trapped.
func TestTUI_IdleCtrlQStartsTurn(t *testing.T) {
	m := newTestModel()
	setInput(m, "check recent PRs")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if !m.turnRunning {
		t.Fatal("idle Ctrl+Q should start a turn, same as Enter")
	}
	if len(m.queue) != 0 {
		t.Errorf("idle Ctrl+Q must not leave the message queued, got %+v", m.queue)
	}
	if m.ta.Value() != "" {
		t.Errorf("input should clear, got %q", m.ta.Value())
	}
}

func TestTUI_RunningCtrlQQueues(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	setInput(m, "run the linter")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if len(m.queue) != 1 || m.queue[0].text != "run the linter" {
		t.Fatalf("Ctrl+Q should queue, got %+v", m.queue)
	}
	if m.a.Inbox.HasPending() {
		t.Error("queue must not enqueue to inbox")
	}
}

func TestTUI_EscInterruptsRunningTurn(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	cancelled := false
	m.cancelTurn = func() { cancelled = true }
	m.queue = []pendingItem{{text: "later"}}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if !cancelled {
		t.Error("Esc should cancel the running turn")
	}
	if len(m.queue) != 1 {
		t.Error("Esc must NOT clear the queue (design §9)")
	}
	if m.quit {
		t.Error("Esc must not quit")
	}
}

// Esc before the model produces any output (echo still pending) takes the turn
// back: the typed text returns to the input box and the deferred echo never
// reaches the scrollback.
func TestTUI_EscTakesBackBeforeOutput(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	cancelled := false
	m.cancelTurn = func() { cancelled = true }
	m.echoPending = userEchoStyle.Render("> ") + "fix the bug"
	m.echoRestore = "fix the bug"

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if !cancelled {
		t.Error("Esc should cancel the running turn")
	}
	if m.ta.Value() != "fix the bug" {
		t.Errorf("typed text should return to the input box, got %q", m.ta.Value())
	}
	if m.echoPending != "" {
		t.Error("the deferred echo should be dropped, not committed")
	}
	if len(m.printlnBuf) != 0 {
		t.Errorf("nothing should be flushed to the scrollback, got %v", m.printlnBuf)
	}
}

// Once output has streamed the echo is already committed (echoPending == ""), so
// Esc just interrupts and leaves the input box untouched.
func TestTUI_EscAfterOutputJustInterrupts(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	cancelled := false
	m.cancelTurn = func() { cancelled = true }
	// echoPending already committed by the first output event.

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if !cancelled {
		t.Error("Esc should still interrupt once output has started")
	}
	if m.ta.Value() != "" {
		t.Errorf("input box should stay empty mid-stream, got %q", m.ta.Value())
	}
}

// The first streaming event promotes the deferred echo to the scrollback so the
// message lands just above the assistant reply.
func TestTUI_FirstOutputCommitsEcho(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	m.echoPending = userEchoStyle.Render("> ") + "hello"
	m.echoRestore = "hello"

	m.handleEvent(agent.AgentEvent{Kind: agent.EventTextDelta, Text: "hi"})

	if m.echoPending != "" {
		t.Error("first output should commit (clear) the deferred echo")
	}
	// printlnBlock prefixes the echo with one blank separator line.
	if len(m.printlnBuf) == 0 || !strings.Contains(m.printlnBuf[len(m.printlnBuf)-1], "hello") {
		t.Fatalf("the echo should be queued to the scrollback, got %v", m.printlnBuf)
	}
}

// While the model streams a large tool argument (e.g. a big write_file body),
// the view must show a live byte counter so the wait reads as progress, not a
// freeze. EventToolStarted (args complete) clears the readout.
func TestTUI_ToolInputStreamProgress(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true

	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolInputDelta, ToolID: "c1", ToolName: "write_file", InputDelta: strings.Repeat("x", 2048)})
	if m.toolStreamName != "write_file" || m.toolStreamBytes != 2048 {
		t.Fatalf("after first delta: name=%q bytes=%d", m.toolStreamName, m.toolStreamBytes)
	}
	out := m.View()
	if !strings.Contains(out, "Writing") || !strings.Contains(out, "2.0 KB") {
		t.Errorf("view should show a live write progress line; got:\n%s", out)
	}

	// A second fragment of the same call accumulates.
	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolInputDelta, ToolID: "c1", ToolName: "write_file", InputDelta: strings.Repeat("x", 1024)})
	if m.toolStreamBytes != 3072 {
		t.Fatalf("bytes should accumulate within one call, got %d", m.toolStreamBytes)
	}

	// Args complete → tool dispatches → readout clears.
	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "write_file", Input: map[string]any{"path": "x.html"}})
	if m.toolStreamName != "" || m.toolStreamBytes != 0 {
		t.Errorf("EventToolStarted should clear the stream readout; name=%q bytes=%d", m.toolStreamName, m.toolStreamBytes)
	}
}

// The reasoning trace stays out of the scrollback — only its size feeds the
// live activity line's "↑ ~N tokens" readout, so a long agentic turn doesn't
// fill the transcript with thinking text.
func TestTUI_ThinkingStaysOutOfScrollback(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true

	m.handleEvent(agent.AgentEvent{Kind: agent.EventThinkingDelta, Text: "step one\n"})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventThinkingDelta, Text: strings.Repeat("x", 3999)})

	if joined := strings.Join(m.printlnBuf, "\n"); joined != "" {
		t.Fatalf("thinking must not reach the scrollback; got %q", joined)
	}
	if m.turnOutChars != 4008 {
		t.Errorf("turnOutChars = %d, want 4008", m.turnOutChars)
	}
	// The wait-on-model activity line shows the running token estimate (chars/4).
	out := m.View()
	if !strings.Contains(out, "↑ ~1.0k tokens") {
		t.Errorf("view should show the output-token readout; got:\n%s", out)
	}
}

// A new turn resets the output-token readout.
func TestTUI_TurnStartResetsOutChars(t *testing.T) {
	m := newTestModel()
	m.turnOutChars = 999
	_ = m.startTurnEcho("hi", "hi")
	if m.turnOutChars != 0 {
		t.Errorf("turnOutChars after turn start = %d, want 0", m.turnOutChars)
	}
}

// Non-card tools (MCP tools, sub_agent, …) commit a single header-style status
// line on completion — no started line, no progress lines.
func TestTUI_NonCardToolOneLine(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true

	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "mcp_thing", Input: map[string]any{"q": "x"}})
	if len(m.printlnBuf) != 0 {
		t.Fatalf("started must not print; got %v", m.printlnBuf)
	}
	if m.running == nil || m.running.verb != "mcp_thing" {
		t.Fatalf("started should set the live indicator; got %+v", m.running)
	}
	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolProgress, ToolID: "c1", ToolName: "mcp_thing", Chunk: "partial"})
	if len(m.printlnBuf) != 0 {
		t.Fatalf("progress must not print; got %v", m.printlnBuf)
	}
	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "mcp_thing", Output: "done"})

	joined := stripANSI(strings.Join(m.printlnBuf, "\n"))
	if !strings.Contains(joined, "● mcp_thing(q=x)") {
		t.Errorf("done should commit one status line; got %q", joined)
	}
	if m.running != nil {
		t.Error("done should clear the live indicator")
	}
}

// Block-level scrollback items are separated by exactly one blank line; the
// separator is not duplicated when the previous line is already blank.
func TestTUI_PrintlnBlockSeparation(t *testing.T) {
	m := newTestModel()
	m.printlnBlock("first")
	m.printlnBlock("second")
	want := []string{"", "first", "", "second"}
	if len(m.printlnBuf) != len(want) {
		t.Fatalf("printlnBuf = %q, want %q", m.printlnBuf, want)
	}
	for i, w := range want {
		if m.printlnBuf[i] != w {
			t.Fatalf("printlnBuf[%d] = %q, want %q", i, m.printlnBuf[i], w)
		}
	}
}

// Resuming a session replays prior turns into the scrollback: prompts and
// replies verbatim, tool calls collapsed to a one-line ✓/✗ (no raw output),
// closed by a dim "resumed" marker.
func TestTUI_ReplayHistoryLines(t *testing.T) {
	m := newTestModel()
	m.width = 80
	h := m.a.History
	h.Append(agent.NewUserMessage("hello there"))
	h.Append(agent.NewToolUseMessage([]agent.ContentBlock{
		agent.NewTextBlock("let me check"),
		agent.NewToolUseBlock("c1", "terminal", map[string]any{"command": "ls"}),
	}))
	h.Append(agent.NewToolResultMessage([]agent.ContentBlock{
		agent.NewToolResultBlock("c1", "file1\nfile2", false),
	}))
	h.Append(agent.NewAssistantMessage("here are the files"))

	joined := stripANSI(strings.Join(m.replayHistoryLines(), "\n"))

	for _, want := range []string{"hello there", "let me check", "here are the files", "terminal", "✓", "resumed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("replay missing %q; got:\n%s", want, joined)
		}
	}
	// The collapsed tool line must NOT include the raw tool output.
	if strings.Contains(joined, "file1") {
		t.Errorf("raw tool output should be collapsed away; got:\n%s", joined)
	}
}

// stripANSI removes ANSI SGR escape sequences so assertions can match the
// underlying text (glamour splits words across color spans).
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func TestTUI_ReplayHistoryLines_FreshSessionEmpty(t *testing.T) {
	m := newTestModel()
	if lines := m.replayHistoryLines(); lines != nil {
		t.Errorf("a fresh session should replay nothing; got %v", lines)
	}
}

// A failed tool call collapses to a ✗ line.
func TestTUI_ReplayHistoryLines_ToolError(t *testing.T) {
	m := newTestModel()
	m.width = 80
	h := m.a.History
	h.Append(agent.NewUserMessage("run it"))
	h.Append(agent.NewToolUseMessage([]agent.ContentBlock{
		agent.NewToolUseBlock("c1", "terminal", map[string]any{"command": "boom"}),
	}))
	h.Append(agent.NewToolResultMessage([]agent.ContentBlock{
		agent.NewToolResultBlock("c1", "exit 1", true),
	}))

	joined := strings.Join(m.replayHistoryLines(), "\n")
	if !strings.Contains(joined, "✗") {
		t.Errorf("failed tool should collapse to a ✗ line; got:\n%s", joined)
	}
}

func TestTUI_CtrlDQuits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !m.quit || cmd == nil {
		t.Error("Ctrl+D should set quit and return a quit cmd")
	}
}

func TestTUI_TurnFinishedDequeuesNext(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	m.queue = []pendingItem{{text: "next task"}}

	_, _ = m.handleTurnFinished()

	// Dequeued and a new turn started.
	if !m.turnRunning {
		t.Fatal("a queued item should start the next turn")
	}
	if len(m.queue) != 0 {
		t.Errorf("queue should be drained, got %+v", m.queue)
	}
}

func TestTUI_DegradedInboxRunsBeforeQueue(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	m.a.Inbox.Enqueue("degraded inbox") // never drained during turn → pending at turn end
	m.queue = []pendingItem{{text: "queued"}}

	_, _ = m.handleTurnFinished()

	// The degraded inbox message is prepended, so it's the one that starts running and
	// "queued" remains in the queue.
	if len(m.queue) != 1 || m.queue[0].text != "queued" {
		t.Fatalf("queue after dequeue = %+v, want [queued]", m.queue)
	}
	if m.a.Inbox.HasPending() {
		t.Error("inbox should have been drained into the queue")
	}
}

func TestTUI_PermissionModal(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{Kind: KindPermission, ToolName: "terminal"}, resp: resp})
	if m.modal == nil {
		t.Fatal("modal should be open")
	}

	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	got := <-resp
	if !got.Allow || !got.Always {
		t.Errorf("'a' should allow+always, got %+v", got)
	}
	if m.modal != nil {
		t.Error("modal should close after answering")
	}
}

func TestTUI_PermissionModalEscDenies(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{Kind: KindPermission, ToolName: "terminal"}, resp: resp})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEsc})
	if got := <-resp; got.Allow {
		t.Errorf("Esc should deny, got %+v", got)
	}
}

func TestTUI_QuestionModalSelect(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{
		Kind:     KindQuestion,
		Question: "pick",
		Options:  []string{"alpha", "beta"},
	}, resp: resp})

	// Move to "beta" and confirm.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	got := <-resp
	if len(got.Choices) != 1 || got.Choices[0] != "beta" {
		t.Errorf("selection = %+v, want [beta]", got.Choices)
	}
}

func TestTUI_QuestionModalMultiSelect(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{
		Kind:        KindQuestion,
		Question:    "pick many",
		Options:     []string{"a", "b", "c"},
		MultiSelect: true,
	}, resp: resp})

	// Toggle index 0, move to 2, toggle, confirm.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeySpace})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeySpace})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	got := <-resp
	if len(got.Choices) != 2 || got.Choices[0] != "a" || got.Choices[1] != "c" {
		t.Errorf("multi-select = %+v, want [a c]", got.Choices)
	}
}

func TestTUI_QuestionModalOtherFreeText(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{
		Kind:     KindQuestion,
		Question: "pick",
		Options:  []string{"alpha", "beta"},
	}, resp: resp})

	// Move to "Other" and confirm to enter free-text mode.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.modal.otherActive {
		t.Fatal("selecting Other should activate free-text input")
	}

	// Type "custom", backspace once, then confirm.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c', 'u', 's', 't', 'o', 'm'}})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyBackspace})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	got := <-resp
	if got.Custom != "custo" {
		t.Errorf("custom = %q, want custo", got.Custom)
	}
	if got.Cancelled {
		t.Error("should not be cancelled after confirming non-empty text")
	}
}

func TestTUI_QuestionModalOtherEmptyCancels(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{
		Kind:     KindQuestion,
		Question: "pick",
		Options:  []string{"alpha"},
	}, resp: resp})

	// Select Other (Enter), then confirm empty input (Enter) to cancel.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	got := <-resp
	if !got.Cancelled {
		t.Errorf("empty Other should cancel, got %+v", got)
	}
}

func TestTUI_QuestionModalOtherEscCancels(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{
		Kind:     KindQuestion,
		Question: "pick",
		Options:  []string{"alpha"},
	}, resp: resp})

	// Select Other (Enter), type something, then press Esc.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEsc})

	got := <-resp
	if !got.Cancelled {
		t.Errorf("Esc in Other input should cancel, got %+v", got)
	}
}

func TestTUI_QuestionModalMultiSelectOther(t *testing.T) {
	m := newTestModel()
	resp := make(chan UserResponse, 1)
	m.openModal(askMsg{prompt: UserPrompt{
		Kind:        KindQuestion,
		Question:    "pick",
		Options:     []string{"alpha", "beta"},
		MultiSelect: true,
	}, resp: resp})

	// Toggle Other and confirm to enter free-text mode.
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeySpace})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.modal.otherActive {
		t.Fatal("selecting Other in multi-select should activate free-text input")
	}

	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m', 'y'}})
	_, _ = m.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})

	got := <-resp
	if got.Custom != "my" {
		t.Errorf("custom = %q, want my", got.Custom)
	}
}

func TestTUI_AppendText_BlockBufferingNonPlain(t *testing.T) {
	m := newTestModel() // cfg.plain == false → markdown block buffering
	m.appendText("hello ")
	m.appendText("world\nnext")
	// No blank-line block boundary yet, so the whole in-progress block stays live.
	if got := m.partial.String(); got != "hello world\nnext" {
		t.Errorf("partial = %q, want the full in-progress block", got)
	}
	// A blank line closes the block; only the next block remains live.
	m.appendText("\n\nsecond para start")
	if got := m.partial.String(); got != "second para start" {
		t.Errorf("partial = %q, want 'second para start'", got)
	}
}

func TestTUI_AppendText_PlainLineFlush(t *testing.T) {
	m := newTestModel()
	m.cfg.plain = true // --plain keeps the line-by-line raw flush
	m.appendText("hello ")
	m.appendText("world\nnext")
	if got := m.partial.String(); got != "next" {
		t.Errorf("partial = %q, want 'next' (completed line flushed)", got)
	}
}

func TestTUI_InputHistoryRecall(t *testing.T) {
	m := newTestModel()
	m.inputHistory = []string{"first", "second", "third"}

	// Up recalls the most recent line.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.ta.Value(); got != "third" {
		t.Errorf("input after first Up = %q, want third", got)
	}
	if m.inputHistoryIdx != 0 {
		t.Errorf("history idx = %d, want 0", m.inputHistoryIdx)
	}

	// Up again goes further back.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.ta.Value(); got != "second" {
		t.Errorf("input after second Up = %q, want second", got)
	}

	// Down moves forward in history.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.ta.Value(); got != "third" {
		t.Errorf("input after Down = %q, want third", got)
	}

	// Down past the newest restores empty input.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.ta.Value() != "" {
		t.Errorf("input after Down past newest = %q, want empty", m.ta.Value())
	}
	if m.inputHistoryIdx != -1 {
		t.Errorf("history idx = %d, want -1", m.inputHistoryIdx)
	}
}

func TestTUI_SubmitSavesToHistory(t *testing.T) {
	m := newTestModel()
	setInput(m, "hello")
	_, _ = m.submit()
	if len(m.inputHistory) != 1 || m.inputHistory[0] != "hello" {
		t.Errorf("history after submit = %v, want [hello]", m.inputHistory)
	}

	// Duplicate consecutive line is deduped.
	setInput(m, "hello")
	_, _ = m.submit()
	if len(m.inputHistory) != 1 {
		t.Errorf("history after duplicate = %v, want still 1 entry", m.inputHistory)
	}

	// Different line is added.
	setInput(m, "world")
	_, _ = m.submit()
	if len(m.inputHistory) != 2 || m.inputHistory[1] != "world" {
		t.Errorf("history after new line = %v, want [hello world]", m.inputHistory)
	}
}
