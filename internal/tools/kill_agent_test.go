package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestKillAgentTool_Schema(t *testing.T) {
	def := KillAgentTool{}.Definition()
	if def.Name != "kill_agent" {
		t.Errorf("Name = %q", def.Name)
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	if _, ok := props["agent_id"]; !ok {
		t.Error("schema missing property agent_id")
	}
	required, _ := def.Parameters["required"].([]string)
	if !containsString(required, "agent_id") {
		t.Errorf("agent_id should be required, got %v", required)
	}
}

func TestKillAgentTool_Execute(t *testing.T) {
	stub := &stubSpawner{replies: []SpawnResult{{Reply: "never"}}}
	mgr := NewSubAgentManager(stub)

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	out, err := KillAgentTool{mgr: mgr}.Execute(context.Background(), "kill_agent", map[string]any{
		"agent_id": id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "terminated") {
		t.Errorf("expected termination message, got %q", out.Text)
	}

	// Verify it's actually exited.
	_, status, _ := mgr.Read(id)
	if !strings.Contains(status, "exited") {
		t.Errorf("expected exited status, got %q", status)
	}
}

func TestKillAgentTool_UnknownAgent(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)

	_, err := KillAgentTool{mgr: mgr}.Execute(context.Background(), "kill_agent", map[string]any{
		"agent_id": "agent_999",
	})
	if err == nil || !strings.Contains(err.Error(), "no sub-agent") {
		t.Errorf("expected 'no sub-agent' error, got %v", err)
	}
}

func TestKillAgentTool_RequiredArgs(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)
	_, err := KillAgentTool{mgr: mgr}.Execute(context.Background(), "kill_agent", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "agent_id is required") {
		t.Errorf("expected 'agent_id is required' error, got %v", err)
	}
}

func TestKillAgentTool_NoManagerConfigured(t *testing.T) {
	_, err := KillAgentTool{}.Execute(context.Background(), "kill_agent", map[string]any{
		"agent_id": "id",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestKillAgentTool_RecursionRefused(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)
	ctx := WithSubAgentMarker(context.Background())
	_, err := KillAgentTool{mgr: mgr}.Execute(ctx, "kill_agent", map[string]any{
		"agent_id": "id",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot kill another sub-agent") {
		t.Errorf("expected recursion refusal, got %v", err)
	}
}

func TestDefaultTools_KillAgentGatedOnManager(t *testing.T) {
	SetDefaultSubAgentManager(nil)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "kill_agent" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("kill_agent should be absent when no SubAgentManager is configured")
	}
	SetDefaultSubAgentManager(NewSubAgentManager(&stubSpawner{}))
	if !has() {
		t.Error("kill_agent should be present once a SubAgentManager is registered")
	}
}
