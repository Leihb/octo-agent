//go:build darwin || linux

package tools

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startWithBackgroundChild launches a tracked process whose wrapping shell
// backgrounds a `sleep` and echoes that child's PID ($!), then waits. The
// returned PID is the grandchild that gets orphaned if only the shell wrapper
// (the direct child / group leader) is SIGKILLed instead of the whole group.
func startWithBackgroundChild(t *testing.T, mgr *BackgroundManager) (id string, childPID int) {
	t.Helper()
	id, err := mgr.Start("sleep 60 & echo $! ; wait")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 50 && childPID == 0; i++ {
		out, _, _, _ := mgr.Read(id)
		if fields := strings.Fields(strings.TrimSpace(out)); len(fields) > 0 {
			if pid, perr := strconv.Atoi(fields[0]); perr == nil {
				childPID = pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("did not capture background child PID from shell output")
	}
	// Sanity: the child is alive (signal 0 = existence check).
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child %d should be alive: %v", childPID, err)
	}
	return id, childPID
}

// assertReaped polls until the child PID is gone (ESRCH), failing if it
// survives — i.e. was orphaned rather than taken down with its process group.
func assertReaped(t *testing.T, childPID int, after string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(childPID, syscall.SIGKILL) // don't leak on failure
	t.Fatalf("background child %d survived %s (orphaned)", childPID, after)
}

// TestKillAll_KillsOrphanedChildren is a regression test for background
// processes surviving session shutdown (TUI exit). KillAll must take the whole
// process group down, not just SIGKILL the wrapper and leave the child
// reparented to init and alive.
func TestKillAll_KillsOrphanedChildren(t *testing.T) {
	mgr := NewBackgroundManager()
	_, childPID := startWithBackgroundChild(t, mgr)
	mgr.KillAll()
	assertReaped(t, childPID, "KillAll")
}

// TestRemove_KillsOrphanedChildren guards the belt-and-suspenders path in
// Remove: if it's ever called on a still-running id, the process (and its
// children) must not outlive the map entry. Cancelling the context alone would
// orphan the grandchild, so Remove also group-kills.
func TestRemove_KillsOrphanedChildren(t *testing.T) {
	mgr := NewBackgroundManager()
	id, childPID := startWithBackgroundChild(t, mgr)
	mgr.Remove(id)
	assertReaped(t, childPID, "Remove")
}
