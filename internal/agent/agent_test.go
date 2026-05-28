package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeSender implements Sender for tests, recording its inputs and returning
// canned replies.
type fakeSender struct {
	gotModel    string
	gotSystem   string
	gotMessages []Message
	gotMaxToks  int

	reply Reply
	err   error
}

func (f *fakeSender) SendMessages(_ context.Context, model, system string, messages []Message, maxTokens int) (Reply, error) {
	f.gotModel = model
	f.gotSystem = system
	f.gotMessages = append([]Message(nil), messages...) // defensive copy
	f.gotMaxToks = maxTokens
	if f.err != nil {
		return Reply{}, f.err
	}
	return f.reply, nil
}

func TestAgent_Turn_HappyPath(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "hi from agent", Model: "m", StopReason: "end_turn"}}
	a := New(send, "claude-test")
	a.System = "you are octo"

	reply, err := a.Turn(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if reply.Content != "hi from agent" {
		t.Errorf("reply.Content = %q", reply.Content)
	}

	// Sender saw model + system + the single user message
	if send.gotModel != "claude-test" {
		t.Errorf("Sender saw model %q", send.gotModel)
	}
	if send.gotSystem != "you are octo" {
		t.Errorf("Sender saw system %q", send.gotSystem)
	}
	if len(send.gotMessages) != 1 || send.gotMessages[0].Role != RoleUser {
		t.Errorf("Sender saw messages %+v", send.gotMessages)
	}

	// History now has [user, assistant]
	snap := a.History.Snapshot()
	if len(snap) != 2 || snap[0].Role != RoleUser || snap[1].Role != RoleAssistant {
		t.Errorf("History after Turn = %+v", snap)
	}
}

func TestAgent_Turn_MultiTurnSendsFullHistory(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "ok"}}
	a := New(send, "m")

	for _, msg := range []string{"first", "second", "third"} {
		if _, err := a.Turn(context.Background(), msg); err != nil {
			t.Fatalf("Turn(%q): %v", msg, err)
		}
	}

	// On the third call the Sender must have seen [user, asst, user, asst, user].
	if got := len(send.gotMessages); got != 5 {
		t.Fatalf("len(msgs) on 3rd turn = %d, want 5", got)
	}
	wantRoles := []Role{RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser}
	for i, want := range wantRoles {
		if send.gotMessages[i].Role != want {
			t.Errorf("messages[%d].Role = %q, want %q", i, send.gotMessages[i].Role, want)
		}
	}
}

func TestAgent_Turn_SenderError_RestoresHistory(t *testing.T) {
	send := &fakeSender{err: errors.New("upstream 500")}
	a := New(send, "m")

	if _, err := a.Turn(context.Background(), "hello"); err == nil {
		t.Fatal("Turn: expected error, got nil")
	}

	// User message must be rolled back so the next attempt isn't a dup.
	if n := a.History.Len(); n != 0 {
		t.Errorf("History.Len after failed Turn = %d, want 0", n)
	}
}

func TestAgent_Turn_Validation(t *testing.T) {
	a := New(&fakeSender{}, "")
	if _, err := a.Turn(context.Background(), "hi"); err == nil {
		t.Error("empty model should error")
	}

	a = New(nil, "m")
	if _, err := a.Turn(context.Background(), "hi"); err == nil {
		t.Error("nil sender should error")
	}

	a = New(&fakeSender{}, "m")
	if _, err := a.Turn(context.Background(), ""); err == nil {
		t.Error("empty input should error")
	}
}

// fakeStreamSender implements StreamingSender, emitting a canned slice of
// deltas before returning the aggregated reply.
type fakeStreamSender struct {
	fakeSender
	chunks       []string
	gotCallback  bool
	emittedReply Reply
}

func (f *fakeStreamSender) StreamMessages(
	ctx context.Context,
	model, system string,
	messages []Message,
	maxTokens int,
	onChunk func(string),
) (Reply, error) {
	// Record inputs through the embedded fakeSender so the existing
	// assertions on gotModel/gotMessages still apply.
	f.fakeSender.gotModel = model
	f.fakeSender.gotSystem = system
	f.fakeSender.gotMessages = append([]Message(nil), messages...)
	f.fakeSender.gotMaxToks = maxTokens

	if f.fakeSender.err != nil {
		return Reply{}, f.fakeSender.err
	}
	for _, c := range f.chunks {
		if onChunk != nil {
			f.gotCallback = true
			onChunk(c)
		}
	}
	return f.emittedReply, nil
}

func TestAgent_TurnStream_HappyPath(t *testing.T) {
	send := &fakeStreamSender{
		chunks:       []string{"hi ", "there"},
		emittedReply: Reply{Content: "hi there", Model: "m", StopReason: "end_turn"},
	}
	a := New(send, "m")
	a.System = "you are octo"

	var got []string
	reply, err := a.TurnStream(context.Background(), "hello", func(d string) {
		got = append(got, d)
	})
	if err != nil {
		t.Fatalf("TurnStream: %v", err)
	}
	if reply.Content != "hi there" {
		t.Errorf("reply.Content = %q", reply.Content)
	}
	if got[0] != "hi " || got[1] != "there" {
		t.Errorf("chunks = %v", got)
	}
	if !send.gotCallback {
		t.Errorf("StreamMessages received nil callback")
	}

	// History: user + assistant, same as the buffered path.
	snap := a.History.Snapshot()
	if len(snap) != 2 || snap[0].Role != RoleUser || snap[1].Role != RoleAssistant {
		t.Errorf("History = %+v", snap)
	}
}

func TestAgent_TurnStream_NilCallback(t *testing.T) {
	send := &fakeStreamSender{
		chunks:       []string{"a", "b"},
		emittedReply: Reply{Content: "ab"},
	}
	a := New(send, "m")

	reply, err := a.TurnStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("TurnStream: %v", err)
	}
	if reply.Content != "ab" {
		t.Errorf("reply.Content = %q", reply.Content)
	}
}

func TestAgent_TurnStream_BufferedFallback(t *testing.T) {
	// A plain Sender (no StreamingSender) should still work — TurnStream
	// must fall back to SendMessages and synthesise a single onChunk call.
	send := &fakeSender{reply: Reply{Content: "buffered reply"}}
	a := New(send, "m")

	var got []string
	reply, err := a.TurnStream(context.Background(), "hi", func(d string) {
		got = append(got, d)
	})
	if err != nil {
		t.Fatalf("TurnStream: %v", err)
	}
	if reply.Content != "buffered reply" {
		t.Errorf("reply.Content = %q", reply.Content)
	}
	if len(got) != 1 || got[0] != "buffered reply" {
		t.Errorf("fallback should emit one chunk with the full content, got %v", got)
	}
}

func TestAgent_TurnStream_Error_RestoresHistory(t *testing.T) {
	send := &fakeStreamSender{fakeSender: fakeSender{err: errors.New("boom")}}
	a := New(send, "m")

	if _, err := a.TurnStream(context.Background(), "hi", nil); err == nil {
		t.Fatal("expected error")
	}
	if n := a.History.Len(); n != 0 {
		t.Errorf("History.Len = %d after failed TurnStream, want 0", n)
	}
}

// ─── Run() agentic loop tests ─────────────────────────────────────────────

// fakeToolSender implements both Sender and ToolSender. On each call it pops
// the next reply from the replies slice so callers can script multi-turn
// sequences.
type fakeToolSender struct {
	replies []Reply
	calls   int
}

func (f *fakeToolSender) SendMessages(_ context.Context, _, _ string, _ []Message, _ int) (Reply, error) {
	return f.nextReply()
}

func (f *fakeToolSender) SendMessagesWithTools(_ context.Context, _, _ string, _ []Message, _ int, _ []ToolDefinition) (Reply, error) {
	return f.nextReply()
}

func (f *fakeToolSender) nextReply() (Reply, error) {
	if f.calls >= len(f.replies) {
		return Reply{}, fmt.Errorf("fakeToolSender: no more replies (call %d)", f.calls)
	}
	r := f.replies[f.calls]
	f.calls++
	return r, nil
}

// fakeExecutor implements ToolExecutor, returning canned results keyed by name.
type fakeExecutor struct {
	results map[string]string
	called  []string
}

func (f *fakeExecutor) Execute(_ context.Context, name string, _ map[string]any) (string, error) {
	f.called = append(f.called, name)
	if f.results != nil {
		if v, ok := f.results[name]; ok {
			return v, nil
		}
	}
	return "ok", nil
}

func TestAgent_Run_NoTools_FallsBackToTurn(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{{Content: "plain reply", StopReason: "end_turn"}},
	}
	a := New(send, "m")

	reply, err := a.Run(context.Background(), "hello", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply.Content != "plain reply" {
		t.Errorf("Content = %q", reply.Content)
	}
}

func TestAgent_Run_ToolUse_ThenEndTurn(t *testing.T) {
	// Round 1: model returns tool_use
	// Round 2: model returns end_turn with final answer
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("call-1", "bash", map[string]any{"command": "echo hi"}),
				},
			},
			{
				Content:    "The output was: hi",
				StopReason: "end_turn",
			},
		},
	}
	exec := &fakeExecutor{results: map[string]string{"bash": "hi"}}

	a := New(send, "m")
	defs := []ToolDefinition{{Name: "bash", Description: "run shell"}}

	reply, err := a.Run(context.Background(), "run echo hi", defs, exec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply.Content != "The output was: hi" {
		t.Errorf("Content = %q", reply.Content)
	}
	if reply.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", reply.StopReason)
	}

	// Executor was called once with "bash".
	if len(exec.called) != 1 || exec.called[0] != "bash" {
		t.Errorf("executor called = %v", exec.called)
	}

	// History should be: user, assistant(tool_use), user(tool_result), assistant(final)
	snap := a.History.Snapshot()
	if len(snap) != 4 {
		t.Fatalf("History len = %d, want 4 (%v)", len(snap), snap)
	}
	if snap[0].Role != RoleUser {
		t.Errorf("snap[0] role = %q", snap[0].Role)
	}
	if snap[1].Role != RoleAssistant || len(snap[1].Blocks) == 0 {
		t.Errorf("snap[1] should be assistant with blocks: %+v", snap[1])
	}
	if snap[2].Role != RoleUser || len(snap[2].Blocks) == 0 {
		t.Errorf("snap[2] should be user with tool results: %+v", snap[2])
	}
	if snap[3].Role != RoleAssistant || snap[3].Content != "The output was: hi" {
		t.Errorf("snap[3] = %+v", snap[3])
	}
}

func TestAgent_Run_MultipleToolCalls(t *testing.T) {
	// Round 1: two tool calls in one response
	// Round 2: end_turn
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("c1", "bash", map[string]any{"command": "echo a"}),
					NewToolUseBlock("c2", "bash", map[string]any{"command": "echo b"}),
				},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "bash"}}

	_, err := a.Run(context.Background(), "go", defs, exec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both tool calls executed.
	if len(exec.called) != 2 {
		t.Errorf("executor called %d times, want 2", len(exec.called))
	}
}

func TestAgent_Run_ExceedsMaxTurns_FriendlyStop(t *testing.T) {
	// Every reply is tool_use — loop hits the cap. Hitting it must NOT error;
	// it returns a friendly reply with StopReason "max_turns".
	replies := make([]Reply, defaultMaxTurns+5)
	for i := range replies {
		replies[i] = Reply{
			StopReason: "tool_use",
			Blocks:     []ContentBlock{NewToolUseBlock("c", "bash", nil)},
		}
	}
	send := &fakeToolSender{replies: replies}
	exec := &fakeExecutor{}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "bash"}}

	reply, err := a.Run(context.Background(), "go", defs, exec)
	if err != nil {
		t.Fatalf("hitting the cap should not error, got %v", err)
	}
	if reply.StopReason != StopReasonMaxTurns {
		t.Errorf("StopReason = %q, want %q", reply.StopReason, StopReasonMaxTurns)
	}
	if !strings.Contains(reply.Content, "max-turns") {
		t.Errorf("reply should explain the limit, got %q", reply.Content)
	}
}

func TestAgent_Run_CustomMaxTurns(t *testing.T) {
	// MaxTurns=2 → exactly 2 provider calls, then a friendly stop.
	replies := make([]Reply, 10)
	for i := range replies {
		replies[i] = Reply{StopReason: "tool_use", Blocks: []ContentBlock{NewToolUseBlock("c", "bash", nil)}}
	}
	send := &fakeToolSender{replies: replies}
	exec := &fakeExecutor{}
	a := New(send, "m")
	a.MaxTurns = 2
	defs := []ToolDefinition{{Name: "bash"}}

	reply, err := a.Run(context.Background(), "go", defs, exec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.StopReason != StopReasonMaxTurns {
		t.Errorf("StopReason = %q, want max_turns", reply.StopReason)
	}
	if send.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (MaxTurns)", send.calls)
	}
}

func TestAgent_Run_MaxCostStops(t *testing.T) {
	// First reply reports tokens that push session cost over the budget; the
	// loop must stop before the second provider call.
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason:   "tool_use",
				Blocks:       []ContentBlock{NewToolUseBlock("c", "bash", nil)},
				InputTokens:  1_000_000, // ~$3 at default sonnet input pricing
				OutputTokens: 0,
			},
			{Content: "should not be reached", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{}
	a := New(send, "claude-sonnet-4-6")
	a.MaxCostUSD = 0.50 // well under the first call's ~$3
	defs := []ToolDefinition{{Name: "bash"}}

	reply, err := a.Run(context.Background(), "go", defs, exec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.StopReason != StopReasonMaxCost {
		t.Errorf("StopReason = %q, want max_cost", reply.StopReason)
	}
	// One provider call happened (cost only known after it); the second was
	// gated out.
	if send.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (stopped on cost before 2nd)", send.calls)
	}
}

func TestAgent_Run_SenderWithoutToolSupport_FallsBackToTurn(t *testing.T) {
	// A plain fakeSender (no ToolSender) should just do a single Turn.
	send := &fakeSender{reply: Reply{Content: "plain", StopReason: "end_turn"}}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "bash"}}
	exec := &fakeExecutor{}

	reply, err := a.Run(context.Background(), "hi", defs, exec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply.Content != "plain" {
		t.Errorf("Content = %q", reply.Content)
	}
	// Executor should NOT have been called.
	if len(exec.called) != 0 {
		t.Errorf("executor should not be called when sender has no ToolSender support")
	}
}

// ─── RunStream() + AgentEvent tests ────────────────────────────────────────

// failingExecutor returns an error from Execute, which the agent loop turns
// into a tool_result block with IsError=true.
type failingExecutor struct {
	err error
}

func (f *failingExecutor) Execute(_ context.Context, _ string, _ map[string]any) (string, error) {
	return "", f.err
}

func TestAgent_RunStream_NoTools_EmitsTextDeltaAndTurnDone(t *testing.T) {
	// Without tools, RunStream falls back through TurnStream. With a plain
	// Sender (no streaming variant), TurnStream synthesises a single
	// onChunk call with the full content. That should surface as exactly
	// one EventTextDelta + one EventTurnDone.
	send := &fakeSender{reply: Reply{Content: "hello world", StopReason: "end_turn"}}
	a := New(send, "m")

	var events []AgentEvent
	reply, err := a.RunStream(context.Background(), "hi", nil, nil, func(ev AgentEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if reply.Content != "hello world" {
		t.Errorf("reply.Content = %q", reply.Content)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (TextDelta + TurnDone), got %+v", len(events), events)
	}
	if events[0].Kind != EventTextDelta || events[0].Text != "hello world" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1].Kind != EventTurnDone || events[1].Reply == nil || events[1].Reply.Content != "hello world" {
		t.Errorf("events[1] = %+v", events[1])
	}
}

func TestAgent_RunStream_ToolUse_EmitsStartedAndDone(t *testing.T) {
	// Two-round conversation: tool_use → end_turn.
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("call-1", "terminal", map[string]any{"command": "echo hi"}),
				},
			},
			{Content: "The output was: hi", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{results: map[string]string{"terminal": "hi"}}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	var events []AgentEvent
	reply, err := a.RunStream(context.Background(), "run echo", defs, exec, func(ev AgentEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if reply.Content != "The output was: hi" {
		t.Errorf("reply.Content = %q", reply.Content)
	}

	// Expect (in order):
	// 1. EventToolStarted for call-1
	// 2. EventToolDone for call-1
	// 3. EventTextDelta with "The output was: hi" (round-2 buffered onChunk)
	// 4. EventTurnDone with the final Reply
	if len(events) < 4 {
		t.Fatalf("events = %d, want >=4, got %+v", len(events), events)
	}

	if events[0].Kind != EventToolStarted {
		t.Errorf("events[0].Kind = %q, want tool_started", events[0].Kind)
	}
	if events[0].ToolID != "call-1" || events[0].ToolName != "terminal" {
		t.Errorf("events[0] tool id/name = %q/%q", events[0].ToolID, events[0].ToolName)
	}
	if got := events[0].Input["command"]; got != "echo hi" {
		t.Errorf("events[0].Input.command = %v", got)
	}

	if events[1].Kind != EventToolDone {
		t.Errorf("events[1].Kind = %q, want tool_done", events[1].Kind)
	}
	if events[1].ToolID != "call-1" || events[1].ToolName != "terminal" {
		t.Errorf("events[1] tool id/name = %q/%q", events[1].ToolID, events[1].ToolName)
	}
	if events[1].Output != "hi" {
		t.Errorf("events[1].Output = %q, want 'hi'", events[1].Output)
	}

	// Last event must be EventTurnDone.
	last := events[len(events)-1]
	if last.Kind != EventTurnDone || last.Reply == nil {
		t.Errorf("last event = %+v, want EventTurnDone with Reply set", last)
	}
}

func TestAgent_RunStream_ToolError_EmitsErrorEvent(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("call-1", "terminal", nil),
				},
			},
			{Content: "I see the error", StopReason: "end_turn"},
		},
	}
	exec := &failingExecutor{err: fmt.Errorf("command not found")}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	var events []AgentEvent
	_, err := a.RunStream(context.Background(), "run", defs, exec, func(ev AgentEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	// First should be tool_started, second tool_error.
	if len(events) < 2 {
		t.Fatalf("events = %d, want >=2", len(events))
	}
	if events[0].Kind != EventToolStarted {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1].Kind != EventToolError {
		t.Errorf("events[1] = %+v, want tool_error", events[1])
	}
	if events[1].Err != "command not found" {
		t.Errorf("events[1].Err = %q", events[1].Err)
	}
}

func TestAgent_RunStream_NilHandlerTolerated(t *testing.T) {
	// nil handler must be safe — events are dropped, the run still completes.
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", nil)},
			},
			{Content: "ok", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	reply, err := a.RunStream(context.Background(), "go", defs, exec, nil)
	if err != nil {
		t.Fatalf("RunStream(nil handler): %v", err)
	}
	if reply.Content != "ok" {
		t.Errorf("reply.Content = %q", reply.Content)
	}
}

func TestAgent_RunStream_TruncatesLongToolOutput(t *testing.T) {
	long := strings.Repeat("x", EventToolOutputCap+200)
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", nil)},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{results: map[string]string{"terminal": long}}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	var seen AgentEvent
	_, err := a.RunStream(context.Background(), "go", defs, exec, func(ev AgentEvent) {
		if ev.Kind == EventToolDone {
			seen = ev
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen.Output) > EventToolOutputCap+len("…[truncated]") {
		t.Errorf("Output not truncated: len=%d", len(seen.Output))
	}
	if !strings.HasSuffix(seen.Output, "…[truncated]") {
		t.Errorf("Output should be marked truncated: %q", seen.Output[:50])
	}
}

// ─── StreamingToolExecutor / EventToolProgress tests ────────────────────────

// streamingFakeExecutor implements StreamingToolExecutor — emits N progress
// chunks before returning the aggregated result.
type streamingFakeExecutor struct {
	results map[string]string
	chunks  []string
	called  []string
}

func (f *streamingFakeExecutor) Execute(_ context.Context, name string, _ map[string]any) (string, error) {
	f.called = append(f.called, name)
	if v, ok := f.results[name]; ok {
		return v, nil
	}
	return "ok", nil
}

func (f *streamingFakeExecutor) ExecuteStream(
	_ context.Context, name string, _ map[string]any, progress func(string),
) (string, error) {
	f.called = append(f.called, name)
	for _, c := range f.chunks {
		if progress != nil {
			progress(c)
		}
	}
	if v, ok := f.results[name]; ok {
		return v, nil
	}
	return "ok", nil
}

func TestAgent_RunStream_StreamingExecutor_EmitsProgress(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("c1", "terminal", map[string]any{"command": "echo"}),
				},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}

	exec := &streamingFakeExecutor{
		chunks:  []string{"line A", "line B", "line C"},
		results: map[string]string{"terminal": "line A\nline B\nline C"},
	}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	var progressEvents []AgentEvent
	_, err := a.RunStream(context.Background(), "go", defs, exec, func(ev AgentEvent) {
		if ev.Kind == EventToolProgress {
			progressEvents = append(progressEvents, ev)
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if len(progressEvents) != 3 {
		t.Fatalf("expected 3 progress events, got %d: %+v", len(progressEvents), progressEvents)
	}
	for i, want := range []string{"line A", "line B", "line C"} {
		if progressEvents[i].Chunk != want {
			t.Errorf("event[%d].Chunk = %q, want %q", i, progressEvents[i].Chunk, want)
		}
		if progressEvents[i].ToolID != "c1" {
			t.Errorf("event[%d].ToolID = %q", i, progressEvents[i].ToolID)
		}
		if progressEvents[i].ToolName != "terminal" {
			t.Errorf("event[%d].ToolName = %q", i, progressEvents[i].ToolName)
		}
	}
}

func TestAgent_RunStream_NonStreamingExecutor_NoProgressEvents(t *testing.T) {
	// fakeExecutor only implements ToolExecutor (not StreamingToolExecutor)
	// — RunStream must NOT call ExecuteStream and therefore emits zero
	// progress events.
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", nil)},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{} // plain ToolExecutor
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	var progressEvents int
	_, err := a.RunStream(context.Background(), "go", defs, exec, func(ev AgentEvent) {
		if ev.Kind == EventToolProgress {
			progressEvents++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if progressEvents != 0 {
		t.Errorf("non-streaming executor should produce 0 progress events; got %d", progressEvents)
	}
}

func TestAgent_RunStream_StreamingExecutor_NilHandler_StillRuns(t *testing.T) {
	// handler=nil + streaming executor: dispatchTools must degrade to the
	// non-streaming Execute path (progress would have nowhere to go).
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", nil)},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &streamingFakeExecutor{chunks: []string{"a", "b"}}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	reply, err := a.RunStream(context.Background(), "go", defs, exec, nil)
	if err != nil {
		t.Fatalf("RunStream(nil handler): %v", err)
	}
	if reply.Content != "done" {
		t.Errorf("reply.Content = %q", reply.Content)
	}
	// The fake's chunks should NOT have been emitted because handler is nil
	// → dispatchTools called Execute, not ExecuteStream.
	// (We can't directly observe "ExecuteStream wasn't called" from outside,
	// but the absence of side effects + clean completion is the contract.)
}

// ─── EventToolInputDelta plumbing tests ─────────────────────────────────────

// fakeToolStreamingSender implements ToolStreamingSender — exercises the
// onToolDelta plumbing in RunStream → ToolStreamingSender →
// EventToolInputDelta.
type fakeToolStreamingSender struct {
	fakeToolSender
	textChunks []string
	toolDeltas []struct{ id, name, partialJSON string }
}

func (f *fakeToolStreamingSender) StreamMessagesWithTools(
	_ context.Context, _, _ string, _ []Message, _ int,
	_ []ToolDefinition,
	onChunk func(string),
	onToolDelta ToolInputDeltaFunc,
) (Reply, error) {
	// Replay scripted callbacks only on the FIRST call so multi-iteration
	// loops don't double-fire deltas attached to the first tool call.
	if f.calls == 0 {
		for _, c := range f.textChunks {
			if onChunk != nil {
				onChunk(c)
			}
		}
		for _, d := range f.toolDeltas {
			if onToolDelta != nil {
				onToolDelta(d.id, d.name, d.partialJSON)
			}
		}
	}
	return f.nextReply()
}

// fakeToolStreamingSender must also satisfy StreamingSender (it embeds
// fakeToolSender, which is only Sender + ToolSender). Add the streaming
// text method explicitly so RunStream type-asserts the right interface.
func (f *fakeToolStreamingSender) StreamMessages(
	_ context.Context, _, _ string, _ []Message, _ int, onChunk func(string),
) (Reply, error) {
	for _, c := range f.textChunks {
		if onChunk != nil {
			onChunk(c)
		}
	}
	return f.nextReply()
}

func TestAgent_RunStream_ToolInputDeltaEventsFire(t *testing.T) {
	send := &fakeToolStreamingSender{
		toolDeltas: []struct{ id, name, partialJSON string }{
			{"c1", "terminal", `{"comm`},
			{"c1", "terminal", `and":"`},
			{"c1", "terminal", `ls"}`},
		},
	}
	send.replies = []Reply{
		{
			StopReason: "tool_use",
			Blocks: []ContentBlock{
				NewToolUseBlock("c1", "terminal", map[string]any{"command": "ls"}),
			},
		},
		{Content: "done", StopReason: "end_turn"},
	}
	exec := &fakeExecutor{}
	a := New(send, "m")
	defs := []ToolDefinition{{Name: "terminal"}}

	var deltas []AgentEvent
	_, err := a.RunStream(context.Background(), "ls please", defs, exec, func(ev AgentEvent) {
		if ev.Kind == EventToolInputDelta {
			deltas = append(deltas, ev)
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if len(deltas) != 3 {
		t.Fatalf("expected 3 input-delta events, got %d: %+v", len(deltas), deltas)
	}
	for i, want := range []string{`{"comm`, `and":"`, `ls"}`} {
		if deltas[i].InputDelta != want {
			t.Errorf("delta[%d].InputDelta = %q, want %q", i, deltas[i].InputDelta, want)
		}
		if deltas[i].ToolID != "c1" {
			t.Errorf("delta[%d].ToolID = %q", i, deltas[i].ToolID)
		}
		if deltas[i].ToolName != "terminal" {
			t.Errorf("delta[%d].ToolName = %q", i, deltas[i].ToolName)
		}
	}
}

func TestAgent_RunStream_NilHandler_TolerantOfInputDeltas(t *testing.T) {
	// A nil handler with a sender that fires onToolDelta must NOT crash —
	// the agent layer swallows the call (the closure check `handler == nil`).
	send := &fakeToolStreamingSender{
		toolDeltas: []struct{ id, name, partialJSON string }{
			{"c1", "x", `{"a":1}`},
		},
	}
	send.replies = []Reply{
		{StopReason: "tool_use", Blocks: []ContentBlock{NewToolUseBlock("c1", "x", nil)}},
		{Content: "ok", StopReason: "end_turn"},
	}
	exec := &fakeExecutor{}
	a := New(send, "m")
	if _, err := a.RunStream(context.Background(), "hi", []ToolDefinition{{Name: "x"}}, exec, nil); err != nil {
		t.Fatalf("RunStream(nil handler): %v", err)
	}
}

// ─── PermissionGate tests ──────────────────────────────────────────────────

// fakeGate implements PermissionGate. allow controls the verdict; denied
// records the calls that were rejected.
type fakeGate struct {
	allow  bool
	reason string
	checks []string // tool names the gate was asked about
}

func (g *fakeGate) Check(_ context.Context, name string, _ map[string]any) (bool, string) {
	g.checks = append(g.checks, name)
	return g.allow, g.reason
}

func TestAgent_Run_GateDenies_ToolNotExecuted(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("c1", "terminal", map[string]any{"command": "rm -rf /"}),
				},
			},
			{Content: "understood, I won't do that", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{}
	gate := &fakeGate{allow: false, reason: "permission_denied: terminal rm -rf"}

	a := New(send, "m")
	a.Gate = gate
	defs := []ToolDefinition{{Name: "terminal"}}

	reply, err := a.Run(context.Background(), "delete everything", defs, exec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Executor must NOT have run.
	if len(exec.called) != 0 {
		t.Errorf("denied tool should not execute; called=%v", exec.called)
	}
	// Gate was consulted for the terminal call.
	if len(gate.checks) != 1 || gate.checks[0] != "terminal" {
		t.Errorf("gate checks = %v", gate.checks)
	}
	if reply.Content != "understood, I won't do that" {
		t.Errorf("Content = %q", reply.Content)
	}

	// The denial reason should be in the tool_result fed back to the LLM.
	snap := a.History.Snapshot()
	var foundReason bool
	for _, m := range snap {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" && b.IsError && b.Result == "permission_denied: terminal rm -rf" {
				foundReason = true
			}
		}
	}
	if !foundReason {
		t.Errorf("denial reason not found in tool_result history: %+v", snap)
	}
}

func TestAgent_Run_GateAllows_ToolExecutes(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", map[string]any{"command": "ls"})},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{results: map[string]string{"terminal": "file1\nfile2"}}
	gate := &fakeGate{allow: true}

	a := New(send, "m")
	a.Gate = gate
	defs := []ToolDefinition{{Name: "terminal"}}

	if _, err := a.Run(context.Background(), "list files", defs, exec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(exec.called) != 1 || exec.called[0] != "terminal" {
		t.Errorf("allowed tool should execute once; called=%v", exec.called)
	}
}

func TestAgent_Run_NilGate_AllToolsExecute(t *testing.T) {
	// Backward compatibility: no gate set → tools run as before.
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", nil)},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{}
	a := New(send, "m") // a.Gate == nil
	defs := []ToolDefinition{{Name: "terminal"}}

	if _, err := a.Run(context.Background(), "go", defs, exec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(exec.called) != 1 {
		t.Errorf("nil gate should allow execution; called=%v", exec.called)
	}
}

func TestAgent_RunStream_GateDenies_EmitsErrorEvent(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks:     []ContentBlock{NewToolUseBlock("c1", "terminal", map[string]any{"command": "sudo rm"})},
			},
			{Content: "ok", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{}
	gate := &fakeGate{allow: false, reason: "blocked by policy"}
	a := New(send, "m")
	a.Gate = gate
	defs := []ToolDefinition{{Name: "terminal"}}

	var sawError bool
	_, err := a.RunStream(context.Background(), "go", defs, exec, func(ev AgentEvent) {
		if ev.Kind == EventToolError && ev.Err == "blocked by policy" {
			sawError = true
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if len(exec.called) != 0 {
		t.Errorf("denied tool should not execute; called=%v", exec.called)
	}
	if !sawError {
		t.Errorf("expected EventToolError carrying the denial reason")
	}
}

func TestAccrueChildUsage_FoldsIntoSessionTotals(t *testing.T) {
	a := New(nil, "m")
	a.AccrueChildUsage(100, 50)
	a.AccrueChildUsage(200, 75)
	in, out := a.SessionTokens()
	if in != 300 || out != 125 {
		t.Errorf("SessionTokens after two AccrueChildUsage = (%d,%d), want (300,125)", in, out)
	}
}
