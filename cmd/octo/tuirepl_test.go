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
	if len(m.printlnBuf) != 1 {
		t.Fatalf("the echo should be queued to the scrollback, got %v", m.printlnBuf)
	}
}

// Reasoning deltas must commit to the scrollback (dimmed) before the answer,
// with the 💭 marker on the first line only — so the trace stays in the
// transcript above the reply instead of vanishing into a spinner.
func TestTUI_ThinkingRendersBeforeAnswer(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true

	// One complete thinking line, then a partial; the answer flushes the rest.
	m.handleEvent(agent.AgentEvent{Kind: agent.EventThinkingDelta, Text: "step one\n"})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventThinkingDelta, Text: "step two"})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventTextDelta, Text: "answer"})

	joined := strings.Join(m.printlnBuf, "\n")
	if !strings.Contains(joined, "step one") || !strings.Contains(joined, "step two") {
		t.Fatalf("thinking trace should be committed to scrollback; got %q", joined)
	}
	if n := strings.Count(joined, "💭"); n != 1 {
		t.Errorf("💭 should prefix only the first line of the block; got %d in %q", n, joined)
	}
	if m.thinkPartial.Len() != 0 {
		t.Errorf("thinking buffer should be flushed once the answer starts; still holds %q", m.thinkPartial.String())
	}
}

// A turn's agentic loop produces a fresh reasoning block before each model
// round-trip (between tool calls); every block must get its own 💭, not just
// the turn's first one.
func TestTUI_ThinkingMarkerPerBlock(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true

	// Block 1 → a tool runs → block 2 → the answer.
	m.handleEvent(agent.AgentEvent{Kind: agent.EventThinkingDelta, Text: "first thought\n"})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolStarted, ToolID: "c1", ToolName: "terminal", Input: map[string]any{"command": "ls"}})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventToolDone, ToolID: "c1", ToolName: "terminal", Output: "ok"})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventThinkingDelta, Text: "second thought\n"})
	m.handleEvent(agent.AgentEvent{Kind: agent.EventTextDelta, Text: "answer"})

	joined := strings.Join(m.printlnBuf, "\n")
	if n := strings.Count(joined, "💭"); n != 2 {
		t.Errorf("each reasoning block should get a 💭; got %d in:\n%s", n, joined)
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
