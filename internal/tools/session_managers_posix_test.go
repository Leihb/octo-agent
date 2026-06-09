//go:build darwin || linux

package tools

import (
	"context"
	"testing"
	"time"
)

// TestBackgroundManager_CtxScopedIsolatedAndReaped verifies the per-session
// wiring: a terminal command started under a ctx-scoped manager lands in THAT
// manager (not another session's), and KillAllBackground reaps per-session
// managers, not only the global default.
func TestBackgroundManager_CtxScopedIsolatedAndReaped(t *testing.T) {
	mA := SessionBackgroundManager("sess-iso-A")
	mB := SessionBackgroundManager("sess-iso-B")
	defer CloseSessionBackgroundManager("sess-iso-A")
	defer CloseSessionBackgroundManager("sess-iso-B")

	ctx := WithBackgroundManager(context.Background(), mA)
	if _, err := (TerminalTool{}).Execute(ctx, "terminal", map[string]any{
		"command":           "sleep 60",
		"run_in_background": true,
	}); err != nil {
		t.Fatalf("background launch: %v", err)
	}

	// Routed to session A's manager.
	if got := len(mA.ListRunning()); got != 1 {
		t.Fatalf("session A manager should track 1 process, got %d", got)
	}
	// Isolation: session B can't see it.
	if got := len(mB.ListRunning()); got != 0 {
		t.Fatalf("session B manager should see no processes, got %d", got)
	}

	// KillAllBackground must reap per-session managers, not just defaultBg.
	KillAllBackground()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(mA.ListRunning()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("KillAllBackground did not reap the per-session manager's process")
}
