package main

import (
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/tools"
)

// formatSubAgentNote renders a sub-agent completion notification as a plain
// user-facing message. The text is meant to be submitted as a standalone turn
// (queued or auto-triggered) so the model treats it as a first-class inbox
// item, not a hidden system-reminder block folded into another message.
func formatSubAgentNote(ev tools.SubAgentNotification) string {
	var b strings.Builder
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
	return b.String()
}
