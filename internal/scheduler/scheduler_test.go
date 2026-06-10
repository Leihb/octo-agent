package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// recordRunner records every RunTask call for assertions.
type recordRunner struct {
	mu    sync.Mutex
	tasks []Task
}

func (r *recordRunner) RunTask(_ context.Context, t Task) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, t)
	return "sess_" + t.ID, nil
}

func (r *recordRunner) calls() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Task(nil), r.tasks...)
}

func newTestScheduler(t *testing.T) (*Scheduler, *recordRunner) {
	t.Helper()
	r := &recordRunner{}
	s, err := New(t.TempDir(), r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, r
}

func (s *Scheduler) entryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func TestAddRegistersCronEntry(t *testing.T) {
	s, _ := newTestScheduler(t)
	if err := s.Add(Task{ID: "t1", Name: "n", Cron: "0 0 9 * * *", Prompt: "p", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := len(s.cron.Entries()); got != 1 {
		t.Fatalf("cron entries = %d, want 1", got)
	}
	if got := s.entryCount(); got != 1 {
		t.Fatalf("tracked entries = %d, want 1", got)
	}
}

func TestAddRejectsInvalidCron(t *testing.T) {
	s, _ := newTestScheduler(t)
	// A standard 5-field crontab line is invalid (seconds field required) —
	// it must be rejected even for a disabled task, so a later enable doesn't
	// surface a parse error long after the user typed the expression.
	err := s.Add(Task{ID: "t1", Name: "n", Cron: "0 9 * * *", Prompt: "p", Enabled: false})
	if err == nil || !strings.Contains(err.Error(), "invalid cron expression") {
		t.Fatalf("Add with 5-field cron: got %v, want invalid-expression error", err)
	}
	if got := len(s.tasks); got != 0 {
		t.Fatalf("tasks stored = %d, want 0", got)
	}
}

func TestUpdateReschedulesEntry(t *testing.T) {
	s, _ := newTestScheduler(t)
	task := Task{ID: "t1", Name: "n", Cron: "0 0 9 * * *", Prompt: "p", Enabled: true}
	if err := s.Add(task); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.mu.Lock()
	oldEntry := s.entries["t1"]
	s.mu.Unlock()

	task.Cron = "0 30 18 * * 1-5"
	if err := s.Update(task); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := len(s.cron.Entries()); got != 1 {
		t.Fatalf("cron entries after update = %d, want 1", got)
	}
	s.mu.Lock()
	newEntry := s.entries["t1"]
	s.mu.Unlock()
	if newEntry == oldEntry {
		t.Fatal("expected a fresh cron entry after cron expression change")
	}
	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Cron != "0 30 18 * * 1-5" {
		t.Fatalf("stored cron = %q, want updated expression", got.Cron)
	}
}

func TestUpdateRejectsInvalidCron(t *testing.T) {
	s, _ := newTestScheduler(t)
	task := Task{ID: "t1", Name: "n", Cron: "0 0 9 * * *", Prompt: "p", Enabled: true}
	if err := s.Add(task); err != nil {
		t.Fatalf("Add: %v", err)
	}
	task.Cron = "not a cron"
	if err := s.Update(task); err == nil {
		t.Fatal("Update with bad cron: want error, got nil")
	}
	// The original schedule must survive a rejected update.
	if got := len(s.cron.Entries()); got != 1 {
		t.Fatalf("cron entries = %d, want 1", got)
	}
	got, _ := s.Get("t1")
	if got.Cron != "0 0 9 * * *" {
		t.Fatalf("stored cron = %q, want original expression", got.Cron)
	}
}

func TestDisableRemovesEntryAndStopsRuns(t *testing.T) {
	s, r := newTestScheduler(t)
	task := Task{ID: "t1", Name: "n", Cron: "0 0 9 * * *", Prompt: "p", Enabled: true}
	if err := s.Add(task); err != nil {
		t.Fatalf("Add: %v", err)
	}
	fire := s.wrap("t1") // simulate the registered cron callback

	task.Enabled = false
	if err := s.Update(task); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := len(s.cron.Entries()); got != 0 {
		t.Fatalf("cron entries after disable = %d, want 0", got)
	}
	// Even a stale callback from before the disable must not run the task.
	fire()
	if got := len(r.calls()); got != 0 {
		t.Fatalf("runner calls after disable = %d, want 0", got)
	}

	// Re-enabling restores the schedule without a restart.
	task.Enabled = true
	if err := s.Update(task); err != nil {
		t.Fatalf("Update re-enable: %v", err)
	}
	if got := len(s.cron.Entries()); got != 1 {
		t.Fatalf("cron entries after re-enable = %d, want 1", got)
	}
}

func TestDeleteRemovesEntryAndStopsRuns(t *testing.T) {
	s, r := newTestScheduler(t)
	if err := s.Add(Task{ID: "t1", Name: "n", Cron: "0 0 9 * * *", Prompt: "p", Enabled: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	fire := s.wrap("t1")

	if err := s.Delete("t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := len(s.cron.Entries()); got != 0 {
		t.Fatalf("cron entries after delete = %d, want 0", got)
	}
	fire()
	if got := len(r.calls()); got != 0 {
		t.Fatalf("runner calls after delete = %d, want 0", got)
	}
}

func TestFireUsesCurrentTaskState(t *testing.T) {
	s, r := newTestScheduler(t)
	task := Task{ID: "t1", Name: "n", Cron: "0 0 9 * * *", Prompt: "old prompt", Enabled: true}
	if err := s.Add(task); err != nil {
		t.Fatalf("Add: %v", err)
	}
	fire := s.wrap("t1")

	task.Prompt = "new prompt"
	if err := s.Update(task); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fire()
	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(calls))
	}
	if calls[0].Prompt != "new prompt" {
		t.Fatalf("fired prompt = %q, want the updated prompt", calls[0].Prompt)
	}
	got, _ := s.Get("t1")
	if got.LastRun.IsZero() || got.SessionID != "sess_t1" {
		t.Fatalf("run bookkeeping not recorded: lastRun=%v session=%q", got.LastRun, got.SessionID)
	}
}

func TestLoadAllRegistersEnabledTasks(t *testing.T) {
	dir := t.TempDir()
	r := &recordRunner{}
	s, err := New(dir, r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Add(Task{ID: "t1", Name: "a", Cron: "0 0 9 * * *", Prompt: "p", Enabled: true}); err != nil {
		t.Fatalf("Add enabled: %v", err)
	}
	if err := s.Add(Task{ID: "t2", Name: "b", Cron: "@daily", Prompt: "p", Enabled: false}); err != nil {
		t.Fatalf("Add disabled: %v", err)
	}

	// A fresh scheduler over the same dir mirrors a serve restart.
	s2, err := New(dir, r)
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if got := len(s2.List()); got != 2 {
		t.Fatalf("loaded tasks = %d, want 2", got)
	}
	if got := len(s2.cron.Entries()); got != 1 {
		t.Fatalf("cron entries after reload = %d, want 1 (only the enabled task)", got)
	}
}
