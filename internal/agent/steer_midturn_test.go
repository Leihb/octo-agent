package agent

import (
	"context"
	"testing"
)

// A mid-turn steer that arrives while the model is producing its FINAL answer
// (no further tool calls) must be answered within the same turn — drained into
// history and sent — rather than stranded for the front-end to re-queue (which
// historically dropped any image blocks). Covers the three steer shapes the
// feature must support: image-only, text-only, and multiple images.
func TestAgent_MidTurnSteer_AnsweredInTurn(t *testing.T) {
	img1 := NewImageBlock("image/png", []byte("one"))
	img2 := NewImageBlock("image/jpeg", []byte("two"))

	cases := []struct {
		name        string
		steerText   string
		steerBlocks []ContentBlock
		// wantImgs is how many image blocks the injected user message must carry.
		wantImgs int
		// wantText is the text block the injected user message must carry ("" = none).
		wantText string
	}{
		{"image only", "", []ContentBlock{img1}, 1, ""},
		{"text only", "and the logs?", nil, 0, "and the logs?"},
		{"multiple images", "compare these", []ContentBlock{img1, img2}, 2, "compare these"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			send := &fakeToolSender{
				replies: []Reply{
					{Content: "first answer", StopReason: "end_turn"},
					{Content: "answer to the steer", StopReason: "end_turn"},
				},
			}
			a := New(send, "m")
			// Enqueue the steer the moment the model returns its first (final)
			// answer — i.e. after call 0 has produced its reply.
			send.onCall = func(idx int) {
				if idx == 0 {
					a.Inbox.EnqueueWithBlocks(c.steerText, c.steerBlocks)
				}
			}

			// A tool def routes Run through runLoop (the steer-drain path); the
			// replies are end_turn, so no tool is actually invoked.
			defs := []ToolDefinition{{Name: "bash", Description: "run shell"}}
			reply, err := a.Run(context.Background(), "first question", defs, &fakeExecutor{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			// The turn must not end on the first answer; it loops to answer the steer.
			if reply.Content != "answer to the steer" {
				t.Fatalf("final Content = %q, want the steer to have been answered in-turn", reply.Content)
			}
			// Both sends happened (the steer triggered a second round).
			if send.calls != 2 {
				t.Fatalf("sender called %d times, want 2", send.calls)
			}

			// History: user(q), assistant(first), user(steer), assistant(final).
			snap := a.History.Snapshot()
			if len(snap) != 4 {
				t.Fatalf("History len = %d, want 4: %+v", len(snap), snap)
			}
			steerMsg := snap[2]
			if steerMsg.Role != RoleUser {
				t.Fatalf("snap[2] role = %q, want user", steerMsg.Role)
			}
			gotImgs, gotText := 0, ""
			for _, b := range steerMsg.Blocks {
				switch b.Type {
				case "image":
					gotImgs++
				case "text":
					gotText = b.Text
				}
			}
			// A text-only steer is appended as a plain string message (no Blocks).
			if len(steerMsg.Blocks) == 0 {
				gotText = steerMsg.Content
			}
			if gotImgs != c.wantImgs {
				t.Errorf("injected image blocks = %d, want %d (blocks: %+v)", gotImgs, c.wantImgs, steerMsg.Blocks)
			}
			if gotText != c.wantText {
				t.Errorf("injected text = %q, want %q", gotText, c.wantText)
			}

			// The inbox is empty after the loop consumed the steer.
			if a.Inbox.HasPending() {
				t.Errorf("inbox still has pending items after the turn")
			}
		})
	}
}

// With no mid-turn steer, the turn ends on the first answer exactly as before
// (the HasPending check must not perturb the common path).
func TestAgent_NoSteer_EndsNormally(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{{Content: "done", StopReason: "end_turn"}},
	}
	a := New(send, "m")
	reply, err := a.Run(context.Background(), "q", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply.Content != "done" {
		t.Errorf("Content = %q", reply.Content)
	}
	if send.calls != 1 {
		t.Errorf("sender called %d times, want 1", send.calls)
	}
}
