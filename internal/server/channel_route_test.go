package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/channel"
)

// fullFakeAdapter implements the whole channel.Adapter surface with no-ops,
// recording SendText calls — handleChannelMessage runs a real turn through
// the UIController, which touches typing/update methods too.
type fullFakeAdapter struct {
	mu   sync.Mutex
	sent []string
}

func (a *fullFakeAdapter) Platform() string { return "fake" }
func (a *fullFakeAdapter) Start(ctx context.Context, _ func(channel.InboundEvent)) error {
	<-ctx.Done()
	return nil
}
func (a *fullFakeAdapter) Stop() error { return nil }
func (a *fullFakeAdapter) SendText(chatID, text, replyTo string) channel.SendResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, text)
	return channel.SendResult{OK: true, MessageID: "f1"}
}
func (a *fullFakeAdapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	return channel.SendResult{OK: true}
}
func (a *fullFakeAdapter) UpdateMessage(chatID, messageID, text string) bool { return true }
func (a *fullFakeAdapter) SupportsMessageUpdates() bool                      { return false }
func (a *fullFakeAdapter) SendTyping(chatID, contextToken string) error      { return nil }
func (a *fullFakeAdapter) ValidateConfig(channel.PlatformConfig) []string    { return nil }

func (a *fullFakeAdapter) texts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.sent...)
}

// chanServer returns a mustServer with a channel manager wired to stub agents.
func chanServer(t *testing.T) *Server {
	t.Helper()
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.channelMgr = channel.NewManager(&channel.Config{}, func() *agent.Agent {
		return agent.New(&stubSender{}, "stub-model")
	}, channel.BindByChat)
	return srv
}

func evFor(text string) channel.InboundEvent {
	return channel.InboundEvent{Platform: "fake", ChatID: "c1", UserID: "u1", MessageID: "m1", Text: text}
}

// TestRouteChannelEvent_PendingAskConsumesMessage: while a permission prompt
// is pending, the next plain message answers it instead of starting a turn.
func TestRouteChannelEvent_PendingAskConsumesMessage(t *testing.T) {
	srv := chanServer(t)
	ad := &fullFakeAdapter{}

	sess := srv.channelMgr.GetOrCreateSession(evFor("seed"))
	replyCh, release, err := sess.BeginAsk()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	srv.routeChannelEvent(context.Background(), ad, evFor("yes"))

	select {
	case got := <-replyCh:
		if got != "yes" {
			t.Errorf("ask received %q, want %q", got, "yes")
		}
	case <-time.After(time.Second):
		t.Fatal("pending ask did not receive the message")
	}
}

// TestRouteChannelEvent_CommandBeatsPendingAsk: /stop must reach the command
// router even while an ask is pending — it cancels the turn that is asking.
func TestRouteChannelEvent_CommandBeatsPendingAsk(t *testing.T) {
	srv := chanServer(t)
	ad := &fullFakeAdapter{}

	sess := srv.channelMgr.GetOrCreateSession(evFor("seed"))
	replyCh, release, err := sess.BeginAsk()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	srv.routeChannelEvent(context.Background(), ad, evFor("/stop"))

	select {
	case got := <-replyCh:
		t.Fatalf("command %q was consumed by the ask", got)
	default:
	}
	if len(ad.texts()) == 0 {
		t.Fatal("command produced no reply")
	}
}

// TestRouteChannelEvent_NormalMessageRunsTurn: with no pending ask, a plain
// message starts an agent turn that replies through the adapter.
func TestRouteChannelEvent_NormalMessageRunsTurn(t *testing.T) {
	srv := chanServer(t)
	ad := &fullFakeAdapter{}

	srv.routeChannelEvent(context.Background(), ad, evFor("hello"))

	waitFor(t, func() bool {
		for _, txt := range ad.texts() {
			if strings.Contains(txt, "stub reply") {
				return true
			}
		}
		return false
	})
}

// TestHandleChannelMessage_SetsPerTurnGate: every IM turn gets a fresh
// permission gate (configured mode + chat-interactive ask) on the session
// agent — the factory-time strict gate is gone.
func TestHandleChannelMessage_SetsPerTurnGate(t *testing.T) {
	srv := chanServer(t)
	ad := &fullFakeAdapter{}

	sess := srv.channelMgr.GetOrCreateSession(evFor("seed"))
	if sess.Agent.Gate != nil {
		t.Fatal("factory must not pre-set a gate anymore")
	}

	srv.handleChannelMessage(context.Background(), ad, evFor("hello"))

	if sess.Agent.Gate == nil {
		t.Error("handleChannelMessage must set a per-turn permission gate")
	}
}
