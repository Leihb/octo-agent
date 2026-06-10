package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// blockingToolSender drives a two-round tool turn and blocks at the start of
// each round until released: round 1 so the test can steer a message into the
// running turn, round 2 as the deterministic mid-turn point to observe what
// doAgentTurn has persisted so far. Rounds beyond 2 (title/suggestion
// side-calls) return immediately.
type blockingToolSender struct {
	mu       sync.Mutex
	round    int
	entered1 chan struct{}
	release1 chan struct{}
	entered2 chan struct{}
	release2 chan struct{}
}

func (s *blockingToolSender) SendMessages(_ context.Context, _, _ string, _ []agent.Message, _ int) (agent.Reply, error) {
	return agent.Reply{Content: "side-call"}, nil
}

func (s *blockingToolSender) StreamMessages(_ context.Context, _, _ string, _ []agent.Message, _ int, onChunk func(string), _ func(string)) (agent.Reply, error) {
	return agent.Reply{Content: "side-call"}, nil
}

func (s *blockingToolSender) SendMessagesWithTools(_ context.Context, _, _ string, _ []agent.Message, _ int, _ []agent.ToolDefinition) (agent.Reply, error) {
	return agent.Reply{Content: "side-call"}, nil
}

func (s *blockingToolSender) StreamMessagesWithTools(_ context.Context, _, _ string, _ []agent.Message, _ int, _ []agent.ToolDefinition, onChunk func(string), _ agent.ToolInputDeltaFunc, _ agent.ThinkingDeltaFunc) (agent.Reply, error) {
	s.mu.Lock()
	s.round++
	round := s.round
	s.mu.Unlock()
	switch round {
	case 1:
		close(s.entered1)
		<-s.release1
		// A read of a nonexistent path: allowed by the embedded permission
		// defaults (no gate prompt) and fails fast with an error tool_result,
		// which keeps the loop running into round 2 — no filesystem side
		// effects.
		return agent.Reply{
			Blocks: []agent.ContentBlock{
				agent.NewToolUseBlock("tu1", "read_file", map[string]any{"path": "/nonexistent/incremental-persist-test"}),
			},
			StopReason: "tool_use",
		}, nil
	case 2:
		close(s.entered2)
		<-s.release2
		if onChunk != nil {
			onChunk("round two")
		}
		return agent.Reply{Content: "round two"}, nil
	default:
		return agent.Reply{Content: "side-call"}, nil
	}
}

// hasBlock reports whether the message carries a block of the given type.
func hasBlock(m agent.Message, blockType string) bool {
	for _, b := range m.Blocks {
		if b.Type == blockType {
			return true
		}
	}
	return false
}

// TestDoAgentTurn_PersistsProgressIncrementally is the crash-durability
// guard: by the time the second LLM round is running, the first round's
// tool_use, its tool_result, and the steer message that arrived mid-turn must
// already be on disk — a server killed at that point loses at most the round
// in flight, not the whole turn. It also checks the watermark cap end-to-end:
// a mid-turn history fetch must serve only messages from before the turn,
// because the WS replay buffer owns the turn's own events (without the cap
// every card renders twice on refresh).
func TestDoAgentTurn_PersistsProgressIncrementally(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sender := &blockingToolSender{
		entered1: make(chan struct{}),
		release1: make(chan struct{}),
		entered2: make(chan struct{}),
		release2: make(chan struct{}),
	}
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: true})
	srv.sender = sender
	srv.initWS()
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]agent.InboxItem)
	srv.sessionAgents = make(map[string]*agent.Agent)

	sess := agent.NewSession("stub-model", "")
	sess.Title = "fixed title" // suppress the async title-generation side-call
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.doAgentTurn(sess, "hello", nil, nil)
	}()
	releaseOnce := func(ch chan struct{}) {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	t.Cleanup(func() {
		releaseOnce(sender.release1)
		releaseOnce(sender.release2)
		<-done
	})

	// Round 1 is blocked inside the provider call: the Agent is registered
	// (that happens before RunStream) and the turn cannot finish yet. Steer a
	// message into its inbox — the loop drains it before round 2 — then let
	// round 1 return its tool call.
	select {
	case <-sender.entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never started")
	}
	srv.sessionAgentsMu.Lock()
	a := srv.sessionAgents[sess.ID]
	srv.sessionAgentsMu.Unlock()
	if a == nil {
		t.Fatal("agent not registered while round 1 in flight")
	}
	a.Inbox.Enqueue("steer message")
	releaseOnce(sender.release1)

	select {
	case <-sender.entered2:
	case <-time.After(5 * time.Second):
		t.Fatal("round 2 never started")
	}

	// ── Mid-turn: round 2 is blocked inside the provider call. ──

	onDisk, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("mid-turn load: %v", err)
	}
	if len(onDisk.Messages) != 4 {
		t.Fatalf("mid-turn persisted %d messages, want 4 (user, tool_use, tool_result, steer): %+v",
			len(onDisk.Messages), onDisk.Messages)
	}
	if onDisk.Messages[0].Content != "hello" {
		t.Errorf("persisted[0] = %q, want the user message", onDisk.Messages[0].Content)
	}
	if !hasBlock(onDisk.Messages[1], "tool_use") {
		t.Errorf("persisted[1] lacks the round-1 tool_use block: %+v", onDisk.Messages[1])
	}
	if !hasBlock(onDisk.Messages[2], "tool_result") {
		t.Errorf("persisted[2] lacks the tool_result block: %+v", onDisk.Messages[2])
	}
	if onDisk.Messages[3].Content != "steer message" {
		t.Errorf("persisted[3] = %q, want the steer message", onDisk.Messages[3].Content)
	}

	// The history endpoint must cap at the watermark (the turn's user
	// message): the incrementally-persisted rounds belong to the WS replay
	// buffer until the turn ends.
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	req.SetPathValue("id", sess.ID)
	rec := httptest.NewRecorder()
	srv.handleGetSessionMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages endpoint = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, ev := range body.Events {
		switch ev["type"] {
		case "tool_call", "tool_result", "assistant_message":
			t.Fatalf("mid-turn history served in-flight turn content: %v (events=%v)", ev, body.Events)
		}
	}

	// ── Finish the turn: the full transcript persists and the cap lifts. ──

	releaseOnce(sender.release2)
	<-done

	final, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if n := len(final.Messages); n != 5 {
		t.Fatalf("final persisted %d messages, want 5 (… + final assistant reply): %+v", n, final.Messages)
	}
	if final.Messages[4].Content != "round two" {
		t.Errorf("final reply = %q, want \"round two\"", final.Messages[4].Content)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/messages", nil)
	req2.SetPathValue("id", sess.ID)
	srv.handleGetSessionMessages(rec2, req2)
	var body2 struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("unmarshal final: %v", err)
	}
	toolCalls := 0
	for _, ev := range body2.Events {
		if ev["type"] == "tool_call" {
			toolCalls++
		}
	}
	if toolCalls != 1 {
		t.Fatalf("post-turn history tool_call count = %d, want 1 (cap must lift)", toolCalls)
	}
}

// TestHandleEvent_CompactDoneShiftsWatermark checks the watermark arithmetic
// for mid-turn compaction: folding K leading messages into one summary moves
// the pre-turn boundary to W-K+1, and folding past the boundary clamps to 1
// (only the summary predates the turn). No-op compactions (before == after)
// must leave the watermark alone.
func TestHandleEvent_CompactDoneShiftsWatermark(t *testing.T) {
	cases := []struct {
		name          string
		watermark     int
		stats         *agent.CompactStats
		wantWatermark int
	}{
		{"folds within pre-turn prefix", 10, &agent.CompactStats{BeforeTokens: 100, AfterTokens: 40, FoldedMsgs: 6}, 5},
		{"folds into the current turn", 3, &agent.CompactStats{BeforeTokens: 100, AfterTokens: 40, FoldedMsgs: 8}, 1},
		{"no-op compaction", 10, &agent.CompactStats{BeforeTokens: 100, AfterTokens: 100, FoldedMsgs: 6}, 10},
		{"nil stats", 10, nil, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
			srv.initWS()
			const sid = "compact-watermark-session"
			srv.liveStateMu.Lock()
			srv.liveStates[sid] = &sessionLiveState{
				progress:         &wsEventProgress{Type: "progress", Phase: "active"},
				historyWatermark: tc.watermark,
			}
			srv.liveStateMu.Unlock()

			sw := srv.newWSStreamWriter(sid)
			sw.handleEvent(agent.AgentEvent{Kind: agent.EventCompactDone, Compact: tc.stats})

			srv.liveStateMu.RLock()
			got := srv.liveStates[sid].historyWatermark
			srv.liveStateMu.RUnlock()
			if got != tc.wantWatermark {
				t.Errorf("watermark = %d, want %d", got, tc.wantWatermark)
			}
		})
	}
}
