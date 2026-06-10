package agent

import "testing"

// TestSessionEndsMidTurn pins the crash signature: a transcript ending on a
// user message or an unanswered assistant tool_use means the process died
// mid-turn, while every clean ending (finished reply, interrupt note) is a
// plain assistant text message.
func TestSessionEndsMidTurn(t *testing.T) {
	toolUse := Message{Role: RoleAssistant, Blocks: []ContentBlock{
		NewToolUseBlock("tu1", "terminal", map[string]any{"command": "ls"}),
	}}
	toolResult := NewToolResultMessage([]ContentBlock{
		NewToolResultBlock("tu1", "file.txt", false),
	})

	cases := []struct {
		name     string
		messages []Message
		want     bool
	}{
		{"empty session", nil, false},
		{"ends on initiating user message", []Message{NewUserMessage("hi")}, true},
		{"ends on tool_result batch", []Message{NewUserMessage("hi"), toolUse, toolResult}, true},
		{"ends on assistant tool_use", []Message{NewUserMessage("hi"), toolUse}, true},
		{"ends on finished reply", []Message{NewUserMessage("hi"), NewAssistantMessage("done")}, false},
		{"ends on interrupt note", []Message{NewUserMessage("hi"), toolUse, toolResult, NewAssistantMessage("[interrupted]")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Messages: tc.messages}
			if got := s.EndsMidTurn(); got != tc.want {
				t.Errorf("EndsMidTurn() = %v, want %v", got, tc.want)
			}
		})
	}
}
