// Package conductor implements octo's autonomous long-horizon orchestration:
// a single-threaded loop that drives a large goal to completion through a
// living, on-disk task ledger, isolated worker sub-agents, and an objective
// verification gate.
//
// It is the successor to internal/taskgraph for big coherent work (a whole
// refactor, a TS→Go port). Where taskgraph plans an immutable DAG once and
// fans out stateless, mutually-blind sub-agents under stop-on-fail, the
// conductor keeps a mutable ledger it re-plans against, threads upstream
// results + a shared conventions document into every worker, treats
// max-turns as a checkpoint (Continue) rather than a failure, and only marks
// a unit done once an objective Verifier (e.g. `go build && go vet && go
// test`) passes. See dev-docs/autonomous-orchestrator-design.md.
//
// This file is the data layer: the Ledger / Unit state machine. It has no
// LLM, scheduler, or git dependency — those live in conductor.go and the
// cmd/octo adapters. Persistence lives in store.go.
package conductor

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// LedgerStatus is the top-level lifecycle of a conducted goal.
//
//	pending    on disk, the loop hasn't started
//	running    the conductor is iterating
//	done       every unit reached Done and the global gate is green
//	failed     a unit exhausted its attempts (became Blocked) and no progress
//	           is possible without intervention
//	stalled    K iterations passed with no progress — the loop gave up
//	cancelled  the user cancelled (ctx) the run
//	budget     a budget cap (tokens / iterations / wall-clock) was hit
type LedgerStatus string

const (
	LedgerPending   LedgerStatus = "pending"
	LedgerRunning   LedgerStatus = "running"
	LedgerDone      LedgerStatus = "done"
	LedgerFailed    LedgerStatus = "failed"
	LedgerStalled   LedgerStatus = "stalled"
	LedgerCancelled LedgerStatus = "cancelled"
	LedgerBudget    LedgerStatus = "budget"
)

// Terminal reports whether s is an end state the loop won't advance past.
func (s LedgerStatus) Terminal() bool {
	switch s {
	case LedgerDone, LedgerFailed, LedgerStalled, LedgerCancelled, LedgerBudget:
		return true
	}
	return false
}

// UnitStatus is the per-unit lifecycle.
//
//	pending      not started; eligible once BlockedBy are all Done
//	in_progress  a worker is mid-task (incl. checkpointed at max-turns,
//	             awaiting Continue) — Handle addresses the resumable child
//	blocked      attempts exhausted; needs a re-plan or human intervention
//	done         worker finished AND the Verifier passed
//	abandoned    a re-plan dropped this unit (kept for audit)
type UnitStatus string

const (
	UnitPending    UnitStatus = "pending"
	UnitInProgress UnitStatus = "in_progress"
	UnitBlocked    UnitStatus = "blocked"
	UnitDone       UnitStatus = "done"
	UnitAbandoned  UnitStatus = "abandoned"
)

// Unit is one work item in the ledger.
//
// BlockedBy lists IDs (1-based, within the same Ledger) that must reach
// UnitDone before this unit is eligible. An empty BlockedBy means "ready
// from the start".
//
// Handle is the worker-side resumable id returned by the first Run; a later
// Continue (max-turns checkpoint or a verify-fail retry) addresses the same
// child by it so the partial work and its history carry over. Empty until a
// worker has run at least once.
type Unit struct {
	ID          int        `json:"id"`
	Description string     `json:"description"`
	Status      UnitStatus `json:"status"`
	BlockedBy   []int      `json:"blocked_by,omitempty"`

	// Handle is the live worker id for Continue (cleared when the child is
	// gone / the unit is Done). Branch/Worktree are set in Phase 2 when the
	// unit runs in its own git worktree.
	Handle   string `json:"handle,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`

	// Attempts counts how many verify-fail retry rounds this unit has taken
	// (a max-turns checkpoint+continue does NOT count as an attempt — only a
	// red verdict does). Compared against Config.MaxAttempts.
	Attempts int `json:"attempts,omitempty"`

	// ResultSummary is the worker's latest reply, surfaced to downstream
	// units as upstream context. LastError / LastVerdict capture the most
	// recent failure for retry context and the report.
	ResultSummary string `json:"result_summary,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastVerdict   string `json:"last_verdict,omitempty"`

	// ContinueMsg is the message to send on the next Continue of this unit's
	// worker: a turn-budget nudge after a max-turns checkpoint, or the verdict
	// feedback after a red verification. Cleared once the unit is Done/Blocked.
	ContinueMsg string `json:"continue_msg,omitempty"`

	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
}

// Ledger is one conducted goal plus its mutable list of units. Unlike a
// taskgraph.Task, the unit list is expected to change over a run: the
// conductor may split, add, reorder, or abandon units as it re-plans.
type Ledger struct {
	ID      string       `json:"id"`
	Goal    string       `json:"goal"`
	Status  LedgerStatus `json:"status"`
	Created time.Time    `json:"created"`
	Updated time.Time    `json:"updated"`
	Units   []Unit       `json:"units"`

	// Iterations counts conductor loop turns so far — used for the
	// iteration budget cap and the report. Persisted so resume continues
	// counting rather than resetting.
	Iterations int `json:"iterations,omitempty"`
}

// Find returns a pointer into u.Units for the given id, or nil. Mutations to
// the returned pointer are visible to the Ledger; persist via Store.Update.
func (l *Ledger) Find(id int) *Unit {
	for i := range l.Units {
		if l.Units[i].ID == id {
			return &l.Units[i]
		}
	}
	return nil
}

// Ready reports whether u is eligible to run: Pending with every BlockedBy
// dependency Done. Abandoned dependencies are treated as unsatisfiable (a
// unit blocked by an abandoned one can never become ready — the re-planner
// is expected to rewire it).
func (u Unit) Ready(l *Ledger) bool {
	if u.Status != UnitPending {
		return false
	}
	for _, dep := range u.BlockedBy {
		d := l.Find(dep)
		if d == nil || d.Status != UnitDone {
			return false
		}
	}
	return true
}

// ReadyUnits returns the IDs of every ready unit, ascending.
func (l *Ledger) ReadyUnits() []int {
	var ids []int
	for _, u := range l.Units {
		if u.Ready(l) {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

// InProgressUnits returns the IDs of units a worker is mid-task on (incl.
// max-turns checkpoints awaiting Continue), ascending. The loop resumes
// these before picking up fresh Pending units.
func (l *Ledger) InProgressUnits() []int {
	var ids []int
	for _, u := range l.Units {
		if u.Status == UnitInProgress {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

// AllDone reports whether every non-abandoned unit reached Done.
func (l *Ledger) AllDone() bool {
	any := false
	for _, u := range l.Units {
		if u.Status == UnitAbandoned {
			continue
		}
		any = true
		if u.Status != UnitDone {
			return false
		}
	}
	return any
}

// HasBlocked reports whether any unit is Blocked (attempts exhausted).
func (l *Ledger) HasBlocked() bool {
	for _, u := range l.Units {
		if u.Status == UnitBlocked {
			return true
		}
	}
	return false
}

// UpstreamResults returns the (id, summary) of every Done dependency of the
// unit with the given id, so the conductor can thread them into the worker
// prompt. This is the fix for taskgraph's blind-downstream flaw.
func (l *Ledger) UpstreamResults(id int) []UpstreamResult {
	u := l.Find(id)
	if u == nil {
		return nil
	}
	var out []UpstreamResult
	for _, dep := range u.BlockedBy {
		d := l.Find(dep)
		if d == nil || d.Status != UnitDone {
			continue
		}
		out = append(out, UpstreamResult{ID: d.ID, Description: d.Description, Summary: d.ResultSummary})
	}
	return out
}

// UpstreamResult is one Done dependency's outcome, fed to a downstream worker.
type UpstreamResult struct {
	ID          int
	Description string
	Summary     string
}

// nextUnitID returns the smallest positive id not already used, so a
// re-planner can append units without colliding. IDs are never reused even
// after abandonment (abandoned units stay in the list for audit).
func (l *Ledger) nextUnitID() int {
	max := 0
	for _, u := range l.Units {
		if u.ID > max {
			max = u.ID
		}
	}
	return max + 1
}

// validateUnits checks ledger integrity: non-empty descriptions, unique IDs,
// and BlockedBy references that point at existing units without forming a
// cycle. Unlike taskgraph this allows non-contiguous IDs (re-planning leaves
// gaps) and forward edges as long as the graph is acyclic.
func validateUnits(units []Unit) error {
	if len(units) == 0 {
		return errors.New("conductor: at least one unit is required")
	}
	ids := make(map[int]bool, len(units))
	for i, u := range units {
		if u.ID <= 0 {
			return fmt.Errorf("conductor: unit at index %d has non-positive id %d", i, u.ID)
		}
		if ids[u.ID] {
			return fmt.Errorf("conductor: duplicate unit id %d", u.ID)
		}
		if strings.TrimSpace(u.Description) == "" {
			return fmt.Errorf("conductor: unit %d has empty description", u.ID)
		}
		ids[u.ID] = true
	}
	for _, u := range units {
		for _, dep := range u.BlockedBy {
			if dep == u.ID {
				return fmt.Errorf("conductor: unit %d depends on itself", u.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("conductor: unit %d depends on unknown unit %d", u.ID, dep)
			}
		}
	}
	return cycleCheck(units)
}

// cycleCheck runs a DFS over the BlockedBy edges and reports the first cycle.
func cycleCheck(units []Unit) error {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := make(map[int]int, len(units))
	byID := make(map[int]Unit, len(units))
	for _, u := range units {
		byID[u.ID] = u
	}
	var visit func(id int) error
	visit = func(id int) error {
		color[id] = gray
		for _, dep := range byID[id].BlockedBy {
			switch color[dep] {
			case gray:
				return fmt.Errorf("conductor: dependency cycle through unit %d", dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for _, u := range units {
		if color[u.ID] == white {
			if err := visit(u.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ShortID returns the trailing 8 chars of the ledger id for compact display.
func (l *Ledger) ShortID() string {
	if len(l.ID) <= 8 {
		return l.ID
	}
	return l.ID[len(l.ID)-8:]
}
