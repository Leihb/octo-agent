package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestSendMessageTool_ContinuesAndTagsReply(t *testing.T) {
	stub := &stubSpawner{continueReply: SpawnResult{AgentID: "a1b2c3d4", Reply: "next round done"}}
	useSpawner(t, stub)

	out, err := SendMessageTool{}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "a1b2c3d4",
		"message":  "now do the second half",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "[agent a1b2c3d4] next round done" {
		t.Errorf("Execute returned %q, want tagged reply", out.Text)
	}
	if stub.contAgentID != "a1b2c3d4" || stub.contMessage != "now do the second half" {
		t.Errorf("Continue got (%q,%q)", stub.contAgentID, stub.contMessage)
	}
}

func TestSendMessageTool_EmptyReplySurfaced(t *testing.T) {
	useSpawner(t, &stubSpawner{continueReply: SpawnResult{AgentID: "id", Reply: "  "}})
	out, err := SendMessageTool{}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "id",
		"message":  "ping",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "produced no reply") {
		t.Errorf("empty reply should be surfaced, got %q", out.Text)
	}
}

func TestSendMessageTool_RequiredArgs(t *testing.T) {
	useSpawner(t, &stubSpawner{})
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
			_, err := SendMessageTool{}.Execute(context.Background(), "send_message", tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSendMessageTool_RecursionRefused(t *testing.T) {
	useSpawner(t, &stubSpawner{continueReply: SpawnResult{AgentID: "id", Reply: "would have worked"}})
	ctx := WithSubAgentMarker(context.Background())
	_, err := SendMessageTool{}.Execute(ctx, "send_message", map[string]any{
		"agent_id": "id",
		"message":  "wake up",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot message another sub-agent") {
		t.Errorf("expected recursion refusal, got %v", err)
	}
}

func TestSendMessageTool_NoSpawnerConfigured(t *testing.T) {
	SetSpawner(nil)
	_, err := SendMessageTool{}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "id",
		"message":  "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestSendMessageTool_ContinueErrorPropagates(t *testing.T) {
	useSpawner(t, &stubSpawner{continueErr: errors.New("no longer alive")})
	_, err := SendMessageTool{}.Execute(context.Background(), "send_message", map[string]any{
		"agent_id": "stale",
		"message":  "x",
	})
	if err == nil || !strings.Contains(err.Error(), "no longer alive") {
		t.Errorf("expected propagated continue error, got %v", err)
	}
}

func TestDefaultTools_SendMessageGatedOnSpawner(t *testing.T) {
	SetSpawner(nil)
	t.Cleanup(func() { SetSpawner(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "send_message" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("send_message should be absent when no spawner is configured")
	}
	useSpawner(t, &stubSpawner{})
	if !has() {
		t.Error("send_message should be present once a spawner is registered")
	}
}

func TestLaunchAgentTool_TagsReplyWithAgentID(t *testing.T) {
	useSpawner(t, &stubSpawner{replies: []SpawnResult{{AgentID: "deadbeef", Reply: "the finding"}}})
	out, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "Investigate",
		"prompt":      "look into X",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "[agent deadbeef] the finding" {
		t.Errorf("launch_agent reply should carry the agent tag, got %q", out.Text)
	}
}
