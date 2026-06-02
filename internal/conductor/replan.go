package conductor

import "context"

// ReplanTrigger tells the Replanner why it was invoked, so it can scope its
// response (a routine pre-iteration check vs. a unit that blocked).
type ReplanTrigger struct {
	Reason string // "periodic" | "blocked" | "global_fail"
	// UnitID is the unit that prompted the re-plan (0 for periodic).
	UnitID int
}

// ReplanResult is the ledger edit a re-plan produced. The conductor applies
// it atomically. All slices may be empty (a no-op re-plan).
type ReplanResult struct {
	// Add appends new units. Their IDs are (re)assigned by the conductor to
	// avoid collisions, and any BlockedBy referencing an added unit is
	// remapped from its array index to the assigned id (negative sentinels;
	// see applyReplan).
	Add []Unit
	// Abandon marks these existing unit IDs as Abandoned (kept for audit).
	Abandon []int
	// Note is appended to the journal explaining the change.
	Note string
}

// maybeReplan consults the Replanner before picking work and applies any
// edit. Returns true if the ledger changed. Phase-3: the periodic trigger
// only fires when the ledger looks healthy enough to be worth re-checking;
// blocked/global-fail triggers are raised from finalize/markRedAndMaybeBlock
// paths via Replan with the matching reason.
func (c *Conductor) maybeReplan(ctx context.Context, id string) bool {
	if c.replanner == nil {
		return false
	}
	l, err := c.store.Get(id)
	if err != nil {
		return false
	}
	// maybeReplan is invoked only when the loop is stuck (no actionable units
	// and not all done), so the trigger reflects why.
	trigger := ReplanTrigger{Reason: "stuck"}
	if l.HasBlocked() {
		trigger.Reason = "blocked"
	}
	res, err := c.replanner.Replan(ctx, l, trigger)
	if err != nil {
		c.store.AppendJournal(id, "replan error: "+err.Error())
		return false
	}
	if len(res.Add) == 0 && len(res.Abandon) == 0 {
		return false
	}
	return c.applyReplan(id, res)
}

// applyReplan mutates the ledger per a ReplanResult: abandons units, then
// appends new ones with freshly-assigned IDs. Within res.Add, a BlockedBy
// entry that is negative (-k) is a reference to the k-th (1-based) newly
// added unit and is remapped to its assigned id; non-negative entries
// reference existing units verbatim.
func (c *Conductor) applyReplan(id string, res ReplanResult) bool {
	changed := false
	_, _ = c.store.Update(id, func(l *Ledger) error {
		for _, aid := range res.Abandon {
			if u := l.Find(aid); u != nil && u.Status != UnitDone {
				u.Status = UnitAbandoned
				changed = true
			}
		}
		// Assign IDs to the additions first so intra-batch refs can resolve.
		assigned := make([]int, len(res.Add))
		next := l.nextUnitID()
		for i := range res.Add {
			assigned[i] = next
			next++
		}
		for i, nu := range res.Add {
			nu.ID = assigned[i]
			nu.Status = UnitPending
			remapped := make([]int, 0, len(nu.BlockedBy))
			for _, dep := range nu.BlockedBy {
				if dep < 0 {
					k := -dep - 1 // -1 → index 0
					if k >= 0 && k < len(assigned) {
						remapped = append(remapped, assigned[k])
					}
				} else {
					remapped = append(remapped, dep)
				}
			}
			nu.BlockedBy = remapped
			l.Units = append(l.Units, nu)
			changed = true
		}
		return nil
	})
	if changed {
		note := res.Note
		if note == "" {
			note = "replan applied"
		}
		c.store.AppendJournal(id, "replan: "+note)
		c.logf("↻ replan: %s\n", oneLine(note, 100))
	}
	return changed
}
