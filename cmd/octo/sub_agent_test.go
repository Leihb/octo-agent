package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

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

func (nilExecutor) Execute(_ context.Context, _ string, _ map[string]any) (string, error) {
	return "", nil
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
