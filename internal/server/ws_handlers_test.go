package server

import (
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

// lastVisibleUserIdx must land on the typed prompt, not the tool_result
// carrier an agentic turn leaves as its most recent user-role message.
func TestLastVisibleUserIdx_SkipsToolResultCarriers(t *testing.T) {
	msgs := []agent.Message{
		agent.NewUserMessage("fix the bug"), // 0 — the real prompt
		agent.NewToolUseMessage([]agent.ContentBlock{
			agent.NewToolUseBlock("c1", "terminal", map[string]any{"command": "ls"}),
		}), // 1
		agent.NewToolResultMessage([]agent.ContentBlock{
			agent.NewToolResultBlock("c1", "file1", false),
		}), // 2 — user-role carrier, must be skipped
		agent.NewAssistantMessage("done"), // 3
	}
	if got := lastVisibleUserIdx(msgs); got != 0 {
		t.Errorf("lastVisibleUserIdx = %d, want 0 (the typed prompt)", got)
	}
}

func TestLastVisibleUserIdx_SkipsReminderOnly(t *testing.T) {
	msgs := []agent.Message{
		agent.NewUserMessage("real prompt"), // 0
		agent.NewAssistantMessage("ok"),     // 1
		agent.NewUserMessage("<system-reminder>background process exited</system-reminder>"), // 2 — model-facing only
		agent.NewAssistantMessage("noted"),                                                   // 3
	}
	if got := lastVisibleUserIdx(msgs); got != 0 {
		t.Errorf("lastVisibleUserIdx = %d, want 0 (reminder-only messages are not retryable prompts)", got)
	}
}

func TestLastVisibleUserIdx_None(t *testing.T) {
	if got := lastVisibleUserIdx(nil); got != -1 {
		t.Errorf("empty history: got %d, want -1", got)
	}
	onlyAssistant := []agent.Message{agent.NewAssistantMessage("hi")}
	if got := lastVisibleUserIdx(onlyAssistant); got != -1 {
		t.Errorf("no user messages: got %d, want -1", got)
	}
}

// An image-only user message (no text) is still a retryable prompt.
func TestLastVisibleUserIdx_ImageOnly(t *testing.T) {
	img := agent.NewUserMessage("")
	img.Blocks = []agent.ContentBlock{agent.NewImageBlock("image/png", []byte("hi"))}
	msgs := []agent.Message{
		agent.NewUserMessage("first"), // 0
		agent.NewAssistantMessage("ok"),
		img, // 2
		agent.NewAssistantMessage("nice picture"),
	}
	if got := lastVisibleUserIdx(msgs); got != 2 {
		t.Errorf("lastVisibleUserIdx = %d, want 2 (image-only prompt)", got)
	}
}
