package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSendMessageTool_Schema(t *testing.T) {
	def := SendMessageTool{}.Definition()
	if def.Name != "send_message" {
		t.Errorf("Name = %q", def.Name)
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, want := range []string{"agent_id", "message"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema missing property %q", want)
		}
	}
	required, _ := def.Parameters["required"].([]string)
	if !containsString(required, "agent_id") || !containsString(required, "message") {
		t.Errorf("agent_id and message should be required, got %v", required)
	}
}

func TestSendMessageTool_AsyncDelivery(t *testing.T) {
	contCalled := make(chan struct{})
	stub := &stubSpawner{
		continueReply: SpawnResult{AgentID: "a1b2c3d4", Reply: "next round done"},
		contCalled:    contCalled,
	}
	mgr := NewSubAgentManager(stub)

	// First launch a sub-agent so it exists.
	_, _ = mgr.Start(SpawnRequest{Description: "test", Prompt: "do it"})
	time.Sleep(50 * time.Millisecond)

	out, err := SendMessageTool{mgr: mgr}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "agent_1",
		"message":  "now do the second half",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Message sent") {
		t.Errorf("Execute returned %q, want 'Message sent' prefix", out.Text)
	}

	// Wait for the async Continue goroutine to reach the spawner.
	select {
	case <-contCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Continue to be called")
	}

	if stub.contAgentID != "agent_1" || stub.contMessage != "now do the second half" {
		t.Errorf("Continue got (%q,%q)", stub.contAgentID, stub.contMessage)
	}
}

func TestSendMessageTool_RequiredArgs(t *testing.T) {
	stub := &stubSpawner{}
	mgr := NewSubAgentManager(stub)
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"no agent_id", map[string]any{"message": "x"}, "agent_id is required"},
		{"no message", map[string]any{"agent_id": "id"}, "message is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SendMessageTool{mgr: mgr}.Execute(context.Background(), "send_message", tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSendMessageTool_RecursionRefused(t *testing.T) {
	stub := &stubSpawner{continueReply: SpawnResult{AgentID: "id", Reply: "would have worked"}}
	mgr := NewSubAgentManager(stub)
	ctx := WithSubAgentMarker(context.Background())
	_, err := SendMessageTool{mgr: mgr}.Execute(ctx, "send_message", map[string]any{
		"agent_id": "id",
		"message":  "wake up",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot message another sub-agent") {
		t.Errorf("expected recursion refusal, got %v", err)
	}
}

func TestSendMessageTool_NoManagerConfigured(t *testing.T) {
	_, err := SendMessageTool{}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "id",
		"message":  "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestSendMessageTool_SendErrorPropagates(t *testing.T) {
	stub := &stubSpawner{continueErr: errors.New("no longer alive")}
	mgr := NewSubAgentManager(stub)
	// Don't create the agent first — Send will fail with "no sub-agent".
	_, err := SendMessageTool{mgr: mgr}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "stale",
		"message":  "x",
	})
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestDefaultTools_SendMessageGatedOnManager(t *testing.T) {
	SetDefaultSubAgentManager(nil)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "send_message" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("send_message should be absent when no SubAgentManager is configured")
	}
	SetDefaultSubAgentManager(NewSubAgentManager(&stubSpawner{}))
	if !has() {
		t.Error("send_message should be present once a SubAgentManager is registered")
	}
}
