package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// summarizeFake records the summarization side-call and returns a canned
// summary. It implements only Sender (SendMessages), which is all summarize
// needs.
type summarizeFake struct {
	summary string
	calls   int
	gotMsgs []Message
}

func (f *summarizeFake) SendMessages(_ context.Context, _, _ string, msgs []Message, _ int) (Reply, error) {
	f.calls++
	f.gotMsgs = msgs
	return Reply{Content: f.summary, InputTokens: 5, OutputTokens: 3}, nil
}

func TestSafeSplitIndex(t *testing.T) {
	u := NewUserMessage
	a := NewAssistantMessage
	toolResult := Message{Role: RoleUser, Blocks: []ContentBlock{NewToolResultBlock("c1", "out", false)}}

	t.Run("not enough turns", func(t *testing.T) {
		msgs := []Message{u("1"), a("r1"), u("2"), a("r2")}
		if got := safeSplitIndex(msgs, 4); got != 0 {
			t.Errorf("split = %d, want 0 (only 2 user turns, keep 4)", got)
		}
	})

	t.Run("splits keeping last N user turns", func(t *testing.T) {
		// 6 user turns at indices 0,2,4,6,8,10. Keep last 4 → split at index 4.
		var msgs []Message
		for i := 0; i < 6; i++ {
			msgs = append(msgs, u(fmt.Sprintf("u%d", i)), a(fmt.Sprintf("a%d", i)))
		}
		if got := safeSplitIndex(msgs, 4); got != 4 {
			t.Errorf("split = %d, want 4", got)
		}
	})

	t.Run("tool_result messages are not counted as user turns", func(t *testing.T) {
		// Real user turns at 0 and 4; the tool_result at index 2 must NOT
		// count, so with keep=1 the split lands on the second real turn (4),
		// never on the tool_result.
		msgs := []Message{
			u("real-1"),       // 0  user turn
			a("asst tooluse"), // 1
			toolResult,        // 2  NOT a user turn
			a("asst final"),   // 3
			u("real-2"),       // 4  user turn
			a("asst done"),    // 5
		}
		if got := safeSplitIndex(msgs, 1); got != 4 {
			t.Errorf("split = %d, want 4 (the second real user turn)", got)
		}
	})
}

func TestMaybeCompact_DisabledOrBelowThreshold(t *testing.T) {
	f := &summarizeFake{summary: "S"}
	a := New(f, "m")
	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(fmt.Sprintf("u%d", i)))
		a.History.Append(NewAssistantMessage(fmt.Sprintf("a%d", i)))
	}

	// Disabled (threshold 0).
	a.lastInputTokens = 999999
	if err := a.maybeCompact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || a.History.Len() != 12 {
		t.Errorf("disabled: should not compact; calls=%d len=%d", f.calls, a.History.Len())
	}

	// Enabled but under threshold.
	a.CompactThreshold = 1000
	a.lastInputTokens = 500
	if err := a.maybeCompact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || a.History.Len() != 12 {
		t.Errorf("under threshold: should not compact; calls=%d len=%d", f.calls, a.History.Len())
	}
}

func TestMaybeCompact_RewritesHistory(t *testing.T) {
	f := &summarizeFake{summary: "GOAL: build X. Touched main.go."}
	a := New(f, "m")
	a.CompactThreshold = 100
	a.lastInputTokens = 200 // over threshold

	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(fmt.Sprintf("u%d", i)))
		a.History.Append(NewAssistantMessage(fmt.Sprintf("a%d", i)))
	}

	if err := a.maybeCompact(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := a.History.Snapshot()
	// First message is the summary user-message.
	if snap[0].Role != RoleUser || !strings.Contains(snap[0].Content, "GOAL: build X") {
		t.Errorf("first message should carry the summary, got %+v", snap[0])
	}
	// 6 user turns, keep last 4 → split at index 4; kept = msgs[4:] = 8
	// messages, + 1 summary = 9.
	if len(snap) != 9 {
		t.Errorf("history len = %d, want 9", len(snap))
	}
	// The summarized side-call saw the dropped prefix (4 msgs) + 1 instruction.
	if f.calls != 1 {
		t.Fatalf("summarize calls = %d, want 1", f.calls)
	}
	if len(f.gotMsgs) != 5 {
		t.Errorf("summarize saw %d messages, want 5 (4 dropped + instruction)", len(f.gotMsgs))
	}
	// The most-recent turn survived verbatim.
	if snap[len(snap)-1].Content != "a5" {
		t.Errorf("last message = %q, want 'a5'", snap[len(snap)-1].Content)
	}
	// Trigger reset so we don't immediately re-compact.
	if a.lastInputTokens != 0 {
		t.Errorf("lastInputTokens should reset to 0, got %d", a.lastInputTokens)
	}
}

// compactingFake serves the summarization side-call (SendMessages) and the
// agentic loop (SendMessagesWithTools) from separate scripts, so a Run can be
// observed triggering compaction before its turn.
type compactingFake struct {
	summary        string
	loopReplies    []Reply
	loopCalls      int
	summarizeCalls int
}

func (f *compactingFake) SendMessages(_ context.Context, _, _ string, _ []Message, _ int) (Reply, error) {
	f.summarizeCalls++
	return Reply{Content: f.summary}, nil
}

func (f *compactingFake) SendMessagesWithTools(_ context.Context, _, _ string, _ []Message, _ int, _ []ToolDefinition) (Reply, error) {
	r := f.loopReplies[f.loopCalls]
	f.loopCalls++
	return r, nil
}

func TestRun_CompactsBetweenTurns(t *testing.T) {
	f := &compactingFake{
		summary:     "earlier work summary",
		loopReplies: []Reply{{Content: "done", StopReason: "end_turn"}},
	}
	a := New(f, "m")
	a.CompactThreshold = 100
	a.lastInputTokens = 500 // prior turn was big → should compact before this Run

	// Seed a long prior history (6 user turns).
	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(fmt.Sprintf("u%d", i)))
		a.History.Append(NewAssistantMessage(fmt.Sprintf("a%d", i)))
	}

	defs := []ToolDefinition{{Name: "terminal"}}
	exec := &fakeExecutor{}
	if _, err := a.Run(context.Background(), "next request", defs, exec); err != nil {
		t.Fatal(err)
	}

	if f.summarizeCalls != 1 {
		t.Errorf("expected one summarization call before the turn, got %d", f.summarizeCalls)
	}
	// History now: summary + 8 kept + new user("next request") + assistant("done") = 11.
	snap := a.History.Snapshot()
	if !strings.Contains(snap[0].Content, "earlier work summary") {
		t.Errorf("compaction summary should lead the history, got %+v", snap[0])
	}
}
