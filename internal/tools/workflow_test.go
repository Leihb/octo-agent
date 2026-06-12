package tools

import (
	"context"
	"strings"
	"testing"
)

// TestDefaultTools_WorkflowGatedOnSpawner verifies the workflow tool is only
// advertised when a Spawner is registered (it has nothing to delegate to
// otherwise).
func TestDefaultTools_WorkflowGatedOnSpawner(t *testing.T) {
	SetSpawner(nil)
	t.Cleanup(func() { SetSpawner(nil) })

	if advertisedNames()["workflow"] {
		t.Error("workflow should be absent when no Spawner is configured")
	}
	SetSpawner(&fakeSpawner{})
	if !advertisedNames()["workflow"] {
		t.Error("workflow should appear once a Spawner is registered")
	}
}

// replySpawner echoes the prompt so tests can assert the agent()→Spawner path.
type replySpawner struct{}

func (replySpawner) Spawn(_ context.Context, req SpawnRequest) (SpawnResult, error) {
	return SpawnResult{Reply: "R[" + req.Prompt + "]", OutputTokens: 3}, nil
}
func (replySpawner) Continue(_ context.Context, _, _ string) (SpawnResult, error) {
	return SpawnResult{}, nil
}

// TestWorkflowTool_Execute drives a parallel() script through the tool and
// confirms each agent() call reaches the Spawner and results come back in order.
func TestWorkflowTool_Execute(t *testing.T) {
	SetSpawner(replySpawner{})
	t.Cleanup(func() { SetSpawner(nil) })

	res, err := WorkflowTool{}.Execute(context.Background(), "call-1", map[string]any{
		"script": `parallel(%w[a b c]) { |x| agent(x) }.join(",")`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Text != "R[a],R[b],R[c]" {
		t.Errorf("Text = %q, want R[a],R[b],R[c]", res.Text)
	}
}

// TestWorkflowTool_ExecuteStream verifies live progress chunks flow through the
// streaming callback: log() output plus each agent's start/finish.
func TestWorkflowTool_ExecuteStream(t *testing.T) {
	SetSpawner(replySpawner{})
	t.Cleanup(func() { SetSpawner(nil) })

	var chunks []string
	res, err := WorkflowTool{}.ExecuteStream(context.Background(), "c",
		map[string]any{"script": `log("begin"); parallel(%w[a b]) { |x| agent(x) }.join(",")`},
		func(chunk string) { chunks = append(chunks, chunk) })
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if !strings.Contains(res.Text, "R[a],R[b]") {
		t.Errorf("Text = %q", res.Text)
	}
	joined := strings.Join(chunks, "\n")
	for _, want := range []string{"begin", "→ a", "→ b", "✓ a", "✓ b"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress chunks missing %q; got:\n%s", want, joined)
		}
	}
}

func TestWorkflowTool_RefusesInSubAgent(t *testing.T) {
	SetSpawner(replySpawner{})
	t.Cleanup(func() { SetSpawner(nil) })

	ctx := WithSubAgentMarker(context.Background())
	_, err := WorkflowTool{}.Execute(ctx, "c", map[string]any{"script": `"x"`})
	if err == nil || !strings.Contains(err.Error(), "cannot run a workflow") {
		t.Errorf("err = %v, want sub-agent refusal", err)
	}
}

func TestWorkflowTool_RequiresScript(t *testing.T) {
	SetSpawner(replySpawner{})
	t.Cleanup(func() { SetSpawner(nil) })

	_, err := WorkflowTool{}.Execute(context.Background(), "c", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "script is required") {
		t.Errorf("err = %v, want script-required", err)
	}
}
