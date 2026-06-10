package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

// midTurnSender simulates a user message arriving while the LLM call is in
// flight: its first round invokes the injected callback (which feeds the real
// handleWSUserMessage), then answers like the stub. Guarded by a mutex — the
// after-turn suggestion goroutine calls the sender concurrently.
type midTurnSender struct {
	inject func()
	mu     sync.Mutex
	rounds int
}

func (s *midTurnSender) SendMessages(_ context.Context, _, _ string, _ []agent.Message, _ int) (agent.Reply, error) {
	s.mu.Lock()
	s.rounds++
	first := s.rounds == 1
	s.mu.Unlock()
	if first && s.inject != nil {
		s.inject()
	}
	return agent.Reply{Content: "stub reply"}, nil
}

// TestMidTurnSteer_DeliveredOnceWithImage guards two behaviours of the
// mid-turn (steer) path:
//
//  1. Single delivery. A steer message used to be enqueued into BOTH the
//     server steerQueues and the running Agent's Inbox, so it was answered
//     and persisted twice (once by the post-turn leftover drain, once by the
//     chained turn).
//  2. Image attachments ride the Inbox as real content blocks
//     (EnqueueWithBlocks), exactly like the TUI's mid-turn paste — not as a
//     degraded text note.
func TestMidTurnSteer_DeliveredOnceWithImage(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.initWS()
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]agent.InboxItem)
	srv.sessionAgents = make(map[string]*agent.Agent)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	payload := []byte{0xFF, 0xD8, 0xFF, 7, 7}
	srv.sender = &midTurnSender{inject: func() {
		srv.handleWSUserMessage(nil, &wsMsgUserMessage{
			SessionID: sess.ID,
			Content:   json.RawMessage(`"steer-msg"`),
			Files: []wsUserFile{
				{Name: "mid.jpg", MimeType: "image/jpeg", DataURL: jpegDataURL(payload)},
			},
		})
	}}

	// Mirror handleWSUserMessage's idle branch: mark the turn running, then
	// run the loop (the injected steer arrives during round 1's LLM call).
	srv.turnRunning[sess.ID] = true
	srv.runAgentTurnLoop(sess, "first-msg", nil, nil)

	loaded, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	textCount, imageCount := 0, 0
	for i, m := range loaded.Messages {
		t.Logf("msg[%d] role=%s content=%q blocks=%d", i, m.Role, m.Content, len(m.Blocks))
		if m.Role != agent.RoleUser {
			continue
		}
		if m.Content == "steer-msg" {
			textCount++
		}
		for _, b := range m.Blocks {
			if b.Type == "text" && b.Text == "steer-msg" {
				textCount++
			}
			if b.Type == "image" && b.ImagePath != "" {
				imageCount++
			}
		}
	}
	if textCount != 1 {
		t.Errorf("steer-msg appears %d times in transcript, want 1", textCount)
	}
	if imageCount != 1 {
		t.Errorf("steer image block appears %d times in transcript, want 1", imageCount)
	}
}
