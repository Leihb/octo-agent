package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/truncate"
)

// Live task-list styles — Claude Code's todo block: glyph and label styled
// separately per status. Completed = green ✓ with the label struck through;
// in-progress = filled square in the brand colour with the label bold;
// pending = hollow square, label in the terminal's default foreground.
var (
	taskConnStyle   = lipgloss.NewStyle().Foreground(tui.ColDim)
	taskGlyphDone   = lipgloss.NewStyle().Foreground(tui.ColAccent).SetString("✓")
	taskGlyphActive = lipgloss.NewStyle().Foreground(tui.ColBrand).SetString("■")
	taskGlyphPend   = lipgloss.NewStyle().Foreground(tui.ColDim).SetString("□")
	taskTextDone    = lipgloss.NewStyle().Foreground(tui.ColDim).Strikethrough(true)
	taskTextActive  = lipgloss.NewStyle().Bold(true)
)

// maxTaskRows caps how many lines the live block occupies (markers included).
// Beyond it, completed head rows fold into a "✓ N done" line first — they're
// the least pressing — then the tail collapses into "… N more".
const maxTaskRows = 8

// taskListView renders the live task block shown under the activity spinner
// while a turn runs. It returns "" when there's nothing worth showing: no
// store, no tasks, or every task already completed — the block tracks
// outstanding work, so an all-done list would just clutter the live area
// (the committed transcript still holds the history).
func (m *tuiModel) taskListView() string {
	store := tools.ActiveTaskStore()
	if store == nil {
		return ""
	}
	return renderTaskLines(store.List(), m.width)
}

// activeTaskPhrase returns the in-progress task's present-continuous form (or
// its subject), so the activity spinner can read "Migrating config readers…"
// instead of a generic thinking verb. "" when no store or no task is active.
func (m *tuiModel) activeTaskPhrase() string {
	store := tools.ActiveTaskStore()
	if store == nil {
		return ""
	}
	items := store.List() // ordered in-progress first
	if len(items) == 0 || items[0].Status != tasks.InProgress {
		return ""
	}
	if items[0].ActiveForm != "" {
		return items[0].ActiveForm
	}
	return items[0].Subject
}

// renderTaskLines is the pure formatter behind taskListView, split out so it
// can be tested without the global store. width <= 0 disables truncation.
func renderTaskLines(items []tasks.Task, width int) string {
	// Hide the block once nothing is outstanding (all completed / empty).
	outstanding := false
	for _, t := range items {
		if t.Status == tasks.Pending || t.Status == tasks.InProgress {
			outstanding = true
			break
		}
	}
	if !outstanding {
		return ""
	}

	// Checklist order: by creation (ID), so the block reads top-to-bottom as
	// progress — completed head, active middle, pending tail (Claude Code
	// style). List() is status-sorted for other consumers; re-sort here.
	rows := append([]tasks.Task(nil), items...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	// Over budget: fold completed rows off the head into one "✓ N done" line.
	foldedDone := 0
	for len(rows) > maxTaskRows-1 && rows[0].Status == tasks.Completed {
		rows = rows[1:]
		foldedDone++
	}
	// Still over (long pending tail): collapse the tail into "… N more".
	budget := maxTaskRows
	if foldedDone > 0 {
		budget-- // the fold marker occupies a line
	}
	moreCount := 0
	if len(rows) > budget {
		moreCount = len(rows) - (budget - 1)
		rows = rows[:budget-1]
	}

	var b strings.Builder
	// The first line carries the tree elbow tying the block to the spinner
	// above; the rest align under it.
	writeLine := func(s string) {
		if b.Len() == 0 {
			b.WriteString(taskConnStyle.Render("└ "))
		} else {
			b.WriteString("\n  ")
		}
		b.WriteString(s)
	}
	if foldedDone > 0 {
		writeLine(taskGlyphDone.String() + " " + taskConnStyle.Render(fmt.Sprintf("%d done", foldedDone)))
	}
	for _, t := range rows {
		writeLine(renderTaskRow(t, width))
	}
	if moreCount > 0 {
		writeLine(taskConnStyle.Render(fmt.Sprintf("… %d more", moreCount)))
	}
	return b.String()
}

// renderTaskRow formats a single task line: status glyph + subject, glyph and
// label styled separately, the label truncated to fit the terminal width
// (2-column indent + glyph + space accounted for).
func renderTaskRow(t tasks.Task, width int) string {
	text := t.Subject
	if width > 8 {
		text = truncate.StringWithTail(text, uint(width-6), "…")
	}
	switch t.Status {
	case tasks.InProgress:
		return taskGlyphActive.String() + " " + taskTextActive.Render(text)
	case tasks.Completed:
		return taskGlyphDone.String() + " " + taskTextDone.Render(text)
	default: // Pending
		return taskGlyphPend.String() + " " + text
	}
}
