package main

import (
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

// fakeAttachment builds a pendingAttachment with a dummy image block for tests.
func fakeAttachment() pendingAttachment {
	return pendingAttachment{
		block: agent.NewImageBlock("image/png", []byte{0x89, 'P', 'N', 'G'}),
		label: "image (PNG, 4 B)",
	}
}

// TestTUI_SubmitImageOnlyStartsTurn verifies that Enter with no text but a
// pending attachment still starts a turn and consumes the attachment (an
// image-only message is valid).
func TestTUI_SubmitImageOnlyStartsTurn(t *testing.T) {
	m := newTestModel()
	m.pendingAttachments = []pendingAttachment{fakeAttachment()}
	setInput(m, "")

	_, _ = m.submit()

	if !m.turnRunning {
		t.Fatal("Enter with an attachment (empty text) should start a turn")
	}
	if len(m.pendingAttachments) != 0 {
		t.Errorf("attachment should be consumed on submit, still have %d", len(m.pendingAttachments))
	}
}

// TestTUI_EscDiscardsAttachments verifies idle Esc clears pending attachments
// through the real key handler.
func TestTUI_EscDiscardsAttachments(t *testing.T) {
	m := newTestModel()
	m.pendingAttachments = []pendingAttachment{fakeAttachment()}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if len(m.pendingAttachments) != 0 {
		t.Errorf("idle Esc should discard attachments, still have %d", len(m.pendingAttachments))
	}
}

// TestTUI_MidTurnKeepsAttachments verifies that submitting while a turn runs
// keeps the attachment pending (it can't ride a text-only steer) and still
// enqueues the steer text.
func TestTUI_MidTurnKeepsAttachments(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	m.pendingAttachments = []pendingAttachment{fakeAttachment()}
	setInput(m, "also look here")

	_, _ = m.submit()

	if len(m.pendingAttachments) != 1 {
		t.Errorf("attachment should stay pending mid-turn, got %d", len(m.pendingAttachments))
	}
	if !m.a.Inbox.HasPending() {
		t.Error("steer text should still be enqueued mid-turn")
	}
}
