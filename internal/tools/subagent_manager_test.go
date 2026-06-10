package tools

import (
	"context"
	"sync"
	"testing"
	"time"
)

// collectEvents registers an onEvent hook that appends every event kind to a
// shared slice, returning an accessor for the collected kinds.
func collectEvents(m *SubAgentManager) func() []string {
	var mu sync.Mutex
	var kinds []string
	m.SetOnEvent(func(ev SubAgentEvent) {
		mu.Lock()
		kinds = append(kinds, ev.Kind)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), kinds...)
	}
}

// A synchronous round must bracket its work with started/done events so live
// panels can show and then drop the entry — sync completions never fire the
// onExit hook, so "done" is the only completion signal a panel gets.
func TestRunSync_EmitsStartedAndDone(t *testing.T) {
	m := NewSubAgentManager(&fakeSpawner{})
	kinds := collectEvents(m)

	if _, err := m.RunSync(context.Background(), SpawnRequest{Description: "d", Prompt: "p"}); err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	got := kinds()
	if len(got) != 2 || got[0] != "started" || got[1] != "done" {
		t.Fatalf("events = %v, want [started done]", got)
	}
}

// An async spawn must emit "done" after completion too (alongside the onExit
// notification), so the web live panel clears per-agent.
func TestStart_EmitsDoneOnCompletion(t *testing.T) {
	m := NewSubAgentManager(&fakeSpawner{})
	kinds := collectEvents(m)

	exited := make(chan struct{})
	m.SetOnExit(func(SubAgentNotification) { close(exited) })

	if _, err := m.Start(SpawnRequest{Description: "d", Prompt: "p"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("onExit never fired")
	}
	// "done" is emitted by a deferred call after the hook — give the goroutine
	// a moment to unwind.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := kinds()
		if len(got) >= 2 && got[len(got)-1] == "done" {
			if got[0] != "started" {
				t.Fatalf("events = %v, want started first", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %v, want trailing done", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
