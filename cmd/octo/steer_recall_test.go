package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// steerModel returns a model mid-turn with the given steer messages submitted
// (queued in the inbox + shown as pending), the input box empty.
func steerModel(t *testing.T, steers ...string) *tuiModel {
	t.Helper()
	m := newTestModel()
	m.turnRunning = true
	for _, s := range steers {
		setInput(m, s)
		m.submit()
	}
	return m
}

func TestSteerRecall_UpRetractsLastPending(t *testing.T) {
	m := steerModel(t, "first steer", "second steer")

	// ↑ retracts the most recent pending steer into the input box.
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.ta.Value() != "second steer" {
		t.Fatalf("↑ recalled %q, want 'second steer'", m.ta.Value())
	}
	// It's removed from the pending display AND the inbox (won't be sent).
	if len(m.pendingSteer) != 1 || m.pendingSteer[0] != "first steer" {
		t.Errorf("pendingSteer = %v, want [first steer]", m.pendingSteer)
	}
	if drained := m.a.Inbox.Drain(); len(drained) != 1 || drained[0] != "first steer" {
		t.Errorf("inbox after retract = %v, want [first steer]", drained)
	}
}

func TestSteerRecall_UpDoesNotRetractWhenInputPresent(t *testing.T) {
	m := steerModel(t, "pending one")
	setInput(m, "typing my own")

	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	// With text present, ↑ keeps its ordinary history-browse behaviour and must
	// NOT retract the pending steer — it stays queued (display + inbox).
	if len(m.pendingSteer) != 1 || m.pendingSteer[0] != "pending one" {
		t.Errorf("pending steer should stay queued, got %v", m.pendingSteer)
	}
	if !m.a.Inbox.HasPending() {
		t.Error("steer should remain in the inbox when not retracted")
	}
}

func TestSteerRecall_AlreadyDrainedFallsThroughToHistory(t *testing.T) {
	m := steerModel(t, "drained steer")
	// Simulate the loop draining the inbox before the user hits ↑.
	_ = m.a.Inbox.Drain()

	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	// Inbox.Remove fails (already committed), so ↑ falls through to ordinary
	// history recall — which still has the steer (added at submit time).
	if m.ta.Value() != "drained steer" {
		t.Errorf("↑ value = %q, want history-recalled 'drained steer'", m.ta.Value())
	}
	// pendingSteer is untouched by the fall-through (the EventSteerInjected that
	// would clear it is processed separately).
	if len(m.pendingSteer) != 1 {
		t.Errorf("pendingSteer = %v, want it left for EventSteerInjected to clear", m.pendingSteer)
	}
}

func TestSteerRecall_NoPendingUsesHistory(t *testing.T) {
	m := newTestModel()
	// An ordinary idle submit populates history but no pending steer.
	setInput(m, "earlier prompt")
	m.submit() // idle (turnRunning false) → starts a turn, but history has it
	m.turnRunning = false

	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.ta.Value() != "earlier prompt" {
		t.Errorf("↑ with no pending steer = %q, want history recall 'earlier prompt'", m.ta.Value())
	}
}
