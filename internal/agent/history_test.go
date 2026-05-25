package agent

import (
	"sync"
	"testing"
)

func TestHistory_AppendAndSnapshot(t *testing.T) {
	h := NewHistory()
	if h.Len() != 0 {
		t.Fatalf("fresh history Len = %d, want 0", h.Len())
	}

	h.Append(NewUserMessage("hi"))
	h.Append(NewAssistantMessage("hello"))

	if h.Len() != 2 {
		t.Fatalf("Len after 2 appends = %d, want 2", h.Len())
	}

	snap := h.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Role != RoleUser || snap[1].Role != RoleAssistant {
		t.Errorf("Snapshot roles = [%q, %q], want [user, assistant]", snap[0].Role, snap[1].Role)
	}

	// Snapshot must be a copy — mutating it does not change History.
	snap[0].Content = "MUTATED"
	if h.Snapshot()[0].Content == "MUTATED" {
		t.Errorf("Snapshot leaked mutability back into History")
	}
}

func TestHistory_Reset(t *testing.T) {
	h := NewHistory()
	h.Append(NewUserMessage("hi"))
	h.Reset()
	if h.Len() != 0 {
		t.Errorf("Len after Reset = %d, want 0", h.Len())
	}
}

func TestHistory_ConcurrentAppend(t *testing.T) {
	h := NewHistory()
	const writers = 50
	const perWriter = 20

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				h.Append(NewUserMessage("x"))
			}
		}()
	}
	wg.Wait()

	if h.Len() != writers*perWriter {
		t.Errorf("Len after concurrent appends = %d, want %d", h.Len(), writers*perWriter)
	}
}
