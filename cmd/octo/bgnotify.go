package main

import (
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/tools"
)

// formatBgNote renders a background-process completion as a plain user-facing
// message. The text is meant to be submitted as a standalone turn (queued or
// auto-triggered) so the model treats it as a first-class inbox item, not a
// hidden system-reminder block folded into another message.
func formatBgNote(e tools.BgExit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Background process %s (`%s`) %s.", e.ID, e.Command, e.Status)
	if out := strings.TrimRight(e.NewOutput, "\n"); out != "" {
		b.WriteString("\nOutput since last check:\n")
		b.WriteString(out)
	} else {
		b.WriteString("\n(no new output)")
	}
	return b.String()
}
