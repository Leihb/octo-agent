package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockSpawner is a test double for Spawner.
type mockSpawner struct {
	mu      sync.Mutex
	spawns  []SpawnRequest
	cont    []struct{ id, msg string }
	replies []SpawnResult
	err     error
}

func (m *mockSpawner) Spawn(_ context.Context, req SpawnRequest) (SpawnResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawns = append(m.spawns, req)
	if m.err != nil {
		return SpawnResult{}, m.err
	}
	if len(m.replies) > 0 {
		r := m.replies[0]
		m.replies = m.replies[1:]
		return r, nil
	}
	return SpawnResult{Reply: "mock reply"}, nil
}

func (m *mockSpawner) Continue(_ context.Context, id, msg string) (SpawnResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cont = append(m.cont, struct{ id, msg string }{id, msg})
	if m.err != nil {
		return SpawnResult{}, m.err
	}
	if len(m.replies) > 0 {
		r := m.replies[0]
		m.replies = m.replies[1:]
		return r, nil
	}
	return SpawnResult{Reply: "mock continue reply"}, nil
}

func TestSubAgentManagerStart(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "hello", AgentID: "a1"}}}
	mgr := NewSubAgentManager(sp)

	id, err := mgr.Start(SpawnRequest{Description: "test", Prompt: "do it"})
	if err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if !strings.HasPrefix(id, "agent_") {
		t.Fatalf("unexpected id: %s", id)
	}

	// Should be running immediately.
	info := mgr.ListRunning()
	if len(info) != 1 || info[0].ID != id {
		t.Fatalf("expected 1 running agent, got %+v", info)
	}
}

func TestSubAgentManagerNotification(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "result", InputTokens: 10, OutputTokens: 5}}}
	mgr := NewSubAgentManager(sp)

	var notif SubAgentNotification
	done := make(chan struct{})
	mgr.SetOnExit(func(ev SubAgentNotification) {
		notif = ev
		close(done)
	})

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}

	if notif.AgentID != id {
		t.Fatalf("AgentID = %q, want %q", notif.AgentID, id)
	}
	if notif.Kind != "spawn_done" {
		t.Fatalf("Kind = %q, want spawn_done", notif.Kind)
	}
	if notif.Result != "result" {
		t.Fatalf("Result = %q, want result", notif.Result)
	}
	if notif.InputTokens != 10 {
		t.Fatalf("InputTokens = %d, want 10", notif.InputTokens)
	}
	if notif.OutputTokens != 5 {
		t.Fatalf("OutputTokens = %d, want 5", notif.OutputTokens)
	}
}

func TestSubAgentManagerSend(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{
		{Reply: "spawned"},
		{Reply: "replied"},
	}}
	mgr := NewSubAgentManager(sp)

	var notifs []SubAgentNotification
	var mu sync.Mutex
	mgr.SetOnExit(func(ev SubAgentNotification) {
		mu.Lock()
		defer mu.Unlock()
		notifs = append(notifs, ev)
	})

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for spawn to finish.
	time.Sleep(100 * time.Millisecond)

	// Send a message.
	if err := mgr.Send(id, "follow up"); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	// Wait for reply notification.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if len(notifs) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(notifs))
	}
	if notifs[1].Kind != "message_reply" {
		t.Fatalf("second Kind = %q, want message_reply", notifs[1].Kind)
	}
	if notifs[1].Result != "replied" {
		t.Fatalf("second Result = %q, want replied", notifs[1].Result)
	}
	mu.Unlock()
}

func TestSubAgentManagerSendQueue(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{
		{Reply: "first"},
		{Reply: "second"},
	}}
	mgr := NewSubAgentManager(sp)

	var notifs []SubAgentNotification
	var mu sync.Mutex
	mgr.SetOnExit(func(ev SubAgentNotification) {
		mu.Lock()
		defer mu.Unlock()
		notifs = append(notifs, ev)
	})

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for spawn to finish first.
	time.Sleep(100 * time.Millisecond)

	// Send first message.
	if err := mgr.Send(id, "msg1"); err != nil {
		t.Fatalf("Send msg1 error: %v", err)
	}
	// Second should be queued.
	if err := mgr.Send(id, "msg2"); err != nil {
		t.Fatalf("Send msg2 error: %v", err)
	}
	// Third should fail (queue full).
	if err := mgr.Send(id, "msg3"); err == nil {
		t.Fatal("expected error for third Send, got nil")
	}

	// Wait for all to complete.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	if len(notifs) != 3 {
		t.Fatalf("expected 3 notifications, got %d", len(notifs))
	}
	mu.Unlock()
}

func TestSubAgentManagerKill(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "never"}}}
	mgr := NewSubAgentManager(sp)

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}

	if !mgr.Kill(id) {
		t.Fatal("Kill returned false")
	}

	// Wait for cancellation to propagate.
	time.Sleep(100 * time.Millisecond)

	_, status, _ := mgr.Read(id)
	if !strings.Contains(status, "exited") {
		t.Fatalf("expected exited status, got %q", status)
	}
}

func TestSubAgentManagerRead(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "the result"}}}
	mgr := NewSubAgentManager(sp)

	id, err := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for completion.
	time.Sleep(100 * time.Millisecond)

	result, status, found := mgr.Read(id)
	if !found {
		t.Fatal("expected found")
	}
	if result != "the result" {
		t.Fatalf("result = %q, want the result", result)
	}
	if status != "idle" {
		t.Fatalf("expected idle status, got %q", status)
	}
}

func TestSubAgentManagerReadUnknown(t *testing.T) {
	mgr := NewSubAgentManager(&mockSpawner{})
	_, _, found := mgr.Read("agent_999")
	if found {
		t.Fatal("expected not found")
	}
}

func TestSubAgentManagerSendToUnknown(t *testing.T) {
	mgr := NewSubAgentManager(&mockSpawner{})
	if err := mgr.Send("agent_999", "hi"); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestSubAgentManagerSendToExited(t *testing.T) {
	sp := &mockSpawner{replies: []SpawnResult{{Reply: "done"}}}
	mgr := NewSubAgentManager(sp)

	id, _ := mgr.Start(SpawnRequest{Description: "d", Prompt: "p"})
	mgr.Kill(id)
	time.Sleep(100 * time.Millisecond)

	if err := mgr.Send(id, "too late"); err == nil {
		t.Fatal("expected error for exited agent")
	}
}
