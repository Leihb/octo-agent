package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// subAgentSender returns a canned reply and counts how often it's been called.
// The reply has no tool_use blocks so the child's Run terminates after one
// turn — that's all we need to exercise the spawner glue.
type subAgentSender struct {
	reply        string
	inputTokens  int
	outputTokens int
	calls        int32
	lastSystem   string
	lastModel    string
	lastMessages []agent.Message
}

func (s *subAgentSender) SendMessages(_ context.Context, model, system string, msgs []agent.Message, _ int) (agent.Reply, error) {
	atomic.AddInt32(&s.calls, 1)
	s.lastSystem = system
	s.lastModel = model
	s.lastMessages = msgs
	return agent.Reply{
		Content:      s.reply,
		InputTokens:  s.inputTokens,
		OutputTokens: s.outputTokens,
	}, nil
}

// nilExecutor exists so child Run can be called even though our stub Sender
// never produces tool_use blocks (so Execute is never invoked).
type nilExecutor struct{}

func (nilExecutor) Execute(_ context.Context, _ string, _ map[string]any) (agent.ToolResult, error) {
	return agent.ToolResult{Text: ""}, nil
}

func TestAgentSpawner_RunsChildAndRollsTokensIntoParent(t *testing.T) {
	send := &subAgentSender{reply: "sub-agent answer", inputTokens: 200, outputTokens: 80}
	parent := agent.New(send, "parent-model")
	parent.System = "PARENT SYSTEM"
	parent.MaxTokens = 4096

	parentTools := []agent.ToolDefinition{
		{Name: "read_file"},
		{Name: "grep"},
		{Name: "launch_agent"},
	}
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return parentTools })

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{
		Description: "Investigate",
		Prompt:      "What is in the cache module?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "sub-agent answer" {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.InputTokens != 200 || res.OutputTokens != 80 {
		t.Errorf("token usage in result = (%d,%d)", res.InputTokens, res.OutputTokens)
	}

	// Tokens must roll back into the parent's session totals.
	in, out := parent.SessionTokens()
	if in != 200 || out != 80 {
		t.Errorf("parent session tokens = (%d,%d), want (200,80)", in, out)
	}

	// Child must have inherited the parent's system + model fallback.
	if send.lastSystem != "PARENT SYSTEM" {
		t.Errorf("child system = %q, want parent's", send.lastSystem)
	}
	if send.lastModel != "parent-model" {
		t.Errorf("child model = %q, want parent's default", send.lastModel)
	}
}

func TestAgentSpawner_AppliesToolAllowlist(t *testing.T) {
	send := &subAgentSender{reply: "ok"}
	parent := agent.New(send, "parent-model")
	parentTools := []agent.ToolDefinition{
		{Name: "read_file"},
		{Name: "grep"},
		{Name: "terminal"},
		{Name: "launch_agent"},
	}
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return parentTools })

	// Allowlist restricts to read_file + grep. launch_agent always excluded.
	got := filterChildTools(parentTools, []string{"read_file", "grep"})
	if len(got) != 2 {
		t.Fatalf("filtered tools len = %d, want 2: %+v", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name}
	if !strings.Contains(strings.Join(names, ","), "read_file") || !strings.Contains(strings.Join(names, ","), "grep") {
		t.Errorf("allowlist not applied: %v", names)
	}

	// No allowlist → all parent tools minus launch_agent.
	got = filterChildTools(parentTools, nil)
	if len(got) != 3 {
		t.Errorf("nil allowlist should keep all non-launch_agent tools: %+v", got)
	}
	for _, td := range got {
		if td.Name == "launch_agent" {
			t.Errorf("launch_agent must always be filtered out (no recursion)")
		}
	}

	// Spawning should run with the inferred childTools (no error path here —
	// we just verify the spawner doesn't choke when an allowlist is present).
	_, err := sp.Spawn(context.Background(), tools.SpawnRequest{
		Description: "x",
		Prompt:      "y",
		Tools:       []string{"read_file"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentSpawner_ModelOverride(t *testing.T) {
	send := &subAgentSender{reply: "ok"}
	parent := agent.New(send, "parent-model")
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	_, err := sp.Spawn(context.Background(), tools.SpawnRequest{
		Description: "x",
		Prompt:      "y",
		Model:       "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if send.lastModel != "claude-haiku-4-5" {
		t.Errorf("model override ignored: child ran with %q", send.lastModel)
	}
}

func TestAgentSpawner_SpawnReturnsIDAndContinueResumesSameChild(t *testing.T) {
	send := &subAgentSender{reply: "round one", inputTokens: 200, outputTokens: 80}
	parent := agent.New(send, "parent-model")
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{Description: "x", Prompt: "first task"})
	if err != nil {
		t.Fatal(err)
	}
	if res.AgentID == "" {
		t.Fatal("Spawn should return a non-empty AgentID")
	}
	// First Run sent a single-message history.
	if got := len(send.lastMessages); got != 1 {
		t.Fatalf("after Spawn, child saw %d messages, want 1", got)
	}

	send.reply = "round two"
	res2, err := sp.Continue(context.Background(), res.AgentID, "second task")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Reply != "round two" {
		t.Errorf("Continue reply = %q", res2.Reply)
	}
	// Continuation carries history: [user1, assistant1, user2] = 3 messages.
	if got := len(send.lastMessages); got != 3 {
		t.Errorf("after Continue, child saw %d messages, want 3 (history carried over)", got)
	}
}

func TestAgentSpawner_ContinueAccruesOnlyDeltaTokens(t *testing.T) {
	send := &subAgentSender{reply: "ok", inputTokens: 200, outputTokens: 80}
	parent := agent.New(send, "parent-model")
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{Description: "x", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens != 200 || res.OutputTokens != 80 {
		t.Errorf("Spawn delta tokens = (%d,%d), want (200,80)", res.InputTokens, res.OutputTokens)
	}

	res2, err := sp.Continue(context.Background(), res.AgentID, "again")
	if err != nil {
		t.Fatal(err)
	}
	// The child's cumulative SessionTokens is now (400,160); Continue must
	// report and accrue only this round's delta (200,80), not the cumulative.
	if res2.InputTokens != 200 || res2.OutputTokens != 80 {
		t.Errorf("Continue delta tokens = (%d,%d), want (200,80)", res2.InputTokens, res2.OutputTokens)
	}
	in, out := parent.SessionTokens()
	if in != 400 || out != 160 {
		t.Errorf("parent session tokens = (%d,%d), want (400,160) — double-counting bug if 600/240", in, out)
	}
}

func TestAgentSpawner_ContinueUnknownID(t *testing.T) {
	send := &subAgentSender{reply: "ok"}
	parent := agent.New(send, "parent-model")
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	_, err := sp.Continue(context.Background(), "nope", "hi")
	if err == nil || !strings.Contains(err.Error(), "no longer alive") {
		t.Errorf("expected 'no longer alive' error for unknown id, got %v", err)
	}
}

func TestAgentSpawner_ConcurrentContinueSerialized(t *testing.T) {
	send := &subAgentSender{reply: "ok", inputTokens: 10, outputTokens: 5}
	parent := agent.New(send, "parent-model")
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	res, err := sp.Spawn(context.Background(), tools.SpawnRequest{Description: "x", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sp.Continue(context.Background(), res.AgentID, "more"); err != nil {
				t.Errorf("Continue: %v", err)
			}
		}()
	}
	wg.Wait()

	// per-child mutex serializes the runs, so the cumulative total is exact:
	// 1 Spawn + n Continue rounds. (Run under -race to catch History races.)
	in, out := parent.SessionTokens()
	if in != (n+1)*10 || out != (n+1)*5 {
		t.Errorf("parent tokens = (%d,%d), want (%d,%d)", in, out, (n+1)*10, (n+1)*5)
	}
}

func TestFilterChildTools_DropsSendMessage(t *testing.T) {
	parentTools := []agent.ToolDefinition{
		{Name: "read_file"},
		{Name: "launch_agent"},
		{Name: "send_message"},
	}
	got := filterChildTools(parentTools, nil)
	for _, td := range got {
		if td.Name == "send_message" || td.Name == "launch_agent" {
			t.Errorf("child toolbelt must not contain %q", td.Name)
		}
	}
	if len(got) != 1 || got[0].Name != "read_file" {
		t.Errorf("expected only read_file to survive, got %+v", got)
	}
}

func TestChildRegistry_LRUEviction(t *testing.T) {
	reg := newChildRegistry()
	ids := make([]string, 0, maxLiveChildren+1)
	for i := 0; i < maxLiveChildren+1; i++ {
		ids = append(ids, reg.put(&liveChild{}))
	}
	if _, ok := reg.get(ids[0]); ok {
		t.Error("the least-recently-used child should have been evicted")
	}
	if _, ok := reg.get(ids[len(ids)-1]); !ok {
		t.Error("the most-recently-added child should still be alive")
	}
	if len(reg.m) != maxLiveChildren {
		t.Errorf("registry size = %d, want %d", len(reg.m), maxLiveChildren)
	}
}

func TestChildRegistry_LRUKeepsRecentlyUsed(t *testing.T) {
	reg := newChildRegistry()
	first := reg.put(&liveChild{})
	for i := 0; i < maxLiveChildren-1; i++ {
		reg.put(&liveChild{})
	}
	// Touch `first` so it's no longer the LRU victim, then overflow by one.
	if _, ok := reg.get(first); !ok {
		t.Fatal("first should still be alive before overflow")
	}
	reg.put(&liveChild{}) // overflow → evicts the now-oldest, not `first`
	if _, ok := reg.get(first); !ok {
		t.Error("a recently-touched child must survive LRU eviction")
	}
}

func TestChildRegistry_TTLEviction(t *testing.T) {
	now := time.Now()
	reg := newChildRegistry()
	reg.now = func() time.Time { return now }

	id := reg.put(&liveChild{})
	if _, ok := reg.get(id); !ok {
		t.Fatal("child should be alive immediately after put")
	}
	now = now.Add(childIdleTTL + time.Minute)
	if _, ok := reg.get(id); ok {
		t.Error("child should be evicted after idle TTL")
	}
}

func TestAgentSpawner_MarksContextSoRecursionRefused(t *testing.T) {
	send := &subAgentSender{reply: "ok"}
	parent := agent.New(send, "parent-model")
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	// Wire SpawnRequest into a real launch_agent tool that checks the sub-agent
	// context flag. After Spawn returns, the OUTER context shouldn't be marked
	// (only descendants of the spawn call are), but inside Spawn the child's
	// Run sees the marked context. We can't reach that from the outside cleanly,
	// so verify behavior by stubbing the spawner and asserting it would refuse
	// a recursive launch_agent call.
	tools.SetSpawner(sp)
	t.Cleanup(func() { tools.SetSpawner(nil) })

	// Simulating a launch_agent execution from inside a sub-agent's ctx:
	ctx := tools.WithSubAgentMarker(context.Background())
	_, err := (tools.LaunchAgentTool{}).Execute(ctx, "launch_agent", map[string]any{
		"description": "nested",
		"prompt":      "recurse",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot spawn") {
		t.Errorf("recursive launch_agent should be refused, got %v", err)
	}
}
