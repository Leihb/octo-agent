package conductor

import (
	"fmt"
	"strings"
)

// maxConventionsChars caps how much of the shared conventions doc is inlined
// into a worker prompt. The doc grows over a long run; we keep the head
// (earliest, most foundational decisions) and note the truncation, pointing
// the worker at the on-disk path for the full text.
const maxConventionsChars = 8000

// maxUpstreamChars caps each upstream result summary inlined into the prompt.
const maxUpstreamChars = 1500

// buildPrompt assembles the full context a worker needs to do its unit
// without seeing the parent conversation: the goal, the unit, the shared
// conventions doc, upstream results, where to record new decisions, and the
// objective bar it will be checked against. This is the concrete fix for
// taskgraph's stateless, mutually-blind sub-agents.
func (c *Conductor) buildPrompt(id string, l *Ledger, u *Unit, workdir string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are one worker in an autonomous effort toward this overall goal:\n\n  %s\n\n", l.Goal)

	b.WriteString("=== YOUR UNIT ===\n")
	fmt.Fprintf(&b, "#%d: %s\n\n", u.ID, strings.TrimSpace(u.Description))

	// Shared conventions — the cross-worker brain.
	conv := c.store.ReadConventions(id)
	convPath := c.store.ConventionsPath(id)
	if s := strings.TrimSpace(conv); s != "" {
		if len(s) > maxConventionsChars {
			s = s[:maxConventionsChars] + "\n…[truncated — read the full file at " + convPath + "]"
		}
		b.WriteString("=== SHARED CONVENTIONS & DECISIONS (read before you start) ===\n")
		b.WriteString(s)
		b.WriteString("\n\n")
	}

	// Upstream results so the worker builds on, not against, prior units.
	if ups := l.UpstreamResults(u.ID); len(ups) > 0 {
		b.WriteString("=== UPSTREAM RESULTS (what the units you depend on produced) ===\n")
		for _, up := range ups {
			summary := strings.TrimSpace(up.Summary)
			if summary == "" {
				summary = "(no summary reported)"
			}
			if len(summary) > maxUpstreamChars {
				summary = summary[:maxUpstreamChars] + "…[truncated]"
			}
			fmt.Fprintf(&b, "• #%d %s:\n%s\n\n", up.ID, oneLine(up.Description, 80), summary)
		}
	}

	// Retry / continuation feedback.
	if strings.TrimSpace(u.LastError) != "" && u.LastVerdict == "red" {
		b.WriteString("=== PREVIOUS ATTEMPT FAILED VERIFICATION ===\n")
		b.WriteString(strings.TrimSpace(u.LastError))
		b.WriteString("\nFix these issues this round.\n\n")
	}

	b.WriteString("=== HOW TO WORK ===\n")
	if workdir != "" {
		fmt.Fprintf(&b, "- Your working tree is an isolated git worktree at:\n    %s\n", workdir)
		b.WriteString("  Do ALL file edits and shell commands there. `cd` into it first and use absolute paths under it.\n")
	}
	fmt.Fprintf(&b, "- Record any naming, package-layout, type-mapping, or interface decisions you make by\n"+
		"  APPENDING them to the shared conventions file so later units stay consistent:\n    %s\n", convPath)
	b.WriteString("- Verify your own work as you go. This unit is only accepted once the project's\n" +
		"  verification gate passes (e.g. the build and tests are green).\n")
	b.WriteString("- When finished, reply with a concise summary of what you changed and any decisions\n" +
		"  a downstream unit must know. That summary is the only thing downstream units see.\n")

	return b.String()
}
