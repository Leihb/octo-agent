package tools

import "testing"

// TestSessionBackgroundManager_Registry covers get-or-create identity and that
// Close drops the entry so a later lookup builds a fresh manager.
func TestSessionBackgroundManager_Registry(t *testing.T) {
	a1 := SessionBackgroundManager("sess-A")
	a2 := SessionBackgroundManager("sess-A")
	b := SessionBackgroundManager("sess-B")
	defer CloseSessionBackgroundManager("sess-B")

	if a1 != a2 {
		t.Error("same id should return the same manager instance")
	}
	if a1 == b {
		t.Error("different ids should return different manager instances")
	}

	CloseSessionBackgroundManager("sess-A")
	if a3 := SessionBackgroundManager("sess-A"); a3 == a1 {
		t.Error("after Close, a fresh manager should be created for the id")
	}
	CloseSessionBackgroundManager("sess-A")
}

// TestSessionSubAgentManager_Registry mirrors the background-manager registry
// contract: get-or-create identity, single spawner construction, and Close
// dropping the entry.
func TestSessionSubAgentManager_Registry(t *testing.T) {
	built := 0
	mk := func() Spawner { built++; return &fakeSpawner{} }

	a1 := SessionSubAgentManager("sub-A", mk)
	a2 := SessionSubAgentManager("sub-A", mk)
	b := SessionSubAgentManager("sub-B", mk)
	defer CloseSessionSubAgentManager("sub-B")

	if a1 != a2 {
		t.Error("same id should return the same manager instance")
	}
	if built != 2 {
		t.Errorf("spawner built %d times, want 2 (once per session)", built)
	}
	if a1 == b {
		t.Error("different ids should get distinct managers")
	}
	if a1.Synchronous() {
		t.Error("session manager should default to async")
	}

	CloseSessionSubAgentManager("sub-A")
	a3 := SessionSubAgentManager("sub-A", mk)
	if a3 == a1 {
		t.Error("Close should drop the entry so the next lookup builds fresh")
	}
}
