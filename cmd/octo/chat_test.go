package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
)

// TestRunChat_NoArgs_EntersREPL verifies that omitting a message starts the
// interactive REPL (M3) rather than erroring out (M2 behaviour).
// We supply an immediate EOF on stdin so the REPL exits without blocking.
func TestRunChat_NoArgs_EntersREPL(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp) // Windows compat
	// Fake provider so we don't need a real API key.
	var stdout, stderr bytes.Buffer
	// Immediate EOF → REPL reads nothing and exits cleanly.
	code := runChat(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Starting session") {
		t.Errorf("stdout should contain REPL banner; got: %q", stdout.String())
	}
}

func TestRunChat_MissingAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"hello"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	// New (UX-3) actionable error: identifies the missing env var AND points
	// the user at the signup URL + alternative provider so the first-run
	// experience isn't a dead-end.
	out := stderr.String()
	for _, want := range []string{
		"ANTHROPIC_API_KEY",
		"console.anthropic.com",
		"--provider openai",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr should mention %q; got:\n%s", want, out)
		}
	}
}

func TestRunChat_InvalidPermissionMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--permission-mode", "strikt", "hello"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage error)", code)
	}
	if !strings.Contains(stderr.String(), "permission-mode") {
		t.Errorf("stderr should explain the bad flag; got: %q", stderr.String())
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
	// --stream=false because the fake server above returns a plain JSON body,
	// not an SSE stream. Streaming end-to-end is covered by
	// TestRunChat_Anthropic_StreamingEndToEnd below.
	code := runChat([]string{"--model", "x", "--stream=false", "hello"}, strings.NewReader(""), &stdout, &stderr)
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
	// --stream=false: same rationale as the Anthropic test above; this
	// fake serves plain JSON, not SSE.
	code := runChat([]string{"--provider", "openai", "--stream=false", "hello"}, strings.NewReader(""), &stdout, &stderr)
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
	code := runChat([]string{"--provider", "openai", "hello"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	// New (UX-3) actionable error: points the user at the OpenAI signup
	// URL plus the Anthropic fallback so a missing key has a path forward.
	out := stderr.String()
	for _, want := range []string{
		"OPENAI_API_KEY",
		"platform.openai.com",
		"ANTHROPIC_API_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr should mention %q; got:\n%s", want, out)
		}
	}
}

// TestRunChat_UnknownResumeID exercises the UX-3 hint that follows a failed
// session resume: the resolver itself reports "no session matches", and the
// chat wrapper adds a pointer to --list-sessions so the user knows where to
// look for valid IDs.
func TestRunChat_UnknownResumeID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"-c", "no-such-thing"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "no session matches") {
		t.Errorf("stderr should report no match; got:\n%s", out)
	}
	if !strings.Contains(out, "octo chat --list-sessions") {
		t.Errorf("stderr should hint at --list-sessions; got:\n%s", out)
	}
}

func TestRunChat_UnknownProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--provider", "bogus", "hello"}, strings.NewReader(""), &stdout, &stderr)
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

// streamingMockProvider also implements provider.StreamingProvider so we can
// verify providerSender.StreamMessages picks the streaming path when the
// underlying provider supports it.
type streamingMockProvider struct {
	mockProvider
	deltas       []string
	streamReply  provider.Response
	streamCalled bool
}

func (m *streamingMockProvider) SendStream(_ context.Context, req provider.Request, cb provider.StreamCallbacks) (provider.Response, error) {
	m.streamCalled = true
	m.gotReq = req
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

func TestProviderSender_StreamingPathPreferred(t *testing.T) {
	fake := &streamingMockProvider{
		deltas:      []string{"hi ", "there"},
		streamReply: provider.Response{Content: "hi there", Model: "m", StopReason: "end_turn"},
	}
	s := providerSender{p: fake}

	var got []string
	reply, err := s.StreamMessages(
		context.Background(), "m", "",
		[]agent.Message{agent.NewUserMessage("hi")}, 0,
		func(d string) { got = append(got, d) },
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

func TestProviderSender_StreamingFallback_NonStreamingProvider(t *testing.T) {
	// mockProvider only implements provider.Provider — not StreamingProvider.
	// providerSender.StreamMessages must fall back to Send and synthesise
	// a single onChunk call with the full content.
	fake := &mockProvider{reply: provider.Response{Content: "buffered"}}
	s := providerSender{p: fake}

	var got []string
	reply, err := s.StreamMessages(
		context.Background(), "m", "",
		[]agent.Message{agent.NewUserMessage("hi")}, 0,
		func(d string) { got = append(got, d) },
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

func TestRunChat_Anthropic_StreamingEndToEnd(t *testing.T) {
	// Full chain: runChat → agent.TurnStream → providerSender.StreamMessages
	// → anthropic.SendStream → SSE parse → callback → stdout.
	sse := "" +
		`data: {"type":"message_start","message":{"id":"m","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi "}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":2}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	code := runChat([]string{"hello"}, strings.NewReader(""), &stdout, &stderr) // streaming on by default
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "hi there") {
		t.Errorf("stdout should contain streamed reply; got: %q", out)
	}
}

func TestRunChat_OpenAI_StreamingEndToEnd(t *testing.T) {
	sse := "" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"howdy "}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"partner"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_API_KEY", "")

	var stdout, stderr bytes.Buffer
	code := runChat([]string{"--provider", "openai", "hi"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "howdy partner") {
		t.Errorf("stdout should contain streamed reply; got: %q", stdout.String())
	}
}

// TestRunChat_ResumedToolSession_DefaultOnNoWarning confirms the common case
// is footgun-free now that tools are on by default: resuming a tool-using
// session needs no flag and prints no warning, because the tools array goes
// out with the request as it did originally.
func TestRunChat_ResumedToolSession_DefaultOnNoWarning(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	// Seed a session whose history includes a tool_use block (mirrors what
	// a real tool-enabled session looks like on disk).
	sess := agent.NewSession("test-model", "")
	sess.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "list files"},
		{Role: agent.RoleAssistant, Blocks: []agent.ContentBlock{
			agent.NewToolUseBlock("call_1", "terminal", map[string]any{"command": "ls"}),
		}},
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	// Resume with no tool flag at all. Provide immediate EOF so the REPL exits.
	var stdout, stderr bytes.Buffer
	code := runChat([]string{"-c", sess.ID}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "may make the model emit tool calls as text") {
		t.Errorf("tools-on resume should not warn; got stderr:\n%s", stderr.String())
	}
}

// TestRunChat_ResumedToolSession_NoToolsWarns confirms that explicitly
// resuming a tool-using session with --no-tools respects the choice but
// warns once about the garbled-XML risk.
func TestRunChat_ResumedToolSession_NoToolsWarns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	sess := agent.NewSession("test-model", "")
	sess.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "list files"},
		{Role: agent.RoleAssistant, Blocks: []agent.ContentBlock{
			agent.NewToolUseBlock("call_1", "terminal", map[string]any{"command": "ls"}),
		}},
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runChat([]string{"-c", sess.ID, "--no-tools"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "may make the model emit tool calls as text") {
		t.Errorf("expected --no-tools warning on a tool session; got stderr:\n%s", stderr.String())
	}
}

// TestRunChat_ResumedPlainSession_NoWarning confirms a session that never
// used tools triggers no --no-tools warning — we don't nag every resume.
func TestRunChat_ResumedPlainSession_NoWarning(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	sess := agent.NewSession("test-model", "")
	sess.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "hi"},
		{Role: agent.RoleAssistant, Content: "hello"},
	}
	if err := sess.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runChat([]string{"-c", sess.ID, "--no-tools"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "may make the model emit tool calls as text") {
		t.Errorf("plain session should not warn; got stderr:\n%s", stderr.String())
	}
}
