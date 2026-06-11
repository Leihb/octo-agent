package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Palette — adaptive so cards, panels, and chrome read on both light and dark
// terminals. Values follow GitHub's light/dark themes; lipgloss resolves the
// right side at render time from the terminal-background probe.
var (
	ColAccent = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"} // success / additions
	ColDanger = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"} // errors / removals
	ColMuted  = lipgloss.AdaptiveColor{Light: "#57606A", Dark: "#8B949E"} // secondary text
	ColDim    = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#6E7681"} // gutters, line numbers
	ColDimmer = lipgloss.AdaptiveColor{Light: "#AFB8C1", Dark: "#484F58"} // dimmest (unchanged line nos)
	ColBorder = lipgloss.AdaptiveColor{Light: "#D0D7DE", Dark: "#30363D"} // panel / input-box border

	ColBrand    = lipgloss.AdaptiveColor{Light: "#6F42C1", Dark: "#A371F7"} // purple — octo brand
	ColBrandDim = lipgloss.AdaptiveColor{Light: "#B4A7D6", Dark: "#6E5494"} // muted purple
	ColUserMsg  = lipgloss.AdaptiveColor{Light: "#0969DA", Dark: "#58A6FF"} // blue — user messages
)

func IsDark() bool { return lipgloss.HasDarkBackground() }

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

// ── Banner ─────────────────────────────────────────────────────────────────

var (
	bannerStyle = lipgloss.NewStyle().
			Foreground(ColBrand).
			Bold(true)
	bannerSubStyle = lipgloss.NewStyle().
			Foreground(ColMuted)
	bannerSepStyle = lipgloss.NewStyle().
			Foreground(ColBorder)
	bannerHintStyle = lipgloss.NewStyle().
			Foreground(ColDimmer).
			Italic(true)
)

// bannerHints are the key bindings shown beside the mascot at startup, so the
// bottom status bar can stay free of a persistent hint line.
var bannerHints = []string{
	"Enter send · Shift+Enter/Ctrl+J newline · Ctrl+Q queue",
	"Shift+Tab perm-mode · Ctrl+T tasks · Esc interrupt · Ctrl+C quit",
}

// octopusASCII is the pixel-art octo mascot — a rounded octopus with a domed
// head, two eyes, and eight tentacles fanning out to both sides with curled
// tips. It renders in the terminal's default foreground colour.
var octopusASCII = []string{
	"      ▄████▄",
	"    ▟████████▙",
	"    ██ ●  ● ██",
	"    ▜████████▛",
	"  ╲ ╲ ╲ ╲ ╱ ╱ ╱ ╱",
	"  ◟ ◟ ◟ ◟ ◞ ◞ ◞ ◞",
}

// octoArtWidth is the column the banner text starts at — wide enough to clear
// the widest art line plus a two-space gutter, so the title/info/hints align.
const octoArtWidth = 19

// BannerHeight is the number of lines Banner renders (including the separator).
const BannerHeight = 7

// Banner renders the octo welcome header with pixel-art mascot.
// The icon sits left of the title, Claude Code style.
func Banner(version, model, cwd string, width int) string {
	if width < 20 {
		width = 20
	}

	// Build text lines
	title := "◆ octo"
	if version != "" {
		title += "  " + version
	}

	info := model
	if cwd != "" {
		if info != "" {
			info += " · "
		}
		info += cwd
	}

	// Merge art + text side-by-side. The mascot renders in the terminal's
	// default colour; each text line is pushed to a fixed column so the
	// title/info/hints align past the variable-width art.
	var b strings.Builder
	for i, artLine := range octopusASCII {
		b.WriteString(artLine)
		gap := octoArtWidth - lipgloss.Width(artLine)
		if gap < 2 {
			gap = 2
		}
		pad := strings.Repeat(" ", gap)
		switch i {
		case 1:
			b.WriteString(pad)
			b.WriteString(bannerStyle.Render(title))
		case 2:
			b.WriteString(pad)
			b.WriteString(bannerSubStyle.Render(info))
		case 3:
			b.WriteString(pad)
			b.WriteString(bannerHintStyle.Render(bannerHints[0]))
		case 4:
			b.WriteString(pad)
			b.WriteString(bannerHintStyle.Render(bannerHints[1]))
		}
		b.WriteByte('\n')
	}

	sep := strings.Repeat("─", width)
	b.WriteString(bannerSepStyle.Render(sep))
	return b.String()
}

// ── Status bar ─────────────────────────────────────────────────────────────

var (
	statusSegStyle   = lipgloss.NewStyle().Foreground(ColMuted)
	statusHintStyle  = lipgloss.NewStyle().Foreground(ColDimmer).Italic(true)
	statusBarStyle   = lipgloss.NewStyle().Foreground(ColBorder)
	statusModelStyle = lipgloss.NewStyle().Foreground(ColBrandDim).Bold(true)
	statusTimeStyle  = lipgloss.NewStyle().Foreground(ColMuted)
	statusShellStyle = lipgloss.NewStyle().Foreground(ColAccent)
)

// StatusBar renders the bottom status line with styled segments.
// Each segment is a [label, value] pair; the hint is shown on a second line.
func StatusBar(segments [][2]string, hint string, width int) string {
	var b strings.Builder
	if width > 3 {
		b.WriteString(statusBarStyle.Render(strings.Repeat("─", width)))
		b.WriteByte('\n')
	}
	for i, seg := range segments {
		if i > 0 {
			b.WriteString(statusSegStyle.Render(" · "))
		}
		label, value := seg[0], seg[1]
		var rendered string
		switch label {
		case "model":
			rendered = statusModelStyle.Render(value)
		case "elapsed":
			rendered = statusTimeStyle.Render(value)
		case "shell":
			rendered = statusShellStyle.Render(value)
		default:
			rendered = statusSegStyle.Render(value)
		}
		b.WriteString(rendered)
	}
	if hint != "" {
		b.WriteByte('\n')
		b.WriteString(statusHintStyle.Render(hint))
	}
	return b.String()
}
