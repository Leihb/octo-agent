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

// TestRunLoop_SteerMergedIntoToolResult is the core Step 2 invariant: a steer
// queued while a tool batch runs is folded into the SAME user/tool_result
// message as an extra text block — never appended as a second user message in
// a row (which would break the user/assistant alternation the providers
// require).
func TestRunLoop_SteerMergedIntoToolResult(t *testing.T) {
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
	// Steer arrives before the boundary (modelled by pre-seeding; the loop
	// drains it right after the tool batch completes).
	a.Steer("also handle the error case")

	defs := []ToolDefinition{{Name: "terminal"}}
	exec := &fakeExecutor{results: map[string]string{"terminal": "hi"}}
	if _, err := a.RunStream(context.Background(), "run echo", defs, exec, nil); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	msgs := a.History.Snapshot()
	// Expect: user("run echo"), assistant(tool_use), user(tool_result+steer),
	// assistant("done").
	if len(msgs) != 4 {
		t.Fatalf("history len = %d, want 4: %+v", len(msgs), msgs)
	}

	// No two user-role messages in a row.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == RoleUser && msgs[i-1].Role == RoleUser {
			t.Fatalf("consecutive user messages at %d/%d — alternation broken", i-1, i)
		}
	}

	tr := msgs[2]
	if tr.Role != RoleUser {
		t.Fatalf("msg[2].Role = %q, want user (tool_result)", tr.Role)
	}
	if len(tr.Blocks) != 2 {
		t.Fatalf("tool_result message blocks = %d, want 2 (tool_result + steer text): %+v", len(tr.Blocks), tr.Blocks)
	}
	if tr.Blocks[0].Type != "tool_result" {
		t.Errorf("blocks[0].Type = %q, want tool_result", tr.Blocks[0].Type)
	}
	if tr.Blocks[1].Type != "text" || tr.Blocks[1].Text != "also handle the error case" {
		t.Errorf("blocks[1] = %+v, want text steer", tr.Blocks[1])
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
	tr := msgs[2]
	if len(tr.Blocks) != 2 || tr.Blocks[1].Type != "text" || tr.Blocks[1].Text != "switch to the other approach" {
		t.Fatalf("steer not merged into tool_result: %+v", tr.Blocks)
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
