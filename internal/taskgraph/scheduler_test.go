package taskgraph

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExec runs a function per subtask description. The function decides
// what to return + how long to take. Used to control concurrency / ordering
// / failure-injection without spawning real LLM calls.
type fakeExec struct {
	fn        func(ctx context.Context, description string) (string, error)
	callCount int32
}

func (f *fakeExec) Execute(ctx context.Context, description string) (string, error) {
	atomic.AddInt32(&f.callCount, 1)
	if f.fn == nil {
		return "ok:" + description, nil
	}
	return f.fn(ctx, description)
}

// runWithExec is the standard helper: create a task with the given subtasks
// in a fresh store, run the scheduler with the given fakeExec, return the
// final task state + the scheduler's error.
func runWithExec(t *testing.T, subs []Subtask, exec Executor) (*Task, error, string) {
	t.Helper()
	store := NewStoreAt(t.TempDir())
	created, err := store.Create("test goal", subs)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	sch := NewScheduler(store, exec, &buf)
	runErr := sch.Run(context.Background(), created.ID)
	final, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return final, runErr, buf.String()
}

// ── Happy path ───────────────────────────────────────────────────────────

func TestScheduler_LinearDAGCompletes(t *testing.T) {
	got, err, log := runWithExec(t, []Subtask{
		{ID: 1, Description: "A", Status: SubtaskPending},
		{ID: 2, Description: "B", BlockedBy: []int{1}, Status: SubtaskPending},
		{ID: 3, Description: "C", BlockedBy: []int{2}, Status: SubtaskPending},
	}, &fakeExec{})
	if err != nil {
		t.Fatalf("Run = %v, want nil; log:\n%s", err, log)
	}
	if got.Status != TaskDone {
		t.Errorf("final task status = %q, want %q", got.Status, TaskDone)
	}
	for _, sub := range got.Subtasks {
		if sub.Status != SubtaskDone {
			t.Errorf("subtask #%d status = %q, want Done", sub.ID, sub.Status)
		}
		if sub.Result != "ok:"+sub.Description {
			t.Errorf("subtask #%d result = %q", sub.ID, sub.Result)
		}
		if sub.Started == nil || sub.Finished == nil {
			t.Errorf("subtask #%d missing Started/Finished timestamps", sub.ID)
		}
	}
}

func TestScheduler_SingleSubtaskCompletes(t *testing.T) {
	got, err, _ := runWithExec(t, []Subtask{
		{ID: 1, Description: "only", Status: SubtaskPending},
	}, &fakeExec{})
	if err != nil || got.Status != TaskDone {
		t.Errorf("single-subtask task should succeed: status=%q err=%v", got.Status, err)
	}
}

func TestScheduler_LogsProgress(t *testing.T) {
	_, _, log := runWithExec(t, []Subtask{
		{ID: 1, Description: "A", Status: SubtaskPending},
		{ID: 2, Description: "B", BlockedBy: []int{1}, Status: SubtaskPending},
	}, &fakeExec{})
	for _, want := range []string{"Running task", "▶ #1", "✓ #1", "▶ #2", "✓ #2", "Task done"} {
		if !strings.Contains(log, want) {
			t.Errorf("progress log missing %q:\n%s", want, log)
		}
	}
}

// ── Parallelism ──────────────────────────────────────────────────────────

func TestScheduler_IndependentSubtasksRunConcurrently(t *testing.T) {
	// Three subtasks with no deps; each blocks on a shared barrier so
	// they all have to be in-flight at the same time for the test to pass.
	// If the scheduler dispatched serially, the barrier never opens and
	// the test times out.
	var wg sync.WaitGroup
	wg.Add(3)
	released := make(chan struct{})

	exec := &fakeExec{fn: func(ctx context.Context, _ string) (string, error) {
		wg.Done()  // arrival
		<-released // release only after main test sees all 3 arrived
		return "", nil
	}}

	store := NewStoreAt(t.TempDir())
	created, _ := store.Create("parallel", []Subtask{
		{ID: 1, Description: "A", Status: SubtaskPending},
		{ID: 2, Description: "B", Status: SubtaskPending},
		{ID: 3, Description: "C", Status: SubtaskPending},
	})

	sch := NewScheduler(store, exec, nil)
	done := make(chan error, 1)
	go func() { done <- sch.Run(context.Background(), created.ID) }()

	// Wait for all three to arrive at the barrier.
	arrived := make(chan struct{})
	go func() { wg.Wait(); close(arrived) }()

	select {
	case <-arrived:
		// Good — three goroutines are simultaneously inside Execute.
	case <-time.After(2 * time.Second):
		close(released) // unblock to clean up
		<-done
		t.Fatal("independent subtasks did not run concurrently")
	}
	close(released)
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestScheduler_DepsForceSerial(t *testing.T) {
	// 1 → 2 → 3: each must complete before the next starts. We assert that
	// by recording the relative order of Execute calls.
	var (
		mu    sync.Mutex
		order []string
	)
	exec := &fakeExec{fn: func(ctx context.Context, d string) (string, error) {
		mu.Lock()
		order = append(order, d)
		mu.Unlock()
		return "", nil
	}}

	got, err, _ := runWithExec(t, []Subtask{
		{ID: 1, Description: "first", Status: SubtaskPending},
		{ID: 2, Description: "second", BlockedBy: []int{1}, Status: SubtaskPending},
		{ID: 3, Description: "third", BlockedBy: []int{2}, Status: SubtaskPending},
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDone {
		t.Fatalf("final status = %q", got.Status)
	}
	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Errorf("execution order = %v, want [first second third]", order)
	}
}

// ── Stop on failure ──────────────────────────────────────────────────────

func TestScheduler_FailureStopsAndMarksRemainingSkipped(t *testing.T) {
	exec := &fakeExec{fn: func(_ context.Context, d string) (string, error) {
		if d == "B" {
			return "", errors.New("oops")
		}
		return "ok:" + d, nil
	}}

	got, err, log := runWithExec(t, []Subtask{
		{ID: 1, Description: "A", Status: SubtaskPending},
		{ID: 2, Description: "B", BlockedBy: []int{1}, Status: SubtaskPending},
		{ID: 3, Description: "C", BlockedBy: []int{2}, Status: SubtaskPending},
	}, exec)
	if err == nil {
		t.Fatal("Run should error when a subtask fails")
	}
	if got.Status != TaskFailed {
		t.Errorf("task status after failure = %q, want %q", got.Status, TaskFailed)
	}
	if got.Subtasks[0].Status != SubtaskDone {
		t.Errorf("subtask #1 should be Done: %+v", got.Subtasks[0])
	}
	if got.Subtasks[1].Status != SubtaskFailed {
		t.Errorf("subtask #2 should be Failed: %+v", got.Subtasks[1])
	}
	if got.Subtasks[1].Error != "oops" {
		t.Errorf("subtask #2 error = %q", got.Subtasks[1].Error)
	}
	if got.Subtasks[2].Status != SubtaskSkipped {
		t.Errorf("subtask #3 should be Skipped: %+v", got.Subtasks[2])
	}
	if !strings.Contains(log, "✗ #2") {
		t.Errorf("failure should be logged:\n%s", log)
	}
}

func TestScheduler_ParallelFailureSkipsUnrelatedBranch(t *testing.T) {
	// Two independent branches: 1 and 2 run in parallel. 1 fails. Even
	// though 2 has nothing to do with 1, the stop-on-fail policy marks
	// the whole task failed and any later-depending pending subtasks
	// as skipped. (Here 2 either lands as Done or Failed in the same
	// batch — both outcomes are valid; what matters is the *task*
	// terminates as Failed.)
	exec := &fakeExec{fn: func(_ context.Context, d string) (string, error) {
		if d == "A" {
			return "", errors.New("nope")
		}
		return "", nil
	}}

	got, err, _ := runWithExec(t, []Subtask{
		{ID: 1, Description: "A", Status: SubtaskPending},
		{ID: 2, Description: "B", Status: SubtaskPending},
		{ID: 3, Description: "C", BlockedBy: []int{1}, Status: SubtaskPending}, // would have run after A
	}, exec)
	if err == nil {
		t.Fatal("expected failure error")
	}
	if got.Status != TaskFailed {
		t.Errorf("task status = %q", got.Status)
	}
	if got.Subtasks[0].Status != SubtaskFailed {
		t.Errorf("#1 should be Failed: %q", got.Subtasks[0].Status)
	}
	if got.Subtasks[2].Status != SubtaskSkipped {
		t.Errorf("#3 (depended on #1) should be Skipped, got %q", got.Subtasks[2].Status)
	}
}

// ── Lifecycle guards ─────────────────────────────────────────────────────

func TestScheduler_RejectsAlreadyDoneTask(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	t1, _ := store.Create("g", []Subtask{{ID: 1, Description: "x", Status: SubtaskPending}})
	_, _ = store.Update(t1.ID, func(t *Task) error { t.Status = TaskDone; return nil })

	sch := NewScheduler(store, &fakeExec{}, nil)
	if err := sch.Run(context.Background(), t1.ID); err == nil {
		t.Error("running a Done task should error")
	}
}

func TestScheduler_RejectsCancelledTask(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	t1, _ := store.Create("g", []Subtask{{ID: 1, Description: "x", Status: SubtaskPending}})
	_, _ = store.Update(t1.ID, func(t *Task) error { t.Status = TaskCancelled; return nil })

	sch := NewScheduler(store, &fakeExec{}, nil)
	if err := sch.Run(context.Background(), t1.ID); err == nil {
		t.Error("running a cancelled task should error; resume should explicitly clear cancellation first")
	}
}

func TestScheduler_NilExecutorErrors(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	t1, _ := store.Create("g", []Subtask{{ID: 1, Description: "x", Status: SubtaskPending}})
	sch := NewScheduler(store, nil, nil)
	if err := sch.Run(context.Background(), t1.ID); err == nil {
		t.Error("nil Executor should error before any state changes")
	}
}

// ── Context cancellation ─────────────────────────────────────────────────

func TestScheduler_ContextCancellation(t *testing.T) {
	// Sub-agent blocks until ctx is cancelled, then returns ctx.Err().
	// The scheduler should treat that as cancellation (not a regular
	// subtask failure) and leave the task marked cancelled.
	exec := &fakeExec{fn: func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}

	store := NewStoreAt(t.TempDir())
	created, _ := store.Create("g", []Subtask{
		{ID: 1, Description: "long", Status: SubtaskPending},
	})

	ctx, cancel := context.WithCancel(context.Background())
	sch := NewScheduler(store, exec, nil)
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx, created.ID) }()

	// Cancel after a tick so the sub-agent has started.
	time.Sleep(20 * time.Millisecond)
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run should return context.Canceled, got %v", err)
	}
	final, _ := store.Get(created.ID)
	if final.Status != TaskCancelled {
		t.Errorf("final task status = %q, want %q", final.Status, TaskCancelled)
	}
}

// ── Persistence ──────────────────────────────────────────────────────────

func TestScheduler_PersistsResultsAndErrors(t *testing.T) {
	exec := &fakeExec{fn: func(_ context.Context, d string) (string, error) {
		return "reply for " + d, nil
	}}
	got, err, _ := runWithExec(t, []Subtask{
		{ID: 1, Description: "alpha", Status: SubtaskPending},
		{ID: 2, Description: "beta", BlockedBy: []int{1}, Status: SubtaskPending},
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subtasks[0].Result != "reply for alpha" {
		t.Errorf("alpha result = %q", got.Subtasks[0].Result)
	}
	if got.Subtasks[1].Result != "reply for beta" {
		t.Errorf("beta result = %q", got.Subtasks[1].Result)
	}
}

// ── readyIDs unit ────────────────────────────────────────────────────────

func TestReadyIDs(t *testing.T) {
	// 1 done, 2 ready (deps satisfied), 3 still blocked, 4 already running.
	tk := &Task{Subtasks: []Subtask{
		{ID: 1, Status: SubtaskDone},
		{ID: 2, Status: SubtaskPending, BlockedBy: []int{1}},
		{ID: 3, Status: SubtaskPending, BlockedBy: []int{2}},
		{ID: 4, Status: SubtaskRunning},
	}}
	got := readyIDs(tk)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("readyIDs = %v, want [2]", got)
	}
}
