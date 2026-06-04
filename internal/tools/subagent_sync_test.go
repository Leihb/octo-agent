package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tasks"
)

// TestLaunchAgentSyncReturnsReplyDirectly verifies the synchronous launch_agent
// path: with a synchronous manager, Execute blocks on the spawner and returns
// the child's reply as the tool_result (tagged with the agent id), rather than
// the async "Started…" acknowledgement.
func TestLaunchAgentSyncReturnsReplyDirectly(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "child answer", AgentID: "c1"}}}
	mgr := NewSubAgentManager(sp)
	mgr.SetSynchronous(true)

	ctx := WithSubAgentManager(context.Background(), mgr)
	res, err := (LaunchAgentTool{}).Execute(ctx, "launch_agent", map[string]any{
		"description": "do a thing",
		"prompt":      "the task",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(res.Text, "Started") {
		t.Errorf("sync launch should return the reply, not an async ack: %q", res.Text)
	}
	if !strings.Contains(res.Text, "child answer") {
		t.Errorf("expected child reply in result, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "c1") {
		t.Errorf("expected [agent c1] tag in result, got %q", res.Text)
	}
	if len(sp.spawns) != 1 {
		t.Errorf("expected 1 synchronous spawn, got %d", len(sp.spawns))
	}
}

// TestLaunchAgentAsyncUnchanged is the regression guard that the default
// (interactive) path still fires-and-forgets with a "Started…" ack.
func TestLaunchAgentAsyncUnchanged(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "x", AgentID: "c1"}}}
	mgr := NewSubAgentManager(sp) // not synchronous

	ctx := WithSubAgentManager(context.Background(), mgr)
	res, err := (LaunchAgentTool{}).Execute(ctx, "launch_agent", map[string]any{
		"description": "d",
		"prompt":      "p",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "Started") {
		t.Errorf("async launch should return a Started ack, got %q", res.Text)
	}
}

// TestCtxManagerPreferredOverGlobal verifies dispatch resolves the ctx-scoped
// manager ahead of the process-global default — the core of per-session
// isolation on the server / IM bridge.
func TestCtxManagerPreferredOverGlobal(t *testing.T) {
	globalSp := &mockSpawner{replies: []SpawnResult{{Reply: "global", AgentID: "g"}}}
	globalMgr := NewSubAgentManager(globalSp) // async
	SetDefaultSubAgentManager(globalMgr)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })

	ctxSp := &mockSpawner{replies: []SpawnResult{{Reply: "scoped reply", AgentID: "s"}}}
	ctxMgr := NewSubAgentManager(ctxSp)
	ctxMgr.SetSynchronous(true)

	ctx := WithSubAgentManager(context.Background(), ctxMgr)
	res, err := (LaunchAgentTool{}).Execute(ctx, "launch_agent", map[string]any{"prompt": "p"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "scoped reply") {
		t.Errorf("expected the ctx-scoped manager to handle the call, got %q", res.Text)
	}
	if len(ctxSp.spawns) != 1 || len(globalSp.spawns) != 0 {
		t.Errorf("ctx spawner should run (1) and global should not (0); got ctx=%d global=%d",
			len(ctxSp.spawns), len(globalSp.spawns))
	}
}

// TestSendMessageSyncContinues verifies the synchronous send_message path
// continues the child inline and returns its reply.
func TestSendMessageSyncContinues(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "continued", AgentID: "c1"}}}
	mgr := NewSubAgentManager(sp)
	mgr.SetSynchronous(true)

	ctx := WithSubAgentManager(context.Background(), mgr)
	res, err := (SendMessageTool{}).Execute(ctx, "send_message", map[string]any{
		"agent_id": "c1",
		"message":  "keep going",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "continued") {
		t.Errorf("expected continued reply, got %q", res.Text)
	}
	if len(sp.cont) != 1 || sp.cont[0].id != "c1" {
		t.Errorf("expected one Continue on c1, got %+v", sp.cont)
	}
}

// TestTaskStoreCtxScoped verifies task_* tools write to the ctx-scoped store,
// not the process-global one — so concurrent server sessions don't share tasks.
func TestTaskStoreCtxScoped(t *testing.T) {
	global := tasks.New()
	SetTaskStore(global)
	t.Cleanup(func() { SetTaskStore(nil) })

	scoped := tasks.New()
	ctx := WithTaskStore(context.Background(), scoped)

	if _, err := (TaskCreateTool{}).Execute(ctx, "task_create", map[string]any{"subject": "scoped task"}); err != nil {
		t.Fatalf("task_create: %v", err)
	}
	if got := len(scoped.List()); got != 1 {
		t.Errorf("expected the ctx-scoped store to hold the task, got %d", got)
	}
	if got := len(global.List()); got != 0 {
		t.Errorf("expected the global store untouched, got %d", got)
	}
}
