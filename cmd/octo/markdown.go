package main

import (
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/Leihb/octo-agent/internal/tui/themes"
)

// markdownRenderer wraps a glamour TermRenderer, lazily (re)built when the wrap
// width changes. Rendering is best-effort: on any error (or an unbuildable
// renderer) it returns the input unchanged, so a turn never breaks over a
// formatting glitch.
type markdownRenderer struct {
	width int
	style string // "dark" or "light"; empty defaults to dark
	r     *glamour.TermRenderer
}

// render styles a complete markdown fragment for the terminal at the given
// wrap width. Empty / whitespace-only input renders to "".
func (m *markdownRenderer) render(src string, width int) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	w := width
	if w <= 0 {
		w = 80
	}
	if m.r == nil || m.width != w {
		style := m.style
		if style == "" {
			style = "dark"
		}
		styleBytes := themes.GetStyle(style)
		r, err := glamour.NewTermRenderer(
			glamour.WithStylesFromJSONBytes(styleBytes),
			glamour.WithWordWrap(w),
		)
		if err != nil {
			return strings.TrimRight(src, "\n")
		}
		m.r, m.width = r, w
	}
	out, err := m.r.Render(src)
	if err != nil {
		return strings.TrimRight(src, "\n")
	}
	return strings.TrimRight(out, "\n")
}

// splitCommittableMarkdown splits a streamed assistant buffer into the prefix
// that is safe to render now — everything up to and including the last blank
// line that sits outside a fenced code block — and the trailing remainder
// still being streamed. Returns ("", buf) when no safe boundary exists yet, so
// glamour only ever renders whole blocks (never half a paragraph or an open
// code fence, which it renders badly).
func splitCommittableMarkdown(buf string) (commit, rest string) {
	inFence := false
	offset := 0
	lastBoundary := -1
	for _, ln := range strings.SplitAfter(buf, "\n") {
		if ln == "" {
			continue // trailing element after a final "\n"
		}
		body := strings.TrimSpace(strings.TrimRight(ln, "\n"))
		if strings.HasPrefix(body, "```") {
			inFence = !inFence
		}
		offset += len(ln)
		// A blank line (terminated by \n) outside a fence ends a block.
		if !inFence && body == "" && strings.HasSuffix(ln, "\n") {
			lastBoundary = offset
		}
	}
	if lastBoundary < 0 {
		return "", buf
	}
	return buf[:lastBoundary], buf[lastBoundary:]
}
