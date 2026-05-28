package taskgraph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Executor runs one subtask's description through whatever execution mechanism
// the caller has wired up (in production: M10's launch_agent / tools.Spawner;
// in tests: a stub). Kept as a local interface so internal/taskgraph stays
// free of an internal/tools dependency — cmd/octo adapts the real Spawner
// into this shape via a tiny shim.
type Executor interface {
	// Execute runs one subtask and returns its final reply text. An error
	// signals the subtask failed and lights the stop-on-fail path. The
	// context honors caller-level cancellation; implementations should
	// propagate ctx.Err() promptly.
	Execute(ctx context.Context, description string) (result string, err error)
}

// Scheduler walks a Task's DAG, dispatching ready Subtasks through Executor
// in parallel goroutines, fsync'ing state transitions through Store. One
// Scheduler runs one Task at a time — there is no global queue here. The
// CLI constructs a fresh Scheduler per `octo task run <id>`.
type Scheduler struct {
	store *Store
	exec  Executor
	// stdout receives one progress line per state transition (subtask
	// Running, Done, Failed, Skipped, and the final task-level summary).
	// nil silences progress output — useful in tests that want to assert
	// only on the persisted state.
	stdout io.Writer
}

// NewScheduler wires the pieces together. stdout may be nil (silent).
func NewScheduler(store *Store, exec Executor, stdout io.Writer) *Scheduler {
	return &Scheduler{store: store, exec: exec, stdout: stdout}
}

// Run drives the task forward to a terminal state (done, failed, or
// cancelled). Returns nil on success (TaskDone) and a descriptive error
// for every other terminal state, so a CLI caller can map the return into
// an exit code without re-reading the task from disk.
//
// The loop pattern per iteration:
//
//  1. Reload the task from disk (so a concurrent edit, however unlikely
//     in v1, doesn't race the in-flight batch).
//  2. Find every subtask that's Pending AND has all its BlockedBy Done.
//  3. Mark every ready subtask as Running in one Update (one fsync).
//  4. Spawn N goroutines, one per ready subtask, each calling exec.Execute.
//  5. After wg.Wait, apply ALL results in one Update (one fsync). Each
//     subtask becomes Done (with its result) or Failed (with its error).
//  6. If any subtask failed: in a final Update, mark every remaining
//     Pending subtask as Skipped, advance the task to TaskFailed, exit.
//  7. If no subtask failed and nothing was ready (i.e. step 2 returned
//     empty), advance the task to TaskDone (if every subtask is Done)
//     and exit.
//
// Two fsyncs per batch (mark-running + apply-results), so a kill -9
// strands at most one transition per subtask.
func (s *Scheduler) Run(ctx context.Context, id string) error {
	if s.exec == nil {
		return errors.New("scheduler: Executor is required")
	}
	// Promote the task to Running before the first batch. Rejects tasks
	// that are already finished or were cancelled — the CLI's resume path
	// will explicitly clear cancellation before calling Run.
	t, err := s.store.Update(id, func(t *Task) error {
		switch t.Status {
		case TaskDone:
			return errors.New("task already done")
		case TaskCancelled:
			return errors.New("task was cancelled; resume to retry")
		}
		t.Status = TaskRunning
		return nil
	})
	if err != nil {
		return err
	}
	s.logf("Running task %s (%d subtasks)\n", t.ID, len(t.Subtasks))

	for {
		// Reload to see whatever the previous iteration / external
		// editors may have changed.
		t, err = s.store.Get(id)
		if err != nil {
			return err
		}
		ready := readyIDs(t)
		if len(ready) == 0 {
			// Nothing else to do. Final status depends on what's left.
			return s.finalize(id, t)
		}

		now := time.Now().UTC()
		_, err = s.store.Update(id, func(t *Task) error {
			for _, subID := range ready {
				if sub := t.Find(subID); sub != nil {
					sub.Status = SubtaskRunning
					started := now
					sub.Started = &started
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("scheduler: mark running: %w", err)
		}
		for _, subID := range ready {
			s.logf("▶ #%d running\n", subID)
		}

		results := s.dispatchBatch(ctx, t, ready)

		_, err = s.store.Update(id, func(t *Task) error {
			finished := time.Now().UTC()
			for subID, r := range results {
				sub := t.Find(subID)
				if sub == nil {
					continue
				}
				f := finished
				sub.Finished = &f
				if r.err != nil {
					sub.Status = SubtaskFailed
					sub.Error = r.err.Error()
				} else {
					sub.Status = SubtaskDone
					sub.Result = r.reply
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("scheduler: apply results: %w", err)
		}
		anyFailed := false
		for subID, r := range results {
			if r.err != nil {
				anyFailed = true
				s.logf("✗ #%d failed: %s\n", subID, r.err.Error())
			} else {
				s.logf("✓ #%d done\n", subID)
			}
		}

		// If the user cancelled mid-batch, prefer "task cancelled" over
		// "task failed" even if some subtasks returned ctx errors — those
		// were ctx.Err(), not application failures.
		if ctxErr := ctx.Err(); ctxErr != nil {
			_, _ = s.store.Update(id, func(t *Task) error {
				t.Status = TaskCancelled
				return nil
			})
			return ctxErr
		}

		if anyFailed {
			return s.markFailed(id)
		}
	}
}

// finalize closes out the task once no more subtasks are ready. Picks
// between TaskDone (every subtask reached Done) and TaskFailed (something
// got stranded — shouldn't happen with stop-on-fail wiring but be defensive).
func (s *Scheduler) finalize(id string, t *Task) error {
	allDone := true
	for _, sub := range t.Subtasks {
		if sub.Status != SubtaskDone {
			allDone = false
			break
		}
	}
	final := TaskDone
	if !allDone {
		final = TaskFailed
	}
	_, err := s.store.Update(id, func(t *Task) error {
		t.Status = final
		return nil
	})
	if err != nil {
		return err
	}
	if final == TaskDone {
		s.logf("Task done.\n")
		return nil
	}
	s.logf("Task finalized as failed (some subtasks not Done).\n")
	return fmt.Errorf("task %s finished with non-Done subtasks", id)
}

// markFailed is the stop-on-fail path: every still-Pending subtask becomes
// Skipped (so resume can tell "tried-and-failed" from "didn't try"), and
// the task itself moves to Failed.
func (s *Scheduler) markFailed(id string) error {
	_, err := s.store.Update(id, func(t *Task) error {
		for i := range t.Subtasks {
			if t.Subtasks[i].Status == SubtaskPending {
				t.Subtasks[i].Status = SubtaskSkipped
			}
		}
		t.Status = TaskFailed
		return nil
	})
	if err != nil {
		return err
	}
	s.logf("Task failed (stop-on-fail; remaining subtasks marked skipped).\n")
	return fmt.Errorf("task %s failed", id)
}

// subResult bundles one goroutine's outcome.
type subResult struct {
	reply string
	err   error
}

// dispatchBatch fans out ready subtasks to Executor in parallel and returns
// a map keyed by subtask ID. The caller persists the results in one Update
// for a single fsync per batch.
//
// Subtasks aren't re-shuffled or sorted here — readyIDs already returns
// them in DAG-natural (ID-ascending) order.
func (s *Scheduler) dispatchBatch(ctx context.Context, t *Task, ready []int) map[int]subResult {
	results := make(map[int]subResult, len(ready))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, subID := range ready {
		sub := t.Find(subID)
		if sub == nil {
			continue
		}
		desc := sub.Description
		wg.Add(1)
		go func(id int, d string) {
			defer wg.Done()
			reply, err := s.exec.Execute(ctx, d)
			mu.Lock()
			results[id] = subResult{reply: reply, err: err}
			mu.Unlock()
		}(subID, desc)
	}
	wg.Wait()
	return results
}

// readyIDs walks the task and returns the IDs of every subtask whose
// dependencies are satisfied. Returned in ascending order so progress
// logs stay readable.
func readyIDs(t *Task) []int {
	var ids []int
	for _, sub := range t.Subtasks {
		if sub.Ready(t) {
			ids = append(ids, sub.ID)
		}
	}
	return ids
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.stdout == nil {
		return
	}
	fmt.Fprintf(s.stdout, format, args...)
}
