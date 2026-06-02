package agent

import (
	"testing"
)

func TestInbox_EnqueueDrain(t *testing.T) {
	ib := &Inbox{}

	if ib.HasPending() {
		t.Fatal("fresh inbox should have no pending messages")
	}
	if got := ib.Drain(); got != nil {
		t.Errorf("Drain on empty = %v, want nil", got)
	}

	// Whitespace-only messages are ignored.
	ib.Enqueue("   ")
	ib.Enqueue("\n\t")
	if ib.HasPending() {
		t.Fatal("whitespace-only messages should be ignored")
	}

	ib.Enqueue("first")
	ib.Enqueue("second")
	if !ib.HasPending() {
		t.Fatal("expected pending messages after two enqueues")
	}

	msgs := ib.Drain()
	if len(msgs) != 2 {
		t.Fatalf("Drain len = %d, want 2", len(msgs))
	}
	if msgs[0] != "first" || msgs[1] != "second" {
		t.Errorf("Drain = %v, want [first second]", msgs)
	}

	// Drain must clear.
	if ib.HasPending() || ib.Drain() != nil {
		t.Error("Drain did not clear the inbox")
	}
}

func TestInbox_Remove(t *testing.T) {
	var ib Inbox
	ib.Enqueue("a")
	ib.Enqueue("b")
	ib.Enqueue("a") // duplicate

	// Removes the LAST matching entry.
	if !ib.Remove("a") {
		t.Fatal("Remove(a) = false, want true")
	}
	if got := ib.Drain(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("after Remove(a) drain = %v, want [a b]", got)
	}
}

func TestInbox_Remove_NotFound(t *testing.T) {
	var ib Inbox
	ib.Enqueue("x")
	if ib.Remove("nope") {
		t.Error("Remove of absent message returned true")
	}
	if !ib.HasPending() {
		t.Error("Remove of absent message should leave the queue intact")
	}
}
