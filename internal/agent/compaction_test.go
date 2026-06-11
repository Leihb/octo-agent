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

func TestContextWindow(t *testing.T) {
	if got := contextWindow("claude-haiku-4-5-20251001"); got != 256_000 {
		t.Errorf("claude window = %d, want 256000", got)
	}
	if got := contextWindow("k2.6"); got != 256_000 {
		t.Errorf("kimi k2.6 window = %d, want 256000", got)
	}
	if got := contextWindow("deepseek-v4-pro"); got != 1_000_000 {
		t.Errorf("deepseek-v4-pro window = %d, want 1000000", got)
	}
	if got := contextWindow("qwen-3.7-max"); got != 1_000_000 {
		t.Errorf("qwen-3.7-max window = %d, want 1000000", got)
	}
	if got := contextWindow("gpt-5.5"); got != 1_000_000 {
		t.Errorf("gpt-5.5 window = %d, want 1000000", got)
	}
	if got := contextWindow("gemini-3.5-flash"); got != 1_000_000 {
		t.Errorf("gemini-3.5-flash window = %d, want 1000000", got)
	}
	if got := contextWindow("unknown-model-xyz"); got != defaultContextWindow {
		t.Errorf("unknown model window = %d, want %d (conservative default)", got, defaultContextWindow)
	}
}

func TestCompactTriggerTokens(t *testing.T) {
	a := New(&summarizeFake{}, "unknown-model-xyz") // unknown model → default window

	a.CompactThreshold = -1
	if got := a.compactTriggerTokens(); got != 0 {
		t.Errorf("disabled trigger = %d, want 0", got)
	}

	a.CompactThreshold = 0 // auto
	want := int(float64(defaultContextWindow) * compactThresholdFraction)
	if got := a.compactTriggerTokens(); got != want {
		t.Errorf("auto trigger = %d, want %d", got, want)
	}

	a.CompactThreshold = 4242 // explicit
	if got := a.compactTriggerTokens(); got != 4242 {
		t.Errorf("explicit trigger = %d, want 4242", got)
	}

	// Custom auto fraction (50%).
	a.CompactThreshold = 0
	a.CompactAutoFraction = 0.5
	want50 := int(float64(defaultContextWindow) * 0.5)
	if got := a.compactTriggerTokens(); got != want50 {
		t.Errorf("custom 50%% trigger = %d, want %d", got, want50)
	}

	// Fraction clamped to 1.0 when > 1.
	a.CompactAutoFraction = 2.0
	want100 := defaultContextWindow
	if got := a.compactTriggerTokens(); got != want100 {
		t.Errorf("clamped 200%% trigger = %d, want %d", got, want100)
	}
}

// TestMaybeCompact_AutoDefaultTriggers proves the C6 flip: with CompactThreshold
// left at 0, a context past the window-relative trigger compacts automatically.
func TestMaybeCompact_AutoDefaultTriggers(t *testing.T) {
	f := &summarizeFake{summary: "S"}
	a := New(f, "unknown-model-xyz") // CompactThreshold defaults to 0 (auto), unknown model → default window
	trigger := int(float64(defaultContextWindow) * compactThresholdFraction)
	// Build a history whose estimated token count exceeds the trigger.
	longMsg := strings.Repeat("x ", 500) // ~250 tokens each
	for estimateMessages(a.History.Snapshot()) <= trigger {
		a.History.Append(NewUserMessage(longMsg))
		a.History.Append(NewAssistantMessage(longMsg))
	}
	if err := a.maybeCompact(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Errorf("auto default should compact once; calls=%d", f.calls)
	}
}

func TestMaybeCompact_DisabledOrBelowThreshold(t *testing.T) {
	f := &summarizeFake{summary: "S"}
	a := New(f, "m")
	longMsg := strings.Repeat("x ", 500) // ~250 tokens each
	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(longMsg))
		a.History.Append(NewAssistantMessage(longMsg))
	}

	// Disabled (negative threshold) — even a huge context must not compact.
	a.CompactThreshold = -1
	if err := a.maybeCompact(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || a.History.Len() != 12 {
		t.Errorf("disabled: should not compact; calls=%d len=%d", f.calls, a.History.Len())
	}

	// Enabled but history still under threshold.
	a.CompactThreshold = 100000 // way above the ~1500 tokens of the 12 messages
	if err := a.maybeCompact(context.Background(), nil); err != nil {
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

	longMsg := strings.Repeat("x ", 500) // ~250 tokens each
	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(longMsg))
		a.History.Append(NewAssistantMessage(longMsg))
	}
	// History now well over the 100-token threshold.

	if err := a.maybeCompact(context.Background(), nil); err != nil {
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
	if snap[len(snap)-1].Content != longMsg {
		t.Errorf("last message = %q, want %q", snap[len(snap)-1].Content, longMsg)
	}
	// Trigger reset so we don't immediately re-compact.
	if a.lastInputTokens != 0 {
		t.Errorf("lastInputTokens should reset to 0, got %d", a.lastInputTokens)
	}
}

// streamingSummarizeFake implements StreamingSender, emitting the canned
// summary in chunks so the compaction-progress events can be observed.
type streamingSummarizeFake struct {
	summary string
	chunks  int
}

func (f *streamingSummarizeFake) SendMessages(_ context.Context, _, _ string, _ []Message, _ int) (Reply, error) {
	return Reply{Content: f.summary}, nil
}

func (f *streamingSummarizeFake) StreamMessages(_ context.Context, _, _ string, _ []Message, _ int, onChunk func(string), _ func(string)) (Reply, error) {
	// Emit one rune at a time so SummaryTokens grows monotonically across events.
	for _, r := range f.summary {
		f.chunks++
		onChunk(string(r))
	}
	return Reply{Content: f.summary, InputTokens: 5, OutputTokens: 3}, nil
}

// TestMaybeCompact_EmitsProgressEvents proves compaction surfaces a
// started → progress(streaming) → done event sequence with sane stats, so the
// TUI can show a live "compacting conversation history" indicator.
func TestMaybeCompact_EmitsProgressEvents(t *testing.T) {
	f := &streamingSummarizeFake{summary: "GOAL build X. Touched main.go."}
	a := New(f, "m")
	a.CompactThreshold = 100

	longMsg := strings.Repeat("x ", 500) // ~250 tokens each
	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(longMsg))
		a.History.Append(NewAssistantMessage(longMsg))
	}

	var events []AgentEvent
	handler := func(ev AgentEvent) { events = append(events, ev) }

	if err := a.maybeCompact(context.Background(), handler); err != nil {
		t.Fatal(err)
	}

	if len(events) < 3 {
		t.Fatalf("expected started + progress + done events, got %d", len(events))
	}

	// First event: started, with the pre-compaction estimate and fold counts.
	first := events[0]
	if first.Kind != EventCompactStarted {
		t.Errorf("first event = %q, want compact_started", first.Kind)
	}
	if first.Compact == nil || first.Compact.BeforeTokens <= 0 {
		t.Errorf("started event missing BeforeTokens: %+v", first.Compact)
	}
	if first.Compact.KeptTurns != compactKeepTurns {
		t.Errorf("started KeptTurns = %d, want %d", first.Compact.KeptTurns, compactKeepTurns)
	}
	if first.Compact.MaxTokens != summarizeMaxTokens {
		t.Errorf("started MaxTokens = %d, want %d", first.Compact.MaxTokens, summarizeMaxTokens)
	}

	// Middle: at least one progress event whose summary estimate is monotonic.
	var progress []AgentEvent
	prevTokens := -1
	for _, ev := range events {
		if ev.Kind != EventCompactProgress {
			continue
		}
		progress = append(progress, ev)
		if ev.Chunk == "" {
			t.Errorf("progress event missing Chunk")
		}
		if ev.Compact == nil || ev.Compact.SummaryTokens < prevTokens {
			t.Errorf("progress SummaryTokens not monotonic: %+v (prev=%d)", ev.Compact, prevTokens)
		}
		prevTokens = ev.Compact.SummaryTokens
	}
	if len(progress) == 0 {
		t.Errorf("expected at least one compact_progress event")
	}

	// Last event: done, with after < before (a real reduction happened).
	last := events[len(events)-1]
	if last.Kind != EventCompactDone {
		t.Errorf("last event = %q, want compact_done", last.Kind)
	}
	if last.Compact == nil || last.Compact.AfterTokens <= 0 || last.Compact.AfterTokens >= last.Compact.BeforeTokens {
		t.Errorf("done event should show reduction: %+v", last.Compact)
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

	// Seed a long prior history whose estimated size exceeds the threshold.
	longMsg := strings.Repeat("x ", 500) // ~250 tokens each
	for i := 0; i < 6; i++ {
		a.History.Append(NewUserMessage(longMsg))
		a.History.Append(NewAssistantMessage(longMsg))
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

// failingSender always errors — used to prove the lite→primary fallback.
type failingSender struct{ calls int }

func (f *failingSender) SendMessages(_ context.Context, _, _ string, _ []Message, _ int) (Reply, error) {
	f.calls++
	return Reply{}, fmt.Errorf("lite endpoint down")
}

// modelRecordingFake records which model each call was made with.
type modelRecordingFake struct {
	summary string
	models  []string
}

func (f *modelRecordingFake) SendMessages(_ context.Context, model, _ string, _ []Message, _ int) (Reply, error) {
	f.models = append(f.models, model)
	return Reply{Content: f.summary}, nil
}

func TestSummarize_UsesLiteModelWhenSet(t *testing.T) {
	primary := &modelRecordingFake{summary: "primary summary"}
	lite := &modelRecordingFake{summary: "lite summary"}

	a := New(primary, "big-model")
	a.LiteSender = lite
	a.LiteModel = "lite-model"

	msgs := []Message{NewUserMessage("hi"), NewAssistantMessage("hello")}
	sum, err := a.summarize(context.Background(), msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "lite summary" {
		t.Errorf("summary = %q, want the lite sender's", sum)
	}
	if len(lite.models) != 1 || lite.models[0] != "lite-model" {
		t.Errorf("lite calls = %v, want one call with lite-model", lite.models)
	}
	if len(primary.models) != 0 {
		t.Errorf("primary sender called %v times, want 0", len(primary.models))
	}
}

func TestSummarize_FallsBackToPrimaryOnLiteError(t *testing.T) {
	primary := &modelRecordingFake{summary: "primary summary"}
	lite := &failingSender{}

	a := New(primary, "big-model")
	a.LiteSender = lite
	a.LiteModel = "lite-model"

	msgs := []Message{NewUserMessage("hi"), NewAssistantMessage("hello")}
	sum, err := a.summarize(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("summarize must survive a lite failure: %v", err)
	}
	if sum != "primary summary" {
		t.Errorf("summary = %q, want the primary fallback", sum)
	}
	if lite.calls != 1 {
		t.Errorf("lite calls = %d, want 1", lite.calls)
	}
	if len(primary.models) != 1 || primary.models[0] != "big-model" {
		t.Errorf("primary calls = %v, want one call with big-model", primary.models)
	}
}

func TestSummarize_NoLiteConfiguredUsesPrimary(t *testing.T) {
	primary := &modelRecordingFake{summary: "primary summary"}
	a := New(primary, "big-model")

	msgs := []Message{NewUserMessage("hi"), NewAssistantMessage("hello")}
	sum, err := a.summarize(context.Background(), msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "primary summary" || len(primary.models) != 1 {
		t.Errorf("got %q after %d calls; want primary summary after 1", sum, len(primary.models))
	}
}
