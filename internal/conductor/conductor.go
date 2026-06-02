package conductor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Worker dispatches one unit's prompt to an isolated sub-agent. Run starts a
// fresh worker; Continue resumes a still-alive one (its history carries over),
// used both for max-turns checkpoints and verify-fail retries. The cmd/octo
// adapter wires these onto agentSpawner.Spawn/Continue.
type Worker interface {
	Run(ctx context.Context, spec WorkSpec) (WorkResult, error)
	Continue(ctx context.Context, handle, message string) (WorkResult, error)
}

// WorkSpec is one worker dispatch.
type WorkSpec struct {
	Label   string // short label for progress/logs
	Prompt  string // the fully assembled worker context
	Workdir string // Phase 2: the worktree to root the worker's shell in ("" = process cwd)
}

// WorkResult is a worker round's outcome. Incomplete is true when the worker
// hit its turn budget (max_turns) — the conductor checkpoints and continues
// rather than treating it as a failure. Handle addresses the child for a
// later Continue.
type WorkResult struct {
	Handle     string
	Reply      string
	Incomplete bool
}

// Verifier is the objective completion gate. A unit is never Done until
// Verify returns Green. workdir is the directory to check ("" = process cwd;
// Phase 2 passes the unit's worktree).
type Verifier interface {
	Verify(ctx context.Context, workdir string) (Verdict, error)
}

// Verdict is a verification outcome. Summary is short failure text fed back
// to the worker on a red verdict (e.g. the first compile errors).
type Verdict struct {
	Green   bool
	Summary string
}

// Worktrees isolates a unit's work in its own git worktree (Phase 2). nil on
// the Conductor means workers run in the process working directory.
type Worktrees interface {
	Create(ctx context.Context, unitID int) (branch, dir string, err error)
	Merge(ctx context.Context, branch string) error
	Cleanup(ctx context.Context, branch, dir string) error
}

// Replanner revises the ledger from observed reality (Phase 3). nil disables
// re-planning — the seed plan is executed as-is.
type Replanner interface {
	Replan(ctx context.Context, l *Ledger, trigger ReplanTrigger) (ReplanResult, error)
}

// Config tunes the loop's guardrails.
type Config struct {
	// MaxAttempts caps verify-fail retries per unit before it is Blocked.
	// A max-turns checkpoint does not count as an attempt. <=0 → 3.
	MaxAttempts int
	// MaxIterations caps total loop turns (a runaway backstop / budget). <=0
	// → a generous default scaled by unit count.
	MaxIterations int
	// MaxConcurrent caps parallel worker dispatch (Phase 2). <=1 → sequential.
	MaxConcurrent int
	// StallRounds: after this many consecutive iterations with no unit
	// reaching Done, the loop declares a stall and stops (Phase 3). <=0 → off.
	StallRounds int
}

func (c Config) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return 3
}

// maxRecoverRounds bounds how many consecutive re-plan attempts the loop will
// make when stuck (no actionable units) before giving up — a backstop against
// a re-planner that keeps adding units that never complete.
const maxRecoverRounds = 5

func (c Config) maxIterations(unitCount int) int {
	if c.MaxIterations > 0 {
		return c.MaxIterations
	}
	// Each unit may take a few continue rounds + retries; scale generously.
	n := unitCount * 12
	if n < 50 {
		n = 50
	}
	return n
}

// Conductor drives one ledger to a terminal state.
type Conductor struct {
	store     *Store
	worker    Worker
	verifier  Verifier
	worktrees Worktrees // nil in Phase 1
	replanner Replanner // nil unless Phase 3 re-planning is wired
	out       io.Writer
	outMu     sync.Mutex // serializes logf across concurrent workers
	cfg       Config
}

// New wires a conductor. worktrees and replanner may be nil. out may be nil
// (silent).
func New(store *Store, worker Worker, verifier Verifier, out io.Writer, cfg Config) *Conductor {
	return &Conductor{store: store, worker: worker, verifier: verifier, out: out, cfg: cfg}
}

// WithWorktrees enables Phase 2 isolation.
func (c *Conductor) WithWorktrees(w Worktrees) *Conductor { c.worktrees = w; return c }

// WithReplanner enables Phase 3 re-planning.
func (c *Conductor) WithReplanner(r Replanner) *Conductor { c.replanner = r; return c }

// Run drives the ledger to a terminal state. Returns nil only when the goal
// reached LedgerDone; every other terminal state returns a descriptive error
// so a CLI can map it to an exit code. The structured outcome is always on
// disk (and via Report) regardless of the return.
func (c *Conductor) Run(ctx context.Context, id string) error {
	if c.worker == nil || c.verifier == nil {
		return errors.New("conductor: worker and verifier are required")
	}

	l, err := c.store.Update(id, func(l *Ledger) error {
		if l.Status.Terminal() && l.Status != LedgerCancelled {
			return fmt.Errorf("conductor: ledger already %s", l.Status)
		}
		l.Status = LedgerRunning
		return nil
	})
	if err != nil {
		return err
	}
	c.logf("Conducting %s — %d units\n", l.ShortID(), len(l.Units))
	c.store.AppendJournal(id, fmt.Sprintf("run started: %d units", len(l.Units)))

	maxIter := c.cfg.maxIterations(len(l.Units))
	roundsSinceProgress := 0
	recoverRounds := 0

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.setStatus(id, LedgerCancelled)
			c.store.AppendJournal(id, "cancelled (context)")
			return ctxErr
		}

		l, err = c.store.Get(id)
		if err != nil {
			return err
		}

		if l.Iterations >= maxIter {
			c.setStatus(id, LedgerBudget)
			c.store.AppendJournal(id, fmt.Sprintf("budget: hit max iterations (%d)", maxIter))
			return fmt.Errorf("conductor: ledger %s hit iteration budget (%d)", id, maxIter)
		}

		batch := c.pickBatch(l)
		if len(batch) == 0 {
			// Nothing actionable. Before giving up, let the re-planner (Phase 3)
			// try to unblock the goal — but bound how many times so a useless
			// re-planner can't spin forever.
			if c.replanner != nil && !l.AllDone() && recoverRounds < maxRecoverRounds {
				if c.maybeReplan(ctx, id) {
					recoverRounds++
					continue
				}
			}
			return c.finalize(ctx, id)
		}

		progressed := c.runBatch(ctx, id, batch)

		// Bump the iteration counter once per worker dispatch in the batch.
		n := len(batch)
		_, _ = c.store.Update(id, func(l *Ledger) error { l.Iterations += n; return nil })

		if progressed {
			roundsSinceProgress = 0
			recoverRounds = 0
		} else {
			roundsSinceProgress++
			if c.cfg.StallRounds > 0 && roundsSinceProgress >= c.cfg.StallRounds {
				c.setStatus(id, LedgerStalled)
				c.store.AppendJournal(id, fmt.Sprintf("stalled: %d rounds with no unit completed", roundsSinceProgress))
				return fmt.Errorf("conductor: ledger %s stalled (no progress in %d rounds)", id, roundsSinceProgress)
			}
		}
	}
}

// dispatchItem is one unit to act on this round.
type dispatchItem struct {
	unitID int
	resume bool // Continue an existing worker (vs. Run a fresh one)
}

// pickBatch selects up to MaxConcurrent actionable units. In-progress units
// (resume via Continue) take priority so checkpointed/retrying work finishes
// before new work starts; the remainder is filled with the lowest-id ready
// units. With MaxConcurrent<=1 this returns at most one item — the sequential
// Phase-1 behaviour. Ready units never depend on in-progress ones (ready
// means all deps Done), so a batch is always free of internal dependencies.
func (c *Conductor) pickBatch(l *Ledger) []dispatchItem {
	limit := c.cfg.MaxConcurrent
	if limit < 1 {
		limit = 1
	}
	var batch []dispatchItem
	for _, uid := range l.InProgressUnits() {
		if len(batch) >= limit {
			return batch
		}
		u := l.Find(uid)
		batch = append(batch, dispatchItem{unitID: uid, resume: u != nil && u.Handle != ""})
	}
	for _, uid := range l.ReadyUnits() {
		if len(batch) >= limit {
			break
		}
		batch = append(batch, dispatchItem{unitID: uid, resume: false})
	}
	return batch
}

// dispatchOutcome is one unit's worker round + verification, ready for the
// serial integrate step (which owns merges + state transitions).
type dispatchOutcome struct {
	unitID    int
	branch    string
	workdir   string
	res       WorkResult
	workerErr error
	verdict   Verdict
	verified  bool // verify actually ran (false on worker error / checkpoint)
}

// runBatch dispatches each item's worker round (and per-unit verification)
// concurrently, then integrates the outcomes SERIALLY in unit-id order. The
// concurrency is the design's bounded parallelism; the serial integrate keeps
// trunk merges + ledger transitions race-free. Returns whether any unit
// reached Done this round.
func (c *Conductor) runBatch(ctx context.Context, id string, batch []dispatchItem) bool {
	outcomes := make([]dispatchOutcome, len(batch))
	if len(batch) == 1 {
		outcomes[0] = c.dispatchUnit(ctx, id, batch[0])
	} else {
		var wg sync.WaitGroup
		for i, item := range batch {
			wg.Add(1)
			go func(i int, item dispatchItem) {
				defer wg.Done()
				outcomes[i] = c.dispatchUnit(ctx, id, item)
			}(i, item)
		}
		wg.Wait()
	}

	progressed := false
	for _, oc := range outcomes {
		if c.integrate(ctx, id, oc) {
			progressed = true
		}
	}
	return progressed
}

// dispatchUnit runs (or resumes) one unit's worker and verifies its output in
// the unit's own working directory. It mutates only this unit's running marker,
// handle, reply, and worktree fields — all through the store's mutex, so it is
// safe to run concurrently for distinct units. Merges and terminal-state
// transitions are deferred to integrate (serial). Safe to call from a goroutine.
func (c *Conductor) dispatchUnit(ctx context.Context, id string, item dispatchItem) dispatchOutcome {
	unitID := item.unitID
	oc := dispatchOutcome{unitID: unitID}

	l, err := c.store.Get(id)
	if err != nil {
		oc.workerErr = err
		return oc
	}
	u := l.Find(unitID)
	if u == nil {
		oc.workerErr = fmt.Errorf("unit %d vanished", unitID)
		return oc
	}

	// Ensure a worktree exists for this unit (Phase 2). Errors fall back to the
	// process cwd so a worktree hiccup doesn't strand the unit.
	oc.branch, oc.workdir = u.Branch, u.Worktree
	if c.worktrees != nil && oc.branch == "" && !item.resume {
		b, d, werr := c.worktrees.Create(ctx, unitID)
		if werr != nil {
			c.logf("⚠ #%d worktree create failed (%v) — using main tree\n", unitID, werr)
			c.store.AppendJournal(id, fmt.Sprintf("#%d worktree create failed: %v", unitID, werr))
		} else {
			oc.branch, oc.workdir = b, d
			_, _ = c.store.Update(id, func(l *Ledger) error {
				if uu := l.Find(unitID); uu != nil {
					uu.Branch, uu.Worktree = b, d
				}
				return nil
			})
		}
	}

	_, _ = c.store.Update(id, func(l *Ledger) error {
		if uu := l.Find(unitID); uu != nil {
			uu.Status = UnitInProgress
			if uu.Started == nil {
				now := time.Now().UTC()
				uu.Started = &now
			}
		}
		return nil
	})

	if item.resume && u.Handle != "" {
		msg := u.ContinueMsg
		if msg == "" {
			msg = "Continue where you left off and finish the task."
		}
		c.logf("▶ #%d resuming\n", unitID)
		oc.res, oc.workerErr = c.worker.Continue(ctx, u.Handle, msg)
	} else {
		spec := WorkSpec{
			Label:   fmt.Sprintf("#%d %s", unitID, oneLine(u.Description, 60)),
			Prompt:  c.buildPrompt(id, l, u, oc.workdir),
			Workdir: oc.workdir,
		}
		c.logf("▶ #%d running\n", unitID)
		oc.res, oc.workerErr = c.worker.Run(ctx, spec)
	}

	if oc.workerErr != nil {
		return oc
	}

	// Persist handle + latest reply.
	_, _ = c.store.Update(id, func(l *Ledger) error {
		if uu := l.Find(unitID); uu != nil {
			if oc.res.Handle != "" {
				uu.Handle = oc.res.Handle
			}
			if oc.res.Reply != "" {
				uu.ResultSummary = oc.res.Reply
			}
		}
		return nil
	})

	if oc.res.Incomplete {
		return oc // checkpoint — integrate handles it, no verify
	}

	// Worker says done — verify objectively in its working dir.
	verdict, vErr := c.verifier.Verify(ctx, oc.workdir)
	if vErr != nil && ctx.Err() == nil {
		verdict = Verdict{Green: false, Summary: "verifier error: " + vErr.Error()}
	}
	oc.verdict = verdict
	oc.verified = true
	return oc
}

// integrate applies one dispatched outcome to the ledger (serial): worker
// error → retry/block; checkpoint → continue next round; green → merge + done;
// red → retry/block. Returns whether the unit reached Done.
func (c *Conductor) integrate(ctx context.Context, id string, oc dispatchOutcome) bool {
	if ctx.Err() != nil {
		return false // cancellation — let the loop top handle it
	}
	unitID := oc.unitID

	if oc.workerErr != nil {
		return c.applyWorkerError(id, unitID, oc.workerErr)
	}

	if oc.res.Incomplete {
		_, _ = c.store.Update(id, func(l *Ledger) error {
			if uu := l.Find(unitID); uu != nil {
				uu.Status = UnitInProgress
				uu.ContinueMsg = "You hit the turn budget mid-task. Continue exactly where you left off and finish."
			}
			return nil
		})
		c.logf("⏸ #%d checkpoint (turn budget) — will continue\n", unitID)
		c.store.AppendJournal(id, fmt.Sprintf("#%d checkpoint at turn budget", unitID))
		return false
	}

	if oc.verdict.Green {
		return c.markDone(ctx, id, unitID, oc.branch, oc.workdir)
	}
	return c.markRedAndMaybeBlock(id, unitID, oc.verdict)
}

// markDone integrates the unit (Phase 2 merge) and records completion.
func (c *Conductor) markDone(ctx context.Context, id string, unitID int, branch, workdir string) bool {
	if c.worktrees != nil && branch != "" {
		if err := c.worktrees.Merge(ctx, branch); err != nil {
			// Merge failure is a verify-style failure: the unit's work doesn't
			// integrate cleanly. Surface it back to the worker as a retry.
			c.logf("✗ #%d merge failed: %v\n", unitID, err)
			return c.markRedAndMaybeBlock(id, unitID, Verdict{
				Summary: "your branch does not merge into the trunk cleanly:\n" + err.Error() +
					"\nrebase onto the latest trunk and resolve the conflicts.",
			})
		}
		_ = c.worktrees.Cleanup(ctx, branch, workdir)
	}
	_, _ = c.store.Update(id, func(l *Ledger) error {
		if uu := l.Find(unitID); uu != nil {
			uu.Status = UnitDone
			uu.ContinueMsg = ""
			uu.LastError = ""
			uu.LastVerdict = "green"
			uu.Handle = "" // child can be released
			now := time.Now().UTC()
			uu.Finished = &now
		}
		return nil
	})
	c.logf("✓ #%d done\n", unitID)
	c.store.AppendJournal(id, fmt.Sprintf("#%d done (verified green)", unitID))
	return true
}

// markRedAndMaybeBlock records a failed verdict and either queues a retry
// (keeping the unit in_progress so the next round Continues it with the
// failure as context) or marks it Blocked once attempts are exhausted.
func (c *Conductor) markRedAndMaybeBlock(id string, unitID int, v Verdict) bool {
	blocked := false
	_, _ = c.store.Update(id, func(l *Ledger) error {
		uu := l.Find(unitID)
		if uu == nil {
			return nil
		}
		uu.Attempts++
		uu.LastVerdict = "red"
		uu.LastError = v.Summary
		if uu.Attempts >= c.cfg.maxAttempts() {
			uu.Status = UnitBlocked
			uu.ContinueMsg = ""
			blocked = true
		} else {
			uu.Status = UnitInProgress
			uu.ContinueMsg = "Verification failed:\n" + v.Summary +
				"\n\nFix the issues so the build and tests pass, then report."
		}
		return nil
	})
	if blocked {
		c.logf("✗ #%d blocked after %d attempts\n", unitID, c.cfg.maxAttempts())
		c.store.AppendJournal(id, fmt.Sprintf("#%d blocked (attempts exhausted): %s", unitID, oneLine(v.Summary, 120)))
	} else {
		c.logf("✗ #%d verify red — will retry\n", unitID)
		c.store.AppendJournal(id, fmt.Sprintf("#%d verify red, retry queued: %s", unitID, oneLine(v.Summary, 120)))
	}
	return false
}

// applyWorkerError treats a hard worker error like a red verdict (retry or
// block). Cancellation is handled by the caller before reaching here.
func (c *Conductor) applyWorkerError(id string, unitID int, err error) bool {
	return c.markRedAndMaybeBlock(id, unitID, Verdict{Summary: "worker error: " + err.Error()})
}

// finalize runs the global gate when no units are actionable and sets the
// terminal status.
func (c *Conductor) finalize(ctx context.Context, id string) error {
	l, err := c.store.Get(id)
	if err != nil {
		return err
	}
	if l.AllDone() {
		// Global verification over the whole tree before declaring success.
		verdict, vErr := c.verifier.Verify(ctx, "")
		if vErr == nil && verdict.Green {
			c.setStatus(id, LedgerDone)
			c.logf("Goal %s complete — global verification green.\n", l.ShortID())
			c.store.AppendJournal(id, "done: all units complete, global gate green")
			return nil
		}
		// Units claim done but the whole tree doesn't pass: a re-planner (if
		// any) gets a chance next loop; otherwise it's a failure.
		summary := "global verification failed"
		if vErr != nil {
			summary = "global verification error: " + vErr.Error()
		} else if verdict.Summary != "" {
			summary = verdict.Summary
		}
		c.setStatus(id, LedgerFailed)
		c.store.AppendJournal(id, "failed: "+oneLine(summary, 160))
		return fmt.Errorf("conductor: ledger %s — %s", id, summary)
	}
	// Something is stuck (blocked units, or units depending on blocked/abandoned).
	c.setStatus(id, LedgerFailed)
	c.store.AppendJournal(id, "failed: blocked units with no further progress possible")
	return fmt.Errorf("conductor: ledger %s failed — blocked units remain", id)
}

func (c *Conductor) setStatus(id string, st LedgerStatus) {
	_, _ = c.store.Update(id, func(l *Ledger) error { l.Status = st; return nil })
}

func (c *Conductor) logf(format string, args ...any) {
	if c.out == nil {
		return
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	fmt.Fprintf(c.out, format, args...)
}

// oneLine collapses whitespace and truncates to n runes for compact logs.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n > 0 && len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
