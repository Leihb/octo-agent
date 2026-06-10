package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

// msgRecordingSender captures the message lists it is asked to stream so a test
// can inspect exactly what the model would have seen.
type msgRecordingSender struct {
	mu    sync.Mutex
	calls [][]agent.Message
}

func (s *msgRecordingSender) record(msgs []agent.Message) {
	cp := make([]agent.Message, len(msgs))
	copy(cp, msgs)
	s.mu.Lock()
	s.calls = append(s.calls, cp)
	s.mu.Unlock()
}

func (s *msgRecordingSender) SendMessages(_ context.Context, _, _ string, msgs []agent.Message, _ int) (agent.Reply, error) {
	s.record(msgs)
	return agent.Reply{Content: "ok"}, nil
}

func (s *msgRecordingSender) StreamMessages(_ context.Context, _, _ string, msgs []agent.Message, _ int, onChunk func(string), _ func(string)) (agent.Reply, error) {
	s.record(msgs)
	if onChunk != nil {
		onChunk("ok")
	}
	return agent.Reply{Content: "ok"}, nil
}

// lastUserContent returns the text of the last user message of the first
// recorded provider call.
func (s *msgRecordingSender) lastUserContent(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("provider was never called")
	}
	msgs := s.calls[0]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agent.RoleUser {
			if msgs[i].Content != "" {
				return msgs[i].Content
			}
			for _, b := range msgs[i].Blocks {
				if b.Type == "text" {
					return b.Text
				}
			}
		}
	}
	t.Fatal("no user message reached the provider")
	return ""
}

// newCrashReminderServer builds the minimal turn-capable server used by the
// crash-reminder tests.
func newCrashReminderServer(t *testing.T, sender *msgRecordingSender) *Server {
	t.Helper()
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.sender = sender
	srv.initWS()
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]agent.InboxItem)
	srv.sessionAgents = make(map[string]*agent.Agent)
	return srv
}

// TestDoAgentTurn_CrashRecoveryReminder: a session whose transcript ends
// mid-turn (the previous turn died with the server) must warn the model on
// the next turn that tool side effects may be unrecorded — without the
// reminder ever reaching a UI surface.
func TestDoAgentTurn_CrashRecoveryReminder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sender := &msgRecordingSender{}
	srv := newCrashReminderServer(t, sender)

	// A crashed turn's persisted tail: the user asked, the assistant called a
	// tool, and the process died before the results landed.
	sess := agent.NewSession("stub-model", "")
	sess.Title = "fixed title"
	sess.Messages = []agent.Message{
		agent.NewUserMessage("delete the old backups"),
		{Role: agent.RoleAssistant, Blocks: []agent.ContentBlock{
			agent.NewToolUseBlock("tu1", "terminal", map[string]any{"command": "rm -r backups/"}),
		}},
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv.doAgentTurn(sess, "did it work?", nil, nil)

	got := sender.lastUserContent(t)
	if !strings.Contains(got, "<system-reminder>") || !strings.Contains(got, "ended abnormally") {
		t.Errorf("model did not receive the crash-recovery reminder; user content = %q", got)
	}
	if !strings.Contains(got, "did it work?") {
		t.Errorf("reminder displaced the user's own text; user content = %q", got)
	}

	// The reminder is model-facing only: the history endpoint must strip it.
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	req.SetPathValue("id", sess.ID)
	rec := httptest.NewRecorder()
	srv.handleGetSessionMessages(rec, req)
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, ev := range body.Events {
		if c, _ := ev["content"].(string); strings.Contains(c, "system-reminder") || strings.Contains(c, "ended abnormally") {
			t.Errorf("reminder leaked into a UI surface: %v", ev)
		}
	}
}

// TestDoAgentTurn_NoReminderAfterCleanTurn: a transcript ending on a plain
// assistant reply (finished turn, or finishInterrupted's note after a manual
// interrupt) must not trigger the reminder.
func TestDoAgentTurn_NoReminderAfterCleanTurn(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sender := &msgRecordingSender{}
	srv := newCrashReminderServer(t, sender)

	sess := agent.NewSession("stub-model", "")
	sess.Title = "fixed title"
	sess.Messages = []agent.Message{
		agent.NewUserMessage("hi"),
		agent.NewAssistantMessage("hello!"),
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv.doAgentTurn(sess, "next question", nil, nil)

	got := sender.lastUserContent(t)
	if strings.Contains(got, "system-reminder") {
		t.Errorf("reminder fired after a cleanly finished turn; user content = %q", got)
	}
}
