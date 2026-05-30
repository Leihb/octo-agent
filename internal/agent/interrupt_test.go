package agent

import (
	"context"
	"errors"
	"testing"
)

// TestFinishInterrupted_HistoryShapes covers the three history end-states the
// helper must normalize so the next turn keeps user/assistant alternation.
func TestFinishInterrupted_HistoryShapes(t *testing.T) {
	t.Run("unanswered user input is dropped", func(t *testing.T) {
		a := New(&summarizeFake{}, "m")
		a.History.Append(NewUserMessage("hi"))
		_, err := a.finishInterrupted(nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if a.History.Len() != 0 {
			t.Errorf("history len = %d, want 0 (unanswered input dropped)", a.History.Len())
		}
	})

	t.Run("trailing tool_result is capped with an assistant note", func(t *testing.T) {
		a := New(&summarizeFake{}, "m")
		a.History.Append(NewUserMessage("read it"))
		a.History.Append(NewToolUseMessage([]ContentBlock{NewToolUseBlock("c1", "read_file", nil)}))
		a.History.Append(NewToolResultMessage([]ContentBlock{NewToolResultBlock("c1", "ctx canceled", true)}))
		_, err := a.finishInterrupted(nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		msgs := a.History.Snapshot()
		if len(msgs) != 4 {
			t.Fatalf("history len = %d, want 4", len(msgs))
		}
		if last := msgs[3]; last.Role != RoleAssistant || last.Content != interruptNote {
			t.Errorf("last msg = %+v, want assistant interrupt note", last)
		}
	})

	t.Run("history already ending on assistant is untouched", func(t *testing.T) {
		a := New(&summarizeFake{}, "m")
		a.History.Append(NewUserMessage("hi"))
		a.History.Append(NewAssistantMessage("hello"))
		_, _ = a.finishInterrupted(nil)
		if a.History.Len() != 2 {
			t.Errorf("history len = %d, want 2 (unchanged)", a.History.Len())
		}
	})
}

// cancelOnSendSender cancels the supplied context the first time it is asked to
// produce a reply, simulating Ctrl-C arriving during the provider call.
type cancelOnSendSender struct {
	cancel context.CancelFunc
}

func (s *cancelOnSendSender) SendMessages(ctx context.Context, _, _ string, _ []Message, _ int) (Reply, error) {
	s.cancel()
	return Reply{}, ctx.Err()
}

// TestRun_InterruptDuringFirstCall verifies an interrupt on the very first
// model call returns context.Canceled and leaves history empty (the unanswered
// user turn is dropped).
func TestRun_InterruptDuringFirstCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := New(&cancelOnSendSender{cancel: cancel}, "m")

	_, err := a.Turn(ctx, "hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if a.History.Len() != 0 {
		t.Errorf("history len = %d, want 0", a.History.Len())
	}
}

// toolThenCancelSender returns a tool_use on the first call; the executor
// cancels the context during dispatch, so the loop must still produce a
// well-formed history (assistant tool_use + tool_result + interrupt note).
type toolThenCancelSender struct{ calls int }

func (s *toolThenCancelSender) SendMessages(_ context.Context, _, _ string, _ []Message, _ int) (Reply, error) {
	return Reply{}, nil // unused (tool path goes through SendMessagesWithTools)
}

func (s *toolThenCancelSender) SendMessagesWithTools(_ context.Context, _, _ string, _ []Message, _ int, _ []ToolDefinition) (Reply, error) {
	s.calls++
	return Reply{
		StopReason: "tool_use",
		Blocks:     []ContentBlock{NewToolUseBlock("c1", "do", map[string]any{})},
	}, nil
}

type cancelOnExecExecutor struct{ cancel context.CancelFunc }

func (e cancelOnExecExecutor) Execute(ctx context.Context, _ string, _ map[string]any) (ToolResult, error) {
	e.cancel() // Ctrl-C arrives while the tool runs
	return ToolResult{}, ctx.Err()
}

func TestRun_InterruptDuringToolDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := New(&toolThenCancelSender{}, "m")
	tools := []ToolDefinition{{Name: "do"}}

	_, err := a.Run(ctx, "go", tools, cancelOnExecExecutor{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// History must be well-formed: user, assistant(tool_use), user(tool_result),
	// assistant(interrupt note) — alternation preserved for the next turn.
	msgs := a.History.Snapshot()
	if len(msgs) != 4 {
		t.Fatalf("history len = %d, want 4: %+v", len(msgs), msgs)
	}
	wantRoles := []Role{RoleUser, RoleAssistant, RoleUser, RoleAssistant}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("msg[%d].Role = %q, want %q", i, msgs[i].Role, want)
		}
	}
	if !hasToolResult(msgs[2]) {
		t.Errorf("msg[2] should carry the synthesized tool_result")
	}
	if msgs[3].Content != interruptNote {
		t.Errorf("msg[3] = %q, want interrupt note", msgs[3].Content)
	}
}
