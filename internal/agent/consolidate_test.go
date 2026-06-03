package agent

import (
	"context"
	"strings"
	"testing"
)

// stubExtractSender returns a canned reply for side-calls (consolidation,
// planning). It captures the system prompt and messages so tests can assert
// which side-call fired and what it was given.
type stubExtractSender struct {
	reply        string
	err          error
	lastSystem   string
	lastMessages []Message
}

func (s *stubExtractSender) SendMessages(_ context.Context, _, system string, msgs []Message, _ int) (Reply, error) {
	s.lastSystem = system
	s.lastMessages = msgs
	if s.err != nil {
		return Reply{}, s.err
	}
	return Reply{Content: s.reply, InputTokens: 10, OutputTokens: 20}, nil
}

func (s *stubExtractSender) StreamMessages(_ context.Context, _, _ string, _ []Message, _ int, _ func(string), _ func(string)) (Reply, error) {
	return s.SendMessages(context.Background(), "", "", nil, 0)
}

func TestConsolidateMemory(t *testing.T) {
	s := &stubExtractSender{reply: "Consolidated summary."}
	a := New(s, "test-model")
	out, err := a.ConsolidateMemory(context.Background(), "", "- [user] prefers Go")
	if err != nil || out != "Consolidated summary." {
		t.Errorf("ConsolidateMemory out=%q err=%v", out, err)
	}
	if !strings.Contains(s.lastSystem, "summary") {
		t.Errorf("should use consolidate system prompt, got %q", s.lastSystem)
	}
	// Both arguments empty → short-circuit to empty, no call.
	s.lastMessages = nil
	out, _ = a.ConsolidateMemory(context.Background(), "", "")
	if out != "" || s.lastMessages != nil {
		t.Errorf("empty inputs should short-circuit; out=%q msgs=%v", out, s.lastMessages)
	}
}

func TestConsolidateMemory_IncrementalPrompt(t *testing.T) {
	s := &stubExtractSender{reply: "merged"}
	a := New(s, "test-model")
	if _, err := a.ConsolidateMemory(context.Background(), "PRIOR_SUMMARY_XYZ", "NEW_NOTE_ABC"); err != nil {
		t.Fatal(err)
	}
	if len(s.lastMessages) == 0 {
		t.Fatal("expected a SendMessages call")
	}
	prompt := s.lastMessages[0].Content
	if !strings.Contains(prompt, "PRIOR_SUMMARY_XYZ") || !strings.Contains(prompt, "NEW_NOTE_ABC") {
		t.Errorf("prompt should carry both prior summary and new notes:\n%s", prompt)
	}
}
