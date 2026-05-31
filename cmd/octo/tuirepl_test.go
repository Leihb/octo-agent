package main

import (
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
	m.ti.SetValue(s)
	m.ti.CursorEnd()
}

func TestTUI_IdleEnterStartsTurn(t *testing.T) {
	m := newTestModel()
	setInput(m, "hello")
	_, _ = m.submit(false)
	if !m.turnRunning {
		t.Fatal("idle Enter should start a turn")
	}
	if m.ti.Value() != "" {
		t.Errorf("input should clear after submit, got %q", m.ti.Value())
	}
}

func TestTUI_RunningEnterSteers(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	setInput(m, "also handle errors")
	_, _ = m.submit(false) // Enter
	if !m.a.HasPendingSteer() {
		t.Fatal("Enter while running should steer the agent")
	}
	if got := m.a.DrainSteer(); got != "also handle errors" {
		t.Errorf("steer = %q", got)
	}
	if len(m.queue) != 0 {
		t.Errorf("steer must not enqueue, queue = %+v", m.queue)
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

func TestTUI_RunningAltEnterQueues(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	setInput(m, "run the linter")
	_, _ = m.submit(true) // Alt+Enter
	if len(m.queue) != 1 || m.queue[0].text != "run the linter" {
		t.Fatalf("Alt+Enter should queue, got %+v", m.queue)
	}
	if m.a.HasPendingSteer() {
		t.Error("queue must not steer the agent")
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

func TestTUI_DegradedSteerRunsBeforeQueue(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	m.a.Steer("degraded steer") // never injected → pending at turn end
	m.queue = []pendingItem{{text: "queued"}}

	_, _ = m.handleTurnFinished()

	// The degraded steer is prepended, so it's the one that starts running and
	// "queued" remains in the queue.
	if len(m.queue) != 1 || m.queue[0].text != "queued" {
		t.Fatalf("queue after dequeue = %+v, want [queued]", m.queue)
	}
	if m.a.HasPendingSteer() {
		t.Error("steer should have been drained into the queue")
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
	if got := m.ti.Value(); got != "third" {
		t.Errorf("input after first Up = %q, want third", got)
	}
	if m.inputHistoryIdx != 0 {
		t.Errorf("history idx = %d, want 0", m.inputHistoryIdx)
	}

	// Up again goes further back.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.ti.Value(); got != "second" {
		t.Errorf("input after second Up = %q, want second", got)
	}

	// Down moves forward in history.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.ti.Value(); got != "third" {
		t.Errorf("input after Down = %q, want third", got)
	}

	// Down past the newest restores empty input.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.ti.Value() != "" {
		t.Errorf("input after Down past newest = %q, want empty", m.ti.Value())
	}
	if m.inputHistoryIdx != -1 {
		t.Errorf("history idx = %d, want -1", m.inputHistoryIdx)
	}
}

func TestTUI_SubmitSavesToHistory(t *testing.T) {
	m := newTestModel()
	setInput(m, "hello")
	_, _ = m.submit(false)
	if len(m.inputHistory) != 1 || m.inputHistory[0] != "hello" {
		t.Errorf("history after submit = %v, want [hello]", m.inputHistory)
	}

	// Duplicate consecutive line is deduped.
	setInput(m, "hello")
	_, _ = m.submit(false)
	if len(m.inputHistory) != 1 {
		t.Errorf("history after duplicate = %v, want still 1 entry", m.inputHistory)
	}

	// Different line is added.
	setInput(m, "world")
	_, _ = m.submit(false)
	if len(m.inputHistory) != 2 || m.inputHistory[1] != "world" {
		t.Errorf("history after new line = %v, want [hello world]", m.inputHistory)
	}
}

func TestTUI_StickyFollowsContent(t *testing.T) {
	m := newTestModel()
	m.height = 30

	for i := 0; i < 30; i++ {
		m.pushScrollback("old line")
	}

	// Sticky=true (default on new turn): pushScrollback keeps view at bottom.
	m.sticky = true
	m.scrollOffset = 0
	m.pushScrollback("new content")
	if m.scrollOffset != 0 {
		t.Errorf("sticky pushScrollback should keep scrollOffset 0, got %d", m.scrollOffset)
	}

	// Sticky=true with non-zero scrollOffset: pull back to bottom.
	m.scrollOffset = 10
	m.pushScrollback("another line")
	if m.scrollOffset != 0 {
		t.Errorf("sticky pushScrollback should reset scrollOffset to 0, got %d", m.scrollOffset)
	}
}

func TestTUI_NotStickyPreservesPosition(t *testing.T) {
	m := newTestModel()
	m.height = 30

	for i := 0; i < 30; i++ {
		m.pushScrollback("old line")
	}

	// sticky=false (user has scrolled): pushScrollback preserves position.
	m.sticky = false
	m.scrollOffset = 5
	m.pushScrollback("new content")
	if m.scrollOffset != 5 {
		t.Errorf("non-sticky pushScrollback should preserve scrollOffset, got %d", m.scrollOffset)
	}
}

func TestTUI_MouseWheelClearsSticky(t *testing.T) {
	m := newTestModel()
	m.sticky = true

	m.handleMouse(tea.MouseMsg{Type: tea.MouseWheelUp})
	if m.sticky {
		t.Error("MouseWheelUp should clear sticky")
	}

	m.sticky = true
	m.handleMouse(tea.MouseMsg{Type: tea.MouseWheelDown})
	if m.sticky {
		t.Error("MouseWheelDown should clear sticky")
	}
}

func TestTUI_EndKeyScrollsToBottom(t *testing.T) {
	m := newTestModel()
	m.sticky = false
	m.scrollOffset = 20

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	if m.scrollOffset != 0 {
		t.Errorf("End key should reset scrollOffset to 0, got %d", m.scrollOffset)
	}
	if !m.sticky {
		t.Error("End key should set sticky=true")
	}
}

func TestTUI_ScrollbackPreservesOffsetWhenIdle(t *testing.T) {
	m := newTestModel()

	for i := 0; i < 30; i++ {
		m.pushScrollback("old line")
	}

	m.scrollOffset = 8
	m.sticky = false

	// sticky=false — pushScrollback should preserve scrollOffset.
	m.pushScrollback("some notice line")
	if m.scrollOffset != 8 {
		t.Errorf("pushScrollback while not sticky should preserve scrollOffset, got %d", m.scrollOffset)
	}
}

func TestTUI_MouseWheelUp(t *testing.T) {
	m := newTestModel()
	m.scrollOffset = 0

	m.handleMouse(tea.MouseMsg{Type: tea.MouseWheelUp})
	if m.scrollOffset != wheelScrollLines {
		t.Errorf("MouseWheelUp should increase offset by %d, got %d", wheelScrollLines, m.scrollOffset)
	}
}

func TestTUI_MouseWheelDown(t *testing.T) {
	m := newTestModel()
	m.scrollOffset = 12

	m.handleMouse(tea.MouseMsg{Type: tea.MouseWheelDown})
	if m.scrollOffset != 12-wheelScrollLines {
		t.Errorf("MouseWheelDown should decrease offset by %d, got %d", wheelScrollLines, m.scrollOffset)
	}

	// Scroll past zero — should clamp to 0, not go negative.
	m.scrollOffset = 2
	m.handleMouse(tea.MouseMsg{Type: tea.MouseWheelDown})
	if m.scrollOffset != 0 {
		t.Errorf("MouseWheelDown should clamp to 0, got %d", m.scrollOffset)
	}

	// Already at 0 — should stay at 0.
	m.scrollOffset = 0
	m.handleMouse(tea.MouseMsg{Type: tea.MouseWheelDown})
	if m.scrollOffset != 0 {
		t.Errorf("MouseWheelDown at 0 should stay 0, got %d", m.scrollOffset)
	}
}
