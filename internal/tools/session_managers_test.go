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
