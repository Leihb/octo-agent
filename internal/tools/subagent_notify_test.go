package tools

import (
	"strings"
	"testing"
)

// TestFormatSubAgentNote_MaxTurnsFlagged verifies an async sub-agent that hit
// its turn limit is flagged INCOMPLETE in the completion notice, so the parent
// doesn't treat partial work as a finished answer.
func TestFormatSubAgentNote_MaxTurnsFlagged(t *testing.T) {
	got := FormatSubAgentNote(SubAgentNotification{
		AgentID:     "agent_1",
		Description: "investigate",
		Kind:        "spawn_done",
		Result:      "partial findings",
		StopReason:  "max_turns",
	})
	if !strings.Contains(got, "INCOMPLETE") {
		t.Errorf("max_turns notice should be flagged INCOMPLETE, got:\n%s", got)
	}

	// Normal completion is not flagged.
	clean := FormatSubAgentNote(SubAgentNotification{
		AgentID: "agent_2", Description: "x", Kind: "spawn_done", Result: "done",
	})
	if strings.Contains(clean, "INCOMPLETE") {
		t.Errorf("a complete notice should not be flagged, got:\n%s", clean)
	}
}
