package conductor

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── test doubles ──

// fnWorker is a programmable Worker. fn is called with the unit id, the 0-based
// call index for that unit (0 = first Run, 1+ = Continue rounds), and whether
// this was a resume. It returns the WorkResult to hand back. Prompts are
// recorded for assertions. The handle returned to the conductor encodes the
// unit id ("h<id>") so Continue can route back to the same fn.
type fnWorker struct {
	mu      sync.Mutex
	calls   map[int]int
	prompts map[int]string
	fn      func(unitID, call int, resume bool, spec WorkSpec) WorkResult
}

func newFnWorker(fn func(unitID, call int, resume bool, spec WorkSpec) WorkResult) *fnWorker {
	return &fnWorker{calls: map[int]int{}, prompts: map[int]string{}, fn: fn}
}

func (w *fnWorker) Run(_ context.Context, spec WorkSpec) (WorkResult, error) {
	id := unitIDFromLabel(spec.Label)
	w.mu.Lock()
	c := w.calls[id]
	w.calls[id]++
	w.prompts[id] = spec.Prompt
	w.mu.Unlock()
	res := w.fn(id, c, false, spec)
	if res.Handle == "" {
		res.Handle = "h" + strconv.Itoa(id)
	}
	return res, nil
}

func (w *fnWorker) Continue(_ context.Context, handle, _ string) (WorkResult, error) {
	id, _ := strconv.Atoi(strings.TrimPrefix(handle, "h"))
	w.mu.Lock()
	c := w.calls[id]
	w.calls[id]++
	w.mu.Unlock()
	res := w.fn(id, c, true, WorkSpec{})
	if res.Handle == "" {
		res.Handle = handle
	}
	return res, nil
}

func (w *fnWorker) callCount(id int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[id]
}

func (w *fnWorker) prompt(id int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prompts[id]
}

func unitIDFromLabel(label string) int {
	// label is "#N description"
	label = strings.TrimPrefix(strings.TrimSpace(label), "#")
	end := strings.IndexByte(label, ' ')
	if end < 0 {
		end = len(label)
	}
	n, _ := strconv.Atoi(label[:end])
	return n
}

// fnVerifier is a programmable Verifier keyed by call count.
type fnVerifier struct {
	mu sync.Mutex
	n  int
	fn func(call int, target VerifyTarget) Verdict
}

func (v *fnVerifier) Verify(_ context.Context, target VerifyTarget) (Verdict, error) {
	v.mu.Lock()
	c := v.n
	v.n++
	v.mu.Unlock()
	return v.fn(c, target), nil
}

func alwaysGreen() *fnVerifier {
	return &fnVerifier{fn: func(int, VerifyTarget) Verdict { return Verdict{Green: true} }}
}

// helpers

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStoreAt(t.TempDir())
}

func seed(t *testing.T, s *Store, goal string, units []Unit) string {
	t.Helper()
	l, err := s.Create(goal, units)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return l.ID
}

// ── tests ──

func TestConductHappyPath(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "do the thing", []Unit{
		{ID: 1, Description: "first"},
		{ID: 2, Description: "second", BlockedBy: []int{1}},
	})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		return WorkResult{Reply: "did #" + strconv.Itoa(unitID)}
	})
	c := New(s, worker, alwaysGreen(), nil, Config{})
	if err := c.Run(context.Background(), id); err != nil {
		t.Fatalf("Run: %v", err)
	}
	l, _ := s.Get(id)
	if l.Status != LedgerDone {
		t.Fatalf("status = %s, want done", l.Status)
	}
	for _, u := range l.Units {
		if u.Status != UnitDone {
			t.Errorf("unit %d status = %s, want done", u.ID, u.Status)
		}
	}
}

func TestConductDependencyOrderAndUpstreamContext(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{
		{ID: 1, Description: "make the foo type"},
		{ID: 2, Description: "use the foo type", BlockedBy: []int{1}},
	})
	var order []int
	var mu sync.Mutex
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		mu.Lock()
		order = append(order, unitID)
		mu.Unlock()
		return WorkResult{Reply: "result-of-" + strconv.Itoa(unitID)}
	})
	c := New(s, worker, alwaysGreen(), nil, Config{})
	if err := c.Run(context.Background(), id); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("dispatch order = %v, want [1 2]", order)
	}
	// Unit 2's prompt must carry unit 1's result (the fix for blind downstream).
	p := worker.prompt(2)
	if !strings.Contains(p, "result-of-1") {
		t.Errorf("unit 2 prompt missing upstream result:\n%s", p)
	}
	if !strings.Contains(p, "make the foo type") {
		t.Errorf("unit 2 prompt missing upstream description")
	}
}

func TestConductMaxTurnsCheckpointContinues(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "big task"}})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		if call == 0 {
			return WorkResult{Reply: "partial", Incomplete: true} // hit turn budget
		}
		return WorkResult{Reply: "finished"} // continued and finished
	})
	c := New(s, worker, alwaysGreen(), nil, Config{})
	if err := c.Run(context.Background(), id); err != nil {
		t.Fatalf("Run: %v", err)
	}
	l, _ := s.Get(id)
	if l.Status != LedgerDone {
		t.Fatalf("status = %s, want done", l.Status)
	}
	if worker.callCount(1) != 2 {
		t.Errorf("worker called %d times, want 2 (run + continue)", worker.callCount(1))
	}
	// A checkpoint is not an attempt.
	if l.Units[0].Attempts != 0 {
		t.Errorf("attempts = %d, want 0 (checkpoint is not a failure)", l.Units[0].Attempts)
	}
}

func TestConductVerifyRedThenGreenRetries(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "task"}})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		return WorkResult{Reply: "attempt"}
	})
	verifier := &fnVerifier{fn: func(call int, _ VerifyTarget) Verdict {
		if call == 0 {
			return Verdict{Green: false, Summary: "build broke: undefined Foo"}
		}
		return Verdict{Green: true}
	}}
	c := New(s, worker, verifier, nil, Config{})
	if err := c.Run(context.Background(), id); err != nil {
		t.Fatalf("Run: %v", err)
	}
	l, _ := s.Get(id)
	if l.Status != LedgerDone {
		t.Fatalf("status = %s, want done", l.Status)
	}
	if got := l.Units[0].Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1 (one red verdict)", got)
	}
	if worker.callCount(1) != 2 {
		t.Errorf("worker called %d times, want 2 (run + retry)", worker.callCount(1))
	}
}

func TestConductBlocksAfterMaxAttempts(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "impossible"}})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		return WorkResult{Reply: "tried"}
	})
	redForever := &fnVerifier{fn: func(int, VerifyTarget) Verdict { return Verdict{Green: false, Summary: "nope"} }}
	c := New(s, worker, redForever, nil, Config{MaxAttempts: 2})
	err := c.Run(context.Background(), id)
	if err == nil {
		t.Fatalf("Run returned nil, want failure")
	}
	l, _ := s.Get(id)
	if l.Status != LedgerFailed {
		t.Fatalf("status = %s, want failed", l.Status)
	}
	if l.Units[0].Status != UnitBlocked {
		t.Fatalf("unit status = %s, want blocked", l.Units[0].Status)
	}
	if l.Units[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", l.Units[0].Attempts)
	}
}

func TestConductParallelIndependentUnits(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{
		{ID: 1, Description: "a"},
		{ID: 2, Description: "b"},
		{ID: 3, Description: "c"},
	})
	const n = 3
	var inFlight, maxInFlight int
	var arrived int32
	var mu sync.Mutex
	barrier := make(chan struct{})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		// Block until all n workers have arrived (proving real overlap), or
		// bail after a timeout so a sequential bug fails fast instead of hanging.
		if atomic.AddInt32(&arrived, 1) == n {
			close(barrier)
		}
		select {
		case <-barrier:
		case <-time.After(2 * time.Second):
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return WorkResult{Reply: "done"}
	})
	c := New(s, worker, alwaysGreen(), nil, Config{MaxConcurrent: 3})
	if err := c.Run(context.Background(), id); err != nil {
		t.Fatalf("Run: %v", err)
	}
	l, _ := s.Get(id)
	if l.Status != LedgerDone {
		t.Fatalf("status = %s, want done", l.Status)
	}
	if maxInFlight < 2 {
		t.Errorf("max concurrent workers = %d, want >=2 (parallel dispatch)", maxInFlight)
	}
}

func TestConductStallDetection(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "task"}})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		return WorkResult{Reply: "x", Incomplete: true} // never finishes
	})
	c := New(s, worker, alwaysGreen(), nil, Config{StallRounds: 3})
	err := c.Run(context.Background(), id)
	if err == nil {
		t.Fatalf("Run returned nil, want stall")
	}
	l, _ := s.Get(id)
	if l.Status != LedgerStalled {
		t.Fatalf("status = %s, want stalled", l.Status)
	}
}

func TestConductIterationBudget(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "task"}})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		return WorkResult{Reply: "x", Incomplete: true}
	})
	c := New(s, worker, alwaysGreen(), nil, Config{MaxIterations: 5})
	err := c.Run(context.Background(), id)
	if err == nil {
		t.Fatalf("Run returned nil, want budget stop")
	}
	l, _ := s.Get(id)
	if l.Status != LedgerBudget {
		t.Fatalf("status = %s, want budget", l.Status)
	}
}

// stubReplanner adds one unit the first time it's asked, then no-ops.
type stubReplanner struct {
	added bool
	add   []Unit
}

func (r *stubReplanner) Replan(_ context.Context, _ *Ledger, _ ReplanTrigger) (ReplanResult, error) {
	if r.added {
		return ReplanResult{}, nil
	}
	r.added = true
	return ReplanResult{Add: r.add, Note: "added recovery unit"}, nil
}

func TestConductReplanUnblocks(t *testing.T) {
	s := newTestStore(t)
	// Unit 1 always fails verification → blocks. The re-planner adds an
	// independent unit 2 that succeeds, letting the goal make progress.
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "will block"}})
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		return WorkResult{Reply: "x"}
	})
	// Verify is keyed by call count: unit 1's 3 attempts are red (so it blocks),
	// then the re-planner's recovery unit + the global gate go green.
	calls := 0
	var mu sync.Mutex
	v := &fnVerifier{fn: func(_ int, _ VerifyTarget) Verdict {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 3 {
			return Verdict{Green: false, Summary: "unit1 broke"}
		}
		return Verdict{Green: true}
	}}
	repl := &stubReplanner{add: []Unit{{Description: "recovery"}}}
	c := New(s, worker, v, nil, Config{MaxAttempts: 3}).WithReplanner(repl)
	_ = c.Run(context.Background(), id)
	l, _ := s.Get(id)
	if !repl.added {
		t.Fatalf("replanner was never invoked")
	}
	// The recovery unit (id 2) should exist and be Done.
	u2 := l.Find(2)
	if u2 == nil {
		t.Fatalf("recovery unit not added")
	}
	if u2.Status != UnitDone {
		t.Errorf("recovery unit status = %s, want done", u2.Status)
	}
}

func TestConductCancellation(t *testing.T) {
	s := newTestStore(t)
	id := seed(t, s, "g", []Unit{{ID: 1, Description: "task"}})
	ctx, cancel := context.WithCancel(context.Background())
	worker := newFnWorker(func(unitID, call int, resume bool, spec WorkSpec) WorkResult {
		cancel() // cancel mid-flight
		return WorkResult{Reply: "x", Incomplete: true}
	})
	c := New(s, worker, alwaysGreen(), nil, Config{})
	err := c.Run(ctx, id)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	l, _ := s.Get(id)
	if l.Status != LedgerCancelled {
		t.Fatalf("status = %s, want cancelled", l.Status)
	}
}
