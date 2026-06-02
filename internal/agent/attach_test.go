package agent

import (
	"context"
	"testing"
)

// TestAttachUserBlocks_MergedWithText verifies a queued image block rides the
// next user turn alongside the text, as a single multi-part user message.
func TestAttachUserBlocks_MergedWithText(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "ok", StopReason: "end_turn"}}
	a := New(send, "m")

	a.AttachUserBlocks([]ContentBlock{NewImageBlock("image/png", []byte{0x89, 'P', 'N', 'G'})})
	if _, err := a.Turn(context.Background(), "what is this?"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if len(send.gotMessages) != 1 {
		t.Fatalf("want 1 message, got %d", len(send.gotMessages))
	}
	m := send.gotMessages[0]
	if m.Role != RoleUser {
		t.Fatalf("role = %q", m.Role)
	}
	if len(m.Blocks) != 2 {
		t.Fatalf("want 2 blocks (text+image), got %d: %+v", len(m.Blocks), m.Blocks)
	}
	if m.Blocks[0].Type != "text" || m.Blocks[0].Text != "what is this?" {
		t.Errorf("block[0] = %+v, want text 'what is this?'", m.Blocks[0])
	}
	if m.Blocks[1].Type != "image" || m.Blocks[1].Image == nil {
		t.Errorf("block[1] = %+v, want image with data", m.Blocks[1])
	}

	// Consumed exactly once: a second turn carries no leftover attachment.
	if _, err := a.Turn(context.Background(), "and now?"); err != nil {
		t.Fatalf("Turn 2: %v", err)
	}
	last := send.gotMessages[len(send.gotMessages)-1]
	if len(last.Blocks) != 0 {
		t.Errorf("second turn should not reuse the attachment, got blocks %+v", last.Blocks)
	}
}

// TestAttachUserBlocks_ImageOnly verifies an attachment with empty text is
// allowed (the non-empty-input guard is relaxed when blocks are queued) and
// produces an image-only user message.
func TestAttachUserBlocks_ImageOnly(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "ok", StopReason: "end_turn"}}
	a := New(send, "m")

	a.AttachUserBlocks([]ContentBlock{NewImageBlock("image/png", []byte{1, 2, 3})})
	if _, err := a.Turn(context.Background(), ""); err != nil {
		t.Fatalf("image-only Turn should be allowed: %v", err)
	}
	m := send.gotMessages[0]
	if len(m.Blocks) != 1 || m.Blocks[0].Type != "image" {
		t.Fatalf("want a single image block, got %+v", m.Blocks)
	}
}

// TestEmptyInput_StillRejectedWithoutBlocks keeps the original guard: an empty
// turn with no attachment is an error.
func TestEmptyInput_StillRejectedWithoutBlocks(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "ok"}}
	a := New(send, "m")
	if _, err := a.Turn(context.Background(), ""); err == nil {
		t.Fatal("empty input with no attachment should still error")
	}
}
