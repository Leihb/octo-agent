package agent

import (
	"context"
	"testing"
)

func TestSteer_AccumulatesJoinsAndClears(t *testing.T) {
	a := New(&fakeToolSender{}, "m")

	if a.HasPendingSteer() {
		t.Fatal("fresh agent should have no pending steer")
	}
	if got := a.DrainSteer(); got != "" {
		t.Errorf("DrainSteer on empty = %q, want empty", got)
	}

	// Whitespace-only steers are ignored.
	a.Steer("   ")
	a.Steer("\n\t")
	if a.HasPendingSteer() {
		t.Fatal("whitespace-only steers should be ignored")
	}

	a.Steer("also handle the error case")
	a.Steer("and add a test")
	if !a.HasPendingSteer() {
		t.Fatal("expected pending steer after two messages")
	}

	got := a.DrainSteer()
	want := "also handle the error case\n\nand add a test"
	if got != want {
		t.Errorf("DrainSteer = %q, want %q", got, want)
	}
	// Drain must clear.
	if a.HasPendingSteer() || a.DrainSteer() != "" {
		t.Error("DrainSteer did not clear the buffer")
	}
}

// TestRunLoop_SteerAppendedAsStandaloneUserMessage verifies that steer text
// is appended as a standalone user message after the tool_result, not folded
// into it. This gives steer text (including system-reminder notifications)
// its own message boundary so the model sees it as a first-class inbox item.
func TestRunLoop_SteerAppendedAsStandaloneUserMessage(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("call-1", "terminal", map[string]any{"command": "echo hi"}),
				},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	a := New(send, "m")
	a.Steer("also handle the error case")

	defs := []ToolDefinition{{Name: "terminal"}}
	exec := &fakeExecutor{results: map[string]string{"terminal": "hi"}}
	if _, err := a.RunStream(context.Background(), "run echo", defs, exec, nil); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	msgs := a.History.Snapshot()
	// Expect: user("run echo"), assistant(tool_use), user(tool_result),
	// user(steer), assistant("done").
	if len(msgs) != 5 {
		t.Fatalf("history len = %d, want 5: %+v", len(msgs), msgs)
	}

	// msg[2] is the pure tool_result (no steer folded in).
	tr := msgs[2]
	if tr.Role != RoleUser {
		t.Fatalf("msg[2].Role = %q, want user (tool_result)", tr.Role)
	}
	if len(tr.Blocks) != 1 || tr.Blocks[0].Type != "tool_result" {
		t.Fatalf("tool_result message should have exactly 1 tool_result block, got %+v", tr.Blocks)
	}

	// msg[3] is the standalone steer user message.
	steerMsg := msgs[3]
	if steerMsg.Role != RoleUser {
		t.Fatalf("msg[3].Role = %q, want user (steer)", steerMsg.Role)
	}
	if steerMsg.Content != "also handle the error case" {
		t.Errorf("steer msg content = %q, want 'also handle the error case'", steerMsg.Content)
	}

	// Buffer drained.
	if a.HasPendingSteer() {
		t.Error("steer buffer should be empty after injection")
	}
}

// TestRunLoop_SteerArrivesDuringExecution models the realistic timing: the
// steer is queued from inside tool execution (a stand-in for the UI goroutine
// calling Steer mid-turn), then injected at that batch's boundary.
func TestRunLoop_SteerArrivesDuringExecution(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("call-1", "terminal", map[string]any{"command": "sleep"}),
				},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	a := New(send, "m")
	exec := &steeringExecutor{a: a, steer: "switch to the other approach"}

	defs := []ToolDefinition{{Name: "terminal"}}
	if _, err := a.RunStream(context.Background(), "go", defs, exec, nil); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	msgs := a.History.Snapshot()
	// msg[2] = tool_result, msg[3] = standalone steer user message
	tr := msgs[2]
	if len(tr.Blocks) != 1 || tr.Blocks[0].Type != "tool_result" {
		t.Fatalf("tool_result should be pure (no steer folded in): %+v", tr.Blocks)
	}
	steerMsg := msgs[3]
	if steerMsg.Role != RoleUser || steerMsg.Content != "switch to the other approach" {
		t.Fatalf("steer should be standalone user message: %+v", steerMsg)
	}
}

// TestRunLoop_NoBoundary_SteerStaysPending covers the degrade-to-queue path
// (design §8): a steer queued during a no-tool turn never finds a tool-batch
// boundary, so it must remain pending for the caller to run as the next turn.
func TestRunLoop_NoBoundary_SteerStaysPending(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{{Content: "plain reply", StopReason: "end_turn"}},
	}
	a := New(send, "m")
	a.Steer("do this next")

	defs := []ToolDefinition{{Name: "terminal"}}
	exec := &fakeExecutor{}
	if _, err := a.RunStream(context.Background(), "hello", defs, exec, nil); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if !a.HasPendingSteer() {
		t.Fatal("steer should still be pending after a no-tool turn (degrade to queue)")
	}
	if got := a.DrainSteer(); got != "do this next" {
		t.Errorf("pending steer = %q, want 'do this next'", got)
	}
}

// steeringExecutor calls a.Steer the first time it runs, simulating a user
// typing a steer message while the tool is executing.
type steeringExecutor struct {
	a     *Agent
	steer string
	fired bool
}

func (e *steeringExecutor) Execute(_ context.Context, _ string, _ map[string]any) (ToolResult, error) {
	if !e.fired {
		e.a.Steer(e.steer)
		e.fired = true
	}
	return ToolResult{Text: "ok"}, nil
}
