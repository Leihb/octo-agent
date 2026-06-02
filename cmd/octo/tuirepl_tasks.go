package main

import (
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/truncate"
)

// Live task-list styles — Claude Code's todo block: a checkbox glyph per task,
// the in-progress one highlighted, completed ones dimmed and struck through.
var (
	taskConnStyle    = lipgloss.NewStyle().Foreground(tui.ColDim)
	taskPendingStyle = lipgloss.NewStyle().Foreground(tui.ColMuted)
	taskActiveStyle  = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	taskDoneStyle    = lipgloss.NewStyle().Foreground(tui.ColDim).Strikethrough(true)
)

// maxTaskRows caps how many task rows the live block shows before collapsing
// the tail into a "└ N more" line. List() orders in-progress → pending →
// completed, so the rows we keep are always the most pressing ones.
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

	rows := items
	moreCount := 0
	if len(rows) > maxTaskRows {
		// Reserve the last visible line for the "N more" marker.
		moreCount = len(rows) - (maxTaskRows - 1)
		rows = rows[:maxTaskRows-1]
	}

	var b strings.Builder
	for i, t := range rows {
		// The first row carries the tree elbow tying the block to the spinner
		// above; the rest align their glyph under it.
		if i == 0 {
			b.WriteString(taskConnStyle.Render("└ "))
		} else {
			b.WriteString("\n  ")
		}
		b.WriteString(renderTaskRow(t, width))
	}
	if moreCount > 0 {
		b.WriteString("\n  ")
		b.WriteString(taskConnStyle.Render(fmt.Sprintf("… %d more", moreCount)))
	}
	return b.String()
}

// renderTaskRow formats a single task line: glyph + label, styled by status,
// truncated to fit the terminal width (the 2-column row prefix is accounted
// for by the caller's layout, so we budget width-2 here).
func renderTaskRow(t tasks.Task, width int) string {
	var glyph string
	var style lipgloss.Style
	text := t.Subject
	switch t.Status {
	case tasks.InProgress:
		glyph, style = "□", taskActiveStyle
		if t.ActiveForm != "" {
			text = t.ActiveForm // present-continuous form reads better while active
		}
	case tasks.Completed:
		glyph, style = "✓", taskDoneStyle
	default: // Pending
		glyph, style = "□", taskPendingStyle
	}

	line := glyph + " " + text
	if width > 6 {
		line = truncate.StringWithTail(line, uint(width-2), "…")
	}
	return style.Render(line)
}
