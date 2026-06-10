package tools

import (
	"fmt"
	"strings"
)

// FormatBgNote renders a background-process completion as a <system-reminder>
// block. It rides the steer path of whichever frontend wires it (CLI/TUI:
// Inbox.Enqueue; server: Inbox or steer queue; IM: the session agent's
// Inbox), so the model reads it as an environment event rather than user
// speech. Wrapping in <system-reminder> matches octo's convention for
// injected, non-user context — UIs strip these spans from user-visible text.
func FormatBgNote(e BgExit) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString("[BACKGROUND COMPLETED]\n")
	fmt.Fprintf(&b, "Background process %s (`%s`) %s.", e.ID, e.Command, e.Status)
	if out := strings.TrimRight(e.NewOutput, "\n"); out != "" {
		b.WriteString("\nOutput since last check:\n")
		// A long-running build/test that finished in the background can emit
		// far more than fits a single notice — spill it to a temp file and
		// show a head+tail preview, same as the synchronous terminal path.
		b.WriteString(MaybeSpillOutput(e.ID, out))
	} else {
		b.WriteString("\n(no new output)")
	}
	b.WriteString("\n</system-reminder>")
	return b.String()
}
