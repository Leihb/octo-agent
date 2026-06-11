package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedSpawner records Continue calls and returns canned replies.
type scriptedSpawner struct {
	mu            sync.Mutex
	continueCalls []string // "id|message"
	continueReply SpawnResult
	continueErr   error
}

func (s *scriptedSpawner) Spawn(_ context.Context, _ SpawnRequest) (SpawnResult, error) {
	return SpawnResult{AgentID: "child_1", Reply: "spawn reply"}, nil
}

func (s *scriptedSpawner) Continue(_ context.Context, id, msg string) (SpawnResult, error) {
	s.mu.Lock()
	s.continueCalls = append(s.continueCalls, id+"|"+msg)
	s.mu.Unlock()
	if s.continueErr != nil {
		return SpawnResult{}, s.continueErr
	}
	return s.continueReply, nil
}

func followupCtx(mgr *SubAgentManager) context.Context {
	return WithSubAgentManager(context.Background(), mgr)
}

func TestFollowupTools_GatedOnManager(t *testing.T) {
	names := []string{"sub_agent_send", "sub_agent_status", "sub_agent_kill"}

	SetDefaultSubAgentManager(nil)
	for _, n := range names {
		if advertisedNames()[n] {
			t.Errorf("%s advertised with no sub-agent manager", n)
		}
	}

	SetDefaultSubAgentManager(NewSubAgentManager(&scriptedSpawner{}))
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })
	for _, n := range names {
		if !advertisedNames()[n] {
			t.Errorf("%s missing with a sub-agent manager registered", n)
		}
	}
}

// TestSend_AsyncAgentRepliesViaNotification: a manager-tracked (async) agent
// gets the message through Send; the reply arrives on the onExit hook.
func TestSend_AsyncAgentRepliesViaNotification(t *testing.T) {
	sp := &scriptedSpawner{continueReply: SpawnResult{Reply: "follow-up answer"}}
	mgr := NewSubAgentManager(sp)

	notes := make(chan SubAgentNotification, 4)
	mgr.SetOnExit(func(n SubAgentNotification) { notes <- n })

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	<-notes // spawn_done

	res, err := AgentSendTool{}.Execute(followupCtx(mgr), "sub_agent_send",
		map[string]any{"agent_id": id, "message": "and one more thing"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "notification") {
		t.Errorf("async send result %q should say the reply arrives as a notification", res.Text)
	}

	select {
	case n := <-notes:
		if n.Kind != "message_reply" || n.Result != "follow-up answer" {
			t.Errorf("notification = %+v, want message_reply with the follow-up answer", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message_reply notification")
	}
}

// TestSend_FallsBackToSyncContinue: an id the manager doesn't know is treated
// as a spawner-side (sync-spawned) child and continued synchronously.
func TestSend_FallsBackToSyncContinue(t *testing.T) {
	sp := &scriptedSpawner{continueReply: SpawnResult{AgentID: "child_1", Reply: "sync continue reply"}}
	mgr := NewSubAgentManager(sp)

	res, err := AgentSendTool{}.Execute(followupCtx(mgr), "sub_agent_send",
		map[string]any{"agent_id": "child_1", "message": "go on"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "sync continue reply") {
		t.Errorf("result %q should carry the continued reply inline", res.Text)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.continueCalls) != 1 || sp.continueCalls[0] != "child_1|go on" {
		t.Errorf("Continue calls = %v", sp.continueCalls)
	}
}

// TestSend_KilledAgentErrors: a killed async agent reports its exit instead
// of being misrouted to the sync-continue fallback.
func TestSend_KilledAgentErrors(t *testing.T) {
	sp := &scriptedSpawner{}
	mgr := NewSubAgentManager(sp)
	notes := make(chan SubAgentNotification, 4)
	mgr.SetOnExit(func(n SubAgentNotification) { notes <- n })

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	<-notes
	mgr.Kill(id)

	_, err = AgentSendTool{}.Execute(followupCtx(mgr), "sub_agent_send",
		map[string]any{"agent_id": id, "message": "hello?"})
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("send to killed agent = %v, want an exited error", err)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.continueCalls) != 0 {
		t.Error("killed agent must not fall through to sync continue")
	}
}

func TestStatus_ByIDAndList(t *testing.T) {
	sp := &scriptedSpawner{}
	mgr := NewSubAgentManager(sp)
	notes := make(chan SubAgentNotification, 4)
	mgr.SetOnExit(func(n SubAgentNotification) { notes <- n })

	id, err := mgr.Start(SpawnRequest{Description: "investigate auth", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	<-notes

	res, err := AgentStatusTool{}.Execute(followupCtx(mgr), "sub_agent_status",
		map[string]any{"agent_id": id})
	if err != nil {
		t.Fatalf("status by id: %v", err)
	}
	if !strings.Contains(res.Text, "spawn reply") {
		t.Errorf("status %q should include the latest result", res.Text)
	}

	res, err = AgentStatusTool{}.Execute(followupCtx(mgr), "sub_agent_status", map[string]any{})
	if err != nil {
		t.Fatalf("status list: %v", err)
	}
	// A completed-but-alive async agent stays listed (idle) — it can still
	// receive sub_agent_send follow-ups.
	if !strings.Contains(res.Text, id) || !strings.Contains(res.Text, "idle") {
		t.Errorf("list %q should show the resumable agent as idle", res.Text)
	}

	// After a kill it leaves the list.
	mgr.Kill(id)
	res, err = AgentStatusTool{}.Execute(followupCtx(mgr), "sub_agent_status", map[string]any{})
	if err != nil {
		t.Fatalf("status list after kill: %v", err)
	}
	if !strings.Contains(res.Text, "No sub-agents") {
		t.Errorf("list %q should be empty after kill", res.Text)
	}

	if _, err := (AgentStatusTool{}).Execute(followupCtx(mgr), "sub_agent_status",
		map[string]any{"agent_id": "agent_999"}); err == nil {
		t.Error("unknown id should error")
	}
}

func TestKill_KnownAndUnknown(t *testing.T) {
	sp := &scriptedSpawner{}
	mgr := NewSubAgentManager(sp)
	notes := make(chan SubAgentNotification, 4)
	mgr.SetOnExit(func(n SubAgentNotification) { notes <- n })

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	<-notes

	res, err := AgentKillTool{}.Execute(followupCtx(mgr), "sub_agent_kill",
		map[string]any{"agent_id": id})
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !strings.Contains(res.Text, id) {
		t.Errorf("kill result %q should name the agent", res.Text)
	}

	if _, err := (AgentKillTool{}).Execute(followupCtx(mgr), "sub_agent_kill",
		map[string]any{"agent_id": "agent_999"}); err == nil {
		t.Error("unknown id should error")
	}
}

func TestFollowupTools_RefusedInsideSubAgent(t *testing.T) {
	mgr := NewSubAgentManager(&scriptedSpawner{})
	ctx := WithSubAgentMarker(followupCtx(mgr))

	if _, err := (AgentSendTool{}).Execute(ctx, "sub_agent_send",
		map[string]any{"agent_id": "x", "message": "m"}); err == nil {
		t.Error("sub_agent_send must be refused inside a sub-agent")
	}
	if _, err := (AgentKillTool{}).Execute(ctx, "sub_agent_kill",
		map[string]any{"agent_id": "x"}); err == nil {
		t.Error("sub_agent_kill must be refused inside a sub-agent")
	}
}
