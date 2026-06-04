package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
)

func TestSender_PassesRequestThrough(t *testing.T) {
	fake := &mockProvider{reply: provider.Response{
		Content: "pong", Model: "m", StopReason: "end_turn",
	}}
	a := agent.New(sender{p: fake}, "claude-haiku-4-5-20251001")
	reply, err := a.Turn(context.Background(), "ping")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "pong" {
		t.Errorf("Content = %q, want pong", reply.Content)
	}
	if fake.gotReq.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model passed through = %q", fake.gotReq.Model)
	}
	if len(fake.gotReq.Messages) != 1 || fake.gotReq.Messages[0].Content != "ping" {
		t.Errorf("messages passed through = %+v", fake.gotReq.Messages)
	}
}

func TestSender_NilProvider(t *testing.T) {
	s := sender{p: nil}
	if _, err := s.SendMessages(context.Background(), "m", "", nil, 0); err == nil {
		t.Error("expected error for nil provider")
	}
}

func TestSender_ProviderError_Surfaces(t *testing.T) {
	fake := &mockProvider{err: errors.New("upstream boom")}
	s := sender{p: fake}
	_, err := s.SendMessages(context.Background(), "m", "", []agent.Message{agent.NewUserMessage("hi")}, 0)
	if err == nil || !strings.Contains(err.Error(), "upstream boom") {
		t.Errorf("expected upstream error, got: %v", err)
	}
}

// mockProvider implements provider.Provider for tests.
type mockProvider struct {
	reply  provider.Response
	err    error
	gotReq provider.Request
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	m.gotReq = req
	return m.reply, m.err
}

// streamingMockProvider also implements provider.StreamingProvider so we can
// verify sender.StreamMessages picks the streaming path when the underlying
// provider supports it.
type streamingMockProvider struct {
	mockProvider
	deltas       []string
	thinkDeltas  []string
	streamReply  provider.Response
	streamCalled bool
	// onThinkingSet records whether the caller wired an OnThinking callback —
	// sender drops it when reasoning display is off.
	onThinkingSet bool
}

func (m *streamingMockProvider) SendStream(_ context.Context, req provider.Request, cb provider.StreamCallbacks) (provider.Response, error) {
	m.streamCalled = true
	m.gotReq = req
	m.onThinkingSet = cb.OnThinking != nil
	for _, d := range m.thinkDeltas {
		if cb.OnThinking != nil {
			cb.OnThinking(d)
		}
	}
	for _, d := range m.deltas {
		if cb.OnText != nil {
			cb.OnText(d)
		}
	}
	if m.err != nil {
		return provider.Response{}, m.err
	}
	return m.streamReply, nil
}

func TestSender_ReasoningSink_Gating(t *testing.T) {
	cases := []struct {
		name          string
		showReasoning bool
		onThinking    func(string)
		wantForwarded bool // whether the provider received a non-nil OnThinking
	}{
		{"on + handler → forwarded", true, func(string) {}, true},
		{"off → dropped", false, func(string) {}, false},
		{"on but nil handler → nil", true, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &streamingMockProvider{
				thinkDeltas: []string{"reasoning"},
				streamReply: provider.Response{Content: "ok", StopReason: "end_turn"},
			}
			s := sender{p: fake, showReasoning: c.showReasoning}
			_, err := s.StreamMessages(
				context.Background(), "m", "",
				[]agent.Message{agent.NewUserMessage("hi")}, 0,
				func(string) {},
				c.onThinking,
			)
			if err != nil {
				t.Fatal(err)
			}
			if fake.onThinkingSet != c.wantForwarded {
				t.Errorf("provider OnThinking set = %v, want %v", fake.onThinkingSet, c.wantForwarded)
			}
		})
	}
}

func TestSender_StreamingPathPreferred(t *testing.T) {
	fake := &streamingMockProvider{
		deltas:      []string{"hi ", "there"},
		streamReply: provider.Response{Content: "hi there", Model: "m", StopReason: "end_turn"},
	}
	s := sender{p: fake}

	var got []string
	reply, err := s.StreamMessages(
		context.Background(), "m", "",
		[]agent.Message{agent.NewUserMessage("hi")}, 0,
		func(d string) { got = append(got, d) },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !fake.streamCalled {
		t.Error("expected SendStream to be called when provider supports it")
	}
	if reply.Content != "hi there" {
		t.Errorf("Content = %q", reply.Content)
	}
	if len(got) != 2 || got[0] != "hi " || got[1] != "there" {
		t.Errorf("chunks = %v", got)
	}
}

func TestSender_StreamingFallback_NonStreamingProvider(t *testing.T) {
	// mockProvider only implements provider.Provider — not StreamingProvider.
	// sender.StreamMessages must fall back to Send and synthesise a single
	// onChunk call with the full content.
	fake := &mockProvider{reply: provider.Response{Content: "buffered"}}
	s := sender{p: fake}

	var got []string
	reply, err := s.StreamMessages(
		context.Background(), "m", "",
		[]agent.Message{agent.NewUserMessage("hi")}, 0,
		func(d string) { got = append(got, d) },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "buffered" {
		t.Errorf("Content = %q", reply.Content)
	}
	if len(got) != 1 || got[0] != "buffered" {
		t.Errorf("fallback should emit one chunk with full content; got %v", got)
	}
}

func TestNewSender_UnknownProvider(t *testing.T) {
	if _, err := NewSender(SenderOptions{Provider: "nope", APIKey: "k"}); err == nil {
		t.Error("expected error for unknown provider")
	}
}
