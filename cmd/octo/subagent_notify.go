package main

import (
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/tools"
)

// formatSubAgentNote renders a sub-agent completion notification as a
// <system-reminder> block. It rides the existing steer path (Agent.Steer →
// folded into the next tool_result, or prepended to the next turn — see
// turncore.go), so the model reads it as an environment event rather than
// user speech.
func formatSubAgentNote(ev tools.SubAgentNotification) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	switch ev.Kind {
	case "spawn_done":
		fmt.Fprintf(&b, "Sub-agent %s (%s) has completed.", ev.AgentID, ev.Description)
	case "message_reply":
		fmt.Fprintf(&b, "Sub-agent %s (%s) has replied to your message.", ev.AgentID, ev.Description)
	default:
		fmt.Fprintf(&b, "Sub-agent %s (%s) update: %s", ev.AgentID, ev.Description, ev.Kind)
	}
	if ev.Result != "" {
		b.WriteString("\nResult:\n")
		b.WriteString(ev.Result)
	}
	if ev.InputTokens > 0 || ev.OutputTokens > 0 {
		fmt.Fprintf(&b, "\n[usage] in %d / out %d", ev.InputTokens, ev.OutputTokens)
	}
	b.WriteString("\n</system-reminder>")
	return b.String()
}
