package main

import (
	"fmt"
	"strings"
)

// complItem is one row in the slash-command completion menu.
type complItem struct {
	name string // the trigger, e.g. "/skills" or "/review"
	desc string // one-line description shown to the right
}

// maxComplVisible caps how many completion rows render at once; the list
// scrolls around the selection beyond that.
const maxComplVisible = 10

// builtinSlashCommands are the always-available TUI slash commands, in display
// order. Skills are appended after these by slashCandidates.
var builtinSlashCommands = []complItem{
	{"/help", "List commands and tools"},
	{"/skills", "List discovered skills"},
	{"/mcp", "List connected MCP servers"},
	{"/memory", "Show cross-session memory"},
	{"/conduct", "Conduct a goal to completion, unattended"},
	{"/init", "Generate a .octorules guide for this repo"},
	{"/save", "Save the current session"},
	{"/sessions", "List recent sessions"},
	{"/exit", "Quit octo"},
}

// slashCandidates returns every completable slash command: the built-ins plus
// one "/<name>" entry per discovered skill.
func (m *tuiModel) slashCandidates() []complItem {
	items := make([]complItem, 0, len(builtinSlashCommands)+16)
	items = append(items, builtinSlashCommands...)
	if m.cfg.skillReg != nil {
		for _, s := range m.cfg.skillReg.List() {
			items = append(items, complItem{name: "/" + s.Name, desc: s.Description})
		}
	}
	return items
}

// updateCompletion (re)computes the menu from the current input. It opens the
// menu when the input is a bare "/command" token (a leading slash, no
// whitespace yet) and the session is idle; otherwise it closes the menu.
// complIdx is clamped but not reset here — callers reset it on a fresh edit.
func (m *tuiModel) updateCompletion() {
	v := m.ta.Value()
	if m.turnRunning || !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t\n") {
		m.complItems = nil
		return
	}
	prefix := strings.ToLower(v)
	var matches []complItem
	for _, c := range m.slashCandidates() {
		if strings.HasPrefix(strings.ToLower(c.name), prefix) {
			matches = append(matches, c)
		}
	}
	m.complItems = matches
	if m.complIdx >= len(matches) {
		m.complIdx = 0
	}
}

// acceptCompletion fills the input with the highlighted command plus a trailing
// space (so the user can type args or press Enter to run) and closes the menu.
func (m *tuiModel) acceptCompletion() {
	if m.complIdx < 0 || m.complIdx >= len(m.complItems) {
		return
	}
	m.ta.SetValue(m.complItems[m.complIdx].name + " ")
	m.ta.CursorEnd()
	m.complItems = nil
}

// complWindow returns the [start,end) slice of complItems to render, scrolled so
// the selection stays visible.
func (m *tuiModel) complWindow() (start, end int) {
	n := len(m.complItems)
	if n <= maxComplVisible {
		return 0, n
	}
	start = m.complIdx - maxComplVisible/2
	if start < 0 {
		start = 0
	}
	if start > n-maxComplVisible {
		start = n - maxComplVisible
	}
	return start, start + maxComplVisible
}

// completionHeight is the row count completionView occupies, so liveHeight can
// reserve space in the inline layout.
func (m *tuiModel) completionHeight() int {
	if len(m.complItems) == 0 {
		return 0
	}
	start, end := m.complWindow()
	return (end - start) + 1 // rows + the hint line
}

// completionView renders the menu: one row per candidate (name + description),
// the selection marked, and a key-hint footer.
func (m *tuiModel) completionView() string {
	if len(m.complItems) == 0 {
		return ""
	}
	start, end := m.complWindow()
	var b strings.Builder
	for i := start; i < end; i++ {
		it := m.complItems[i]
		name := fmt.Sprintf("%-20s", it.name)
		if i == m.complIdx {
			b.WriteString(complSelStyle.Render("▸ "+name) + " " + hintStyle.Render(truncate1Line(it.desc)))
		} else {
			b.WriteString("  " + complNameStyle.Render(name) + " " + hintStyle.Render(truncate1Line(it.desc)))
		}
		b.WriteByte('\n')
	}
	hint := "Tab/↑↓ select · Enter complete · Esc dismiss"
	if n := len(m.complItems); n > maxComplVisible {
		hint = fmt.Sprintf("…%d more · %s", n-maxComplVisible, hint)
	}
	b.WriteString(hintStyle.Render("  " + hint))
	b.WriteByte('\n')
	return b.String()
}
