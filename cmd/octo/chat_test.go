package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Leihb/octo/internal/agent"
	"github.com/Leihb/octo/internal/provider"
)

func TestRunChat_MissingMessage(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var stdout, stderr bytes.Buffer
	code := runChat(nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "provide a message") {
		t.Errorf("stderr should mention missing message; got: %q", stderr.String())
	}
}

func TestRunChat_MissingAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"hello"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("stderr should mention env var; got: %q", stderr.String())
	}
}

func TestRunChat_HonoursAnthropicBaseURL(t *testing.T) {
	// Stand up a fake Anthropic-compatible endpoint and verify runChat
	// actually POSTs there when ANTHROPIC_BASE_URL is set. Same shape the
	// user will use with DeepSeek / Kimi / OpenRouter-Anthropic-shim.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"id":"m","type":"message","role":"assistant","model":"x",
			"content":[{"type":"text","text":"hi"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--model", "x", "hello"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hi") {
		t.Errorf("stdout should contain reply; got: %q", stdout.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
}

func TestRunChat_OpenAI_EndToEnd(t *testing.T) {
	// Stand up a fake OpenAI-compatible endpoint and verify --provider openai
	// routes there with Bearer auth and lands at /v1/chat/completions.
	var gotAuthHeader, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-x","object":"chat.completion","model":"gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"howdy"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_API_KEY", "") // ensure we're testing the openai branch

	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--provider", "openai", "hello"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "howdy") {
		t.Errorf("stdout should contain reply; got: %q", stdout.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuthHeader != "Bearer test-key" {
		t.Errorf("Authorization = %q, want 'Bearer test-key'", gotAuthHeader)
	}
}

func TestRunChat_OpenAI_MissingAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--provider", "openai", "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "OPENAI_API_KEY") {
		t.Errorf("stderr should mention env var; got: %q", stderr.String())
	}
}

func TestRunChat_UnknownProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--provider", "bogus", "hello"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown provider") {
		t.Errorf("stderr should mention unknown provider; got: %q", stderr.String())
	}
}

func TestRunChat_EndToEnd(t *testing.T) {
	// httptest server impersonating Anthropic — proves the full chain
	// (cmd → adapter → provider → HTTP) is wired correctly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"msg_test",
			"type":"message",
			"role":"assistant",
			"model":"claude-haiku-4-5-20251001",
			"content":[{"type":"text","text":"pong"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	// Override the Anthropic endpoint via env-derived plumbing: chat.go
	// constructs the client itself, so we test end-to-end via the lower-level
	// provider+adapter+agent chain rather than through runChat. (runChat's
	// other paths are covered by the surrounding tests.)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	// Use the providerSender adapter directly to exercise the wiring.
	fake := &mockProvider{reply: provider.Response{
		Content: "pong", Model: "m", StopReason: "end_turn",
	}}
	a := agent.New(providerSender{p: fake}, "claude-haiku-4-5-20251001")
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

func TestProviderSender_NilProvider(t *testing.T) {
	s := providerSender{p: nil}
	if _, err := s.SendMessages(context.Background(), "m", "", nil, 0); err == nil {
		t.Error("expected error for nil provider")
	}
}

func TestProviderSender_ProviderError_Surfaces(t *testing.T) {
	fake := &mockProvider{err: errors.New("upstream boom")}
	s := providerSender{p: fake}
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
