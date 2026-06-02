package conductor

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Report writes a structured human-readable summary of a ledger: overall
// status, per-unit state, and the blocked/abandoned units that need
// attention. Used by `octo conduct status/report` and printed at the end of
// an unattended run.
func Report(w io.Writer, l *Ledger) {
	fmt.Fprintf(w, "Ledger %s  [%s]  (%d iterations)\n", l.ShortID(), l.Status, l.Iterations)
	fmt.Fprintf(w, "Goal: %s\n\n", oneLine(l.Goal, 0))

	counts := map[UnitStatus]int{}
	for _, u := range l.Units {
		counts[u.Status]++
	}
	fmt.Fprintf(w, "Units: %d done · %d in-progress · %d pending · %d blocked · %d abandoned\n\n",
		counts[UnitDone], counts[UnitInProgress], counts[UnitPending], counts[UnitBlocked], counts[UnitAbandoned])

	for _, u := range l.Units {
		fmt.Fprintf(w, "  %s #%-3d %s\n", unitGlyph(u.Status), u.ID, oneLine(u.Description, 80))
		if len(u.BlockedBy) > 0 {
			fmt.Fprintf(w, "        ↳ blocked_by: %s\n", joinInts(u.BlockedBy))
		}
		if u.Status == UnitBlocked && u.LastError != "" {
			fmt.Fprintf(w, "        ✗ %s\n", oneLine(u.LastError, 100))
		}
	}

	// Call out what needs a human, if anything.
	var blocked []int
	for _, u := range l.Units {
		if u.Status == UnitBlocked {
			blocked = append(blocked, u.ID)
		}
	}
	if len(blocked) > 0 {
		sort.Ints(blocked)
		fmt.Fprintf(w, "\nNeeds attention: %d blocked unit(s): %s\n", len(blocked), joinInts(blocked))
		fmt.Fprintf(w, "Resume after intervention with: octo conduct resume %s\n", l.ShortID())
	}
}

func unitGlyph(s UnitStatus) string {
	switch s {
	case UnitDone:
		return "✓"
	case UnitInProgress:
		return "▶"
	case UnitBlocked:
		return "✗"
	case UnitAbandoned:
		return "∅"
	default:
		return "·"
	}
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ", ")
}
