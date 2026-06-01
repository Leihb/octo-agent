package main

import (
	"context"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// TestVerify_SendMessageReallyResumes drives the exact production tool surface
// the LLM triggers — LaunchAgentTool.Execute then SendMessageTool.Execute,
// wired through the default SubAgentManager + real agentSpawner exactly as the
// REPL wires them — and confirms send_message resumes the same child rather
// than reporting "no longer alive".
func TestVerify_SendMessageReallyResumes(t *testing.T) {
	send := &subAgentSender{reply: "first-task done", inputTokens: 100, outputTokens: 40}
	parent := agent.New(send, "parent-model")
	parent.System = "PARENT"

	// Real spawner with the real childRegistry underneath.
	sp := newAgentSpawner(parent, nilExecutor{}, func() []agent.ToolDefinition { return nil })

	// Wire the manager exactly like cmd/octo/tuirepl.go does in production:
	// SetDefaultSubAgentManager so the bare tools resolve it via t.manager().
	mgr := tools.NewSubAgentManager(sp)
	tools.SetDefaultSubAgentManager(mgr)
	t.Cleanup(func() { tools.SetDefaultSubAgentManager(nil) })

	notes := make(chan tools.SubAgentNotification, 4)
	mgr.SetOnExit(func(ev tools.SubAgentNotification) { notes <- ev })

	waitNote := func(kind string) tools.SubAgentNotification {
		t.Helper()
		select {
		case ev := <-notes:
			if ev.Kind != kind {
				t.Fatalf("notification kind = %q, want %q (result %q)", ev.Kind, kind, ev.Result)
			}
			return ev
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for %q notification", kind)
			return tools.SubAgentNotification{}
		}
	}

	ctx := context.Background()

	// 1. The model calls launch_agent.
	out, err := (tools.LaunchAgentTool{}).Execute(ctx, "launch_agent", map[string]any{
		"description": "research the cache module",
		"prompt":      "Summarise the cache module.",
	})
	if err != nil {
		t.Fatalf("launch_agent.Execute: %v", err)
	}
	t.Logf("launch_agent → %q", out.Text)

	// 2. The spawn_done notification is what the model actually sees; it carries
	//    the handle (agent_N) the model will address in send_message.
	spawn := waitNote("spawn_done")
	handle := spawn.AgentID
	t.Logf("spawn_done notification: agent=%s result=%q", handle, spawn.Result)
	if spawn.Result != "first-task done" {
		t.Fatalf("spawn_done result = %q", spawn.Result)
	}

	// 3. The model calls send_message with that handle.
	send.reply = "follow-up done"
	out, err = (tools.SendMessageTool{}).Execute(ctx, "send_message", map[string]any{
		"agent_id": handle,
		"message":  "Now list the eviction policies.",
	})
	if err != nil {
		t.Fatalf("send_message.Execute: %v", err)
	}
	t.Logf("send_message → %q", out.Text)

	// 4. The reply must come from the SAME resumed child, not an error.
	reply := waitNote("message_reply")
	t.Logf("message_reply notification: agent=%s result=%q", reply.AgentID, reply.Result)
	if reply.Result != "follow-up done" {
		t.Fatalf("send_message did NOT resume the child — got %q", reply.Result)
	}
	// History carried over: [user1, assistant1, user2] = 3 messages on the
	// continuation turn proves it's the same child, not a fresh one.
	if got := len(send.lastMessages); got != 3 {
		t.Fatalf("continuation child saw %d messages, want 3 (same child resumed)", got)
	}

	t.Logf("✅ send_message resumed sub-agent %s with full history carried over", handle)
}
