package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
	"github.com/Leihb/octo-agent/internal/provider/retry"
)

// canonicalOpenAIStream is a realistic Chat Completions SSE transcript:
// role chunk → two content chunks → finish_reason chunk → [DONE] sentinel.
const canonicalOpenAIStream = "" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"hi "}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"there"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

func TestSendStream_OpenAI_AggregatesAndCallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if !req.Stream {
			t.Errorf("apiRequest.Stream = false, want true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, canonicalOpenAIStream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	c, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL

	var chunks []string
	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:        "gpt-4o-mini",
		SystemPrompt: "you are octo",
		Messages:     []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{OnText: func(d string) { chunks = append(chunks, d) }})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	if got, want := strings.Join(chunks, "|"), "hi |there"; got != want {
		t.Errorf("chunks = %q, want %q", got, want)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.StopReason != "stop" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	// InputTokens/OutputTokens are expected zero — we don't send
	// stream_options.include_usage and the canonical transcript carries no
	// usage block. Document this in the test so a future change doesn't
	// silently regress the compatibility tradeoff.
	if resp.InputTokens != 0 || resp.OutputTokens != 0 {
		t.Errorf("Usage = (%d, %d), want (0, 0) without stream_options", resp.InputTokens, resp.OutputTokens)
	}
}

func TestSendStream_OpenAI_NoDoneSentinelTolerated(t *testing.T) {
	// Some OpenAI-compatible third parties (e.g. early Bailian variants)
	// don't emit `data: [DONE]` — they just close the connection. The
	// scanner reaching EOF must produce the same aggregated result.
	noDone := strings.TrimSuffix(canonicalOpenAIStream, "data: [DONE]\n\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, noDone)
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q", resp.Content)
	}
}

func TestSendStream_OpenAI_UsageChunkParsed(t *testing.T) {
	// When the upstream chooses to emit a final usage chunk anyway, the
	// adapter must pick it up. Verifies we don't drop the field.
	withUsage := canonicalOpenAIStream + "" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}` + "\n\n"
	// Re-insert this before [DONE] to mirror the real wire ordering.
	withUsage = strings.Replace(canonicalOpenAIStream, "data: [DONE]\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`+"\n\n"+
			"data: [DONE]\n\n", 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, withUsage)
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InputTokens != 11 || resp.OutputTokens != 3 {
		t.Errorf("Usage = (%d, %d), want (11, 3)", resp.InputTokens, resp.OutputTokens)
	}
}

// TestSendStream_OpenAI_IdleTimeout verifies that a server which sends a
// partial stream and then goes silent (without closing) trips the idle guard
// instead of blocking forever.
func TestSendStream_OpenAI_IdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant"}}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Stall: stop sending without closing until the client cancels.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	c.StreamIdleTimeout = 40 * time.Millisecond

	done := make(chan struct{})
	var err error
	go func() {
		_, err = c.SendStream(context.Background(), provider.Request{
			Model:    "gpt-4o-mini",
			Messages: []agent.Message{agent.NewUserMessage("hi")},
		}, provider.StreamCallbacks{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SendStream hung past the idle window — idle guard did not fire")
	}
	if !errors.Is(err, retry.ErrStreamIdle) {
		t.Fatalf("err = %v, want retry.ErrStreamIdle", err)
	}
}

func TestSendStream_OpenAI_HTTPError_Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.SendStream(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error should mention HTTP 401: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error should include upstream message: %v", err)
	}
}

func TestSendStream_OpenAI_ValidatesRequest(t *testing.T) {
	c, _ := New("k")
	c.BaseURL = "http://invalid"

	if _, err := c.SendStream(context.Background(), provider.Request{}, provider.StreamCallbacks{}); err == nil {
		t.Error("empty request should error")
	}
	if _, err := c.SendStream(context.Background(), provider.Request{Model: "x"}, provider.StreamCallbacks{}); err == nil {
		t.Error("missing messages should error")
	}
}

// Compile-time assertion.
var _ provider.StreamingProvider = (*Client)(nil)
