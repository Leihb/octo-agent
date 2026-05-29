package tools

import (
	"strings"
	"testing"
)

func TestBackgroundManager_ListRunning(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start("sleep 2")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := m.ListRunning()
	if len(run) != 1 {
		t.Fatalf("ListRunning = %d entries, want 1: %+v", len(run), run)
	}
	if run[0].ID != id || run[0].Command != "sleep 2" {
		t.Errorf("running entry = %+v, want id=%s command='sleep 2'", run[0], id)
	}
	if run[0].Start.IsZero() {
		t.Error("running entry has no Start time")
	}

	// Killing it makes it drop off the running list once the waiter records exit.
	m.Kill(id)
	waitFor(t, "process to leave the running list", func() bool {
		return len(m.ListRunning()) == 0
	})
}

func TestBackgroundManager_ListRunning_ExcludesExited(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start("echo done")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for it to finish (Read reports exited), then it must not be listed.
	waitFor(t, "process to exit", func() bool {
		_, status, _ := m.Read(id)
		return strings.HasPrefix(status, "exited")
	})
	if run := m.ListRunning(); len(run) != 0 {
		t.Errorf("exited process should not be listed as running; got %+v", run)
	}
}

func TestRunningBackground_DefaultManagerDelegates(t *testing.T) {
	// Smoke: the package-level helper reads the default manager without panicking.
	_ = RunningBackground()
}
