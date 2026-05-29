package tui

import "github.com/charmbracelet/lipgloss"

// Palette is the shared TUI colour set. These mirror the github-dark-ish
// true-colour values the diff card already uses inline; a later phase swaps
// them for lipgloss.AdaptiveColor (light/dark aware) and folds the diff card's
// own inline styles in here too. Skeleton for now — one place to grow the
// theme as the TUI gains cards, a status bar, and panels.
var (
	ColAccent = lipgloss.Color("#3FB950") // success / additions
	ColDanger = lipgloss.Color("#F85149") // errors / removals
	ColMuted  = lipgloss.Color("#8B949E") // secondary text
	ColDim    = lipgloss.Color("#6E7681") // gutters, line numbers

	// ColBorder is the panel / input-box border colour. Adaptive so it reads
	// on both light and dark terminals (the true-colour diff/output cards stay
	// github-dark-tuned for now — re-theming those is a separate pass).
	ColBorder = lipgloss.AdaptiveColor{Light: "#D0D7DE", Dark: "#30363D"}
)

var (
	panelBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColBorder).
			Padding(0, 1)
	panelTitle = lipgloss.NewStyle().Foreground(ColMuted).Bold(true)
)

// Panel renders body inside a rounded border with a styled title line on top.
// An empty body yields a title-only box. The trailing newline is omitted.
func Panel(title, body string) string {
	inner := panelTitle.Render(title)
	if body != "" {
		inner += "\n" + body
	}
	return panelBorder.Render(inner)
}

// Box wraps content in the same rounded border as Panel, with no title styling
// — for content that supplies its own heading (e.g. a modal prompt).
func Box(content string) string {
	return panelBorder.Render(content)
}
