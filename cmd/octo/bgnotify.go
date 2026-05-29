package main

import (
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/tools"
)

// formatBgNote renders a background-process completion as a <system-reminder>
// block. It rides the existing steer path (Agent.Steer → folded into the next
// tool_result, or prepended to the next turn — see turncore.go), so the model
// reads it as an environment event rather than user speech. Wrapping in
// <system-reminder> matches octo's convention for injected, non-user context.
func formatBgNote(e tools.BgExit) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	fmt.Fprintf(&b, "Background process %s (`%s`) %s.", e.ID, e.Command, e.Status)
	if out := strings.TrimRight(e.NewOutput, "\n"); out != "" {
		b.WriteString("\nOutput since last check:\n")
		b.WriteString(out)
	} else {
		b.WriteString("\n(no new output)")
	}
	b.WriteString("\n</system-reminder>")
	return b.String()
}
