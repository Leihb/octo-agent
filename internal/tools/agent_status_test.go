package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAgentStatusTool_Schema(t *testing.T) {
	def := AgentStatusTool{}.Definition()
	if def.Name != "agent_status" {
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

func TestAgentStatusTool_Execute(t *testing.T) {
	stub := &stubSpawner{replies: []SpawnResult{{Reply: "the result", AgentID: "a1"}}}
	mgr := NewSubAgentManager(stub)

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	out, err := AgentStatusTool{mgr: mgr}.Execute(context.Background(), "agent_status", map[string]any{
		"agent_id": id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Status: idle") {
		t.Errorf("expected idle status, got %q", out.Text)
	}
	if !strings.Contains(out.Text, "the result") {
		t.Errorf("expected result text, got %q", out.Text)
	}
}

func TestAgentStatusTool_UnknownAgent(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)

	_, err := AgentStatusTool{mgr: mgr}.Execute(context.Background(), "agent_status", map[string]any{
		"agent_id": "agent_999",
	})
	if err == nil || !strings.Contains(err.Error(), "no sub-agent") {
		t.Errorf("expected 'no sub-agent' error, got %v", err)
	}
}

func TestAgentStatusTool_RequiredArgs(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)
	_, err := AgentStatusTool{mgr: mgr}.Execute(context.Background(), "agent_status", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "agent_id is required") {
		t.Errorf("expected 'agent_id is required' error, got %v", err)
	}
}

func TestAgentStatusTool_NoManagerConfigured(t *testing.T) {
	_, err := AgentStatusTool{}.Execute(context.Background(), "agent_status", map[string]any{
		"agent_id": "id",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestAgentStatusTool_RecursionRefused(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)
	ctx := WithSubAgentMarker(context.Background())
	_, err := AgentStatusTool{mgr: mgr}.Execute(ctx, "agent_status", map[string]any{
		"agent_id": "id",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot query another sub-agent") {
		t.Errorf("expected recursion refusal, got %v", err)
	}
}

func TestDefaultTools_AgentStatusGatedOnManager(t *testing.T) {
	SetDefaultSubAgentManager(nil)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "agent_status" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("agent_status should be absent when no SubAgentManager is configured")
	}
	SetDefaultSubAgentManager(NewSubAgentManager(&stubSpawner{}))
	if !has() {
		t.Error("agent_status should be present once a SubAgentManager is registered")
	}
}
