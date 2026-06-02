package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSuggestion_SetAcceptClear(t *testing.T) {
	m := newTestModel()

	m.setSuggestion("run the tests")
	if m.suggestion != "run the tests" {
		t.Fatalf("suggestion = %q", m.suggestion)
	}
	if m.ta.Placeholder != "run the tests" {
		t.Errorf("placeholder = %q, want the suggestion as ghost text", m.ta.Placeholder)
	}

	m.acceptSuggestion()
	if m.ta.Value() != "run the tests" {
		t.Errorf("after accept, input = %q, want the suggestion filled in", m.ta.Value())
	}
	if m.suggestion != "" {
		t.Error("suggestion should be cleared after accept")
	}
	if m.ta.Placeholder != inputPlaceholder {
		t.Errorf("placeholder = %q, want it restored to %q", m.ta.Placeholder, inputPlaceholder)
	}
}

func TestSuggestion_ClearRestoresPlaceholder(t *testing.T) {
	m := newTestModel()
	m.setSuggestion("commit")
	m.clearSuggestion()
	if m.suggestion != "" || m.ta.Placeholder != inputPlaceholder {
		t.Errorf("after clear: suggestion=%q placeholder=%q", m.suggestion, m.ta.Placeholder)
	}
}

func TestSuggestion_TabAcceptsWhenEmpty(t *testing.T) {
	m := newTestModel()
	m.setSuggestion("open the PR")

	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyTab}); m.ta.Value() != "open the PR" {
		t.Errorf("Tab on empty input should accept; input = %q", m.ta.Value())
	}
	if m.suggestion != "" {
		t.Error("suggestion not cleared after Tab accept")
	}
}

func TestSuggestion_RightArrowAcceptsWhenEmpty(t *testing.T) {
	m := newTestModel()
	m.setSuggestion("deploy to staging")
	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRight}); m.ta.Value() != "deploy to staging" {
		t.Errorf("→ on empty input should accept; input = %q", m.ta.Value())
	}
}

func TestSuggestion_TabIgnoredWhenInputPresent(t *testing.T) {
	m := newTestModel()
	m.setSuggestion("a suggestion")
	setInput(m, "my own text")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	// With text present, Tab must NOT replace it with the suggestion.
	if m.ta.Value() != "my own text" {
		t.Errorf("Tab with input present overwrote it: %q", m.ta.Value())
	}
	if m.suggestion != "a suggestion" {
		t.Error("suggestion should remain pending when not accepted")
	}
}

func TestSuggestion_StartTurnClears(t *testing.T) {
	m := newTestModel()
	m.setSuggestion("stale")
	_ = m.startTurnEcho("go", "go")
	if m.suggestion != "" {
		t.Error("starting a turn should clear the pending suggestion")
	}
}

func TestSuggestion_MsgIgnoredWhileTurnRunning(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	_, _ = m.Update(suggestionMsg{text: "late suggestion"})
	if m.suggestion != "" {
		t.Error("a suggestion arriving during a turn should be dropped")
	}
}
