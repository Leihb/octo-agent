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
	// We request usage via stream_options.include_usage, but this canonical
	// transcript carries no usage chunk — a server that omits it just leaves the
	// counts at zero, no error.
	if resp.InputTokens != 0 || resp.OutputTokens != 0 {
		t.Errorf("Usage = (%d, %d), want (0, 0) when the server emits no usage chunk", resp.InputTokens, resp.OutputTokens)
	}
}

// reasoningOpenAIStream interleaves reasoning_content deltas (the field
// DeepSeek/Kimi-style reasoning models stream) with the visible answer.
const reasoningOpenAIStream = "" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"let me "}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"answer"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// SendStream must surface reasoning_content fragments to OnThinking (so the CLI
// can display the trace) while keeping them out of OnText/Content, and must
// forward ReasoningEffort to the wire as reasoning_effort.
func TestSendStream_OpenAI_SurfacesReasoning(t *testing.T) {
	var gotEffort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		gotEffort = req.ReasoningEffort
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reasoningOpenAIStream)
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

	var text, thinking []string
	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:           "deepseek-reasoner",
		Messages:        []agent.Message{agent.NewUserMessage("hi")},
		ReasoningEffort: "high",
	}, provider.StreamCallbacks{
		OnText:     func(d string) { text = append(text, d) },
		OnThinking: func(d string) { thinking = append(thinking, d) },
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	if got, want := strings.Join(thinking, ""), "let me think"; got != want {
		t.Errorf("thinking = %q, want %q", got, want)
	}
	if got, want := strings.Join(text, ""), "answer"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	// Reasoning must NOT leak into the visible content.
	if resp.Content != "answer" {
		t.Errorf("Content = %q, want %q (reasoning must stay out of visible text)", resp.Content, "answer")
	}
	if gotEffort != "high" {
		t.Errorf("wire reasoning_effort = %q, want %q", gotEffort, "high")
	}
}

// With no OnThinking callback the stream must still parse cleanly — reasoning is
// accumulated for history round-trip but simply not surfaced.
func TestSendStream_OpenAI_ReasoningWithoutCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reasoningOpenAIStream)
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

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "deepseek-reasoner",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	if resp.Content != "answer" {
		t.Errorf("Content = %q, want %q", resp.Content, "answer")
	}
}

// A finish_reason of "length" (output-cap truncation) must be normalised to the
// canonical "max_tokens" sentinel so the agent loop's truncation recovery is
// provider-agnostic.
func TestSendStream_OpenAI_LengthNormalisedToMaxTokens(t *testing.T) {
	stream := `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, stream)
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

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "gpt-4o-mini",
		Messages: []agent.Message{agent.NewUserMessage("write a big file")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	if resp.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want %q (length must normalise)", resp.StopReason, "max_tokens")
	}
}

func TestSendStream_SendsIncludeUsage(t *testing.T) {
	// DashScope / real OpenAI send no usage at all on a stream unless we ask via
	// stream_options.include_usage. Assert the outgoing body carries it.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, canonicalOpenAIStream)
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	if _, err := c.SendStream(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{}); err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	so, ok := gotBody["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("request body missing stream_options; got %v", gotBody["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true", so["include_usage"])
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

// TestSendStream_DeepSeek_CacheSplitNormalised mirrors a real DeepSeek warm-cache
// usage chunk: prompt_tokens is the WHOLE input and prompt_cache_hit_tokens a
// subset. The adapter must report InputTokens as the uncached remainder so it
// doesn't overlap CacheReadTokens (else context occupancy double-counts).
func TestSendStream_DeepSeek_CacheSplitNormalised(t *testing.T) {
	withUsage := strings.Replace(canonicalOpenAIStream, "data: [DONE]\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[],"usage":{"prompt_tokens":2708,"completion_tokens":16,"total_tokens":2724,"prompt_cache_hit_tokens":2688,"prompt_cache_miss_tokens":20,"prompt_tokens_details":{"cached_tokens":2688}}}`+"\n\n"+
			"data: [DONE]\n\n", 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, withUsage)
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "deepseek-v4-flash",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InputTokens != 20 {
		t.Errorf("InputTokens = %d, want 20 (uncached remainder, not the full 2708)", resp.InputTokens)
	}
	if resp.CacheReadTokens != 2688 {
		t.Errorf("CacheReadTokens = %d, want 2688", resp.CacheReadTokens)
	}
	// Occupancy (input + cache read) reconstructs the full prompt exactly once.
	if got := resp.InputTokens + resp.CacheReadTokens; got != 2708 {
		t.Errorf("input + cache_read = %d, want 2708 (no double-count, no loss)", got)
	}
}

// TestStreamingHTTPClient_OpenAI_DropsTimeout ensures the streaming client
// drops the injected client's end-to-end Timeout while preserving Transport,
// without mutating the original.
func TestStreamingHTTPClient_OpenAI_DropsTimeout(t *testing.T) {
	tr := &http.Transport{}
	c := &Client{HTTPClient: &http.Client{Timeout: 60 * time.Second, Transport: tr}}
	got := c.streamingHTTPClient()
	if got.Timeout != 0 {
		t.Errorf("streaming client Timeout = %v, want 0", got.Timeout)
	}
	if got == c.HTTPClient {
		t.Error("streaming client must be a clone, not the injected client")
	}
	if got.Transport != tr {
		t.Error("clone must preserve the injected Transport")
	}
	if c.HTTPClient.Timeout != 60*time.Second {
		t.Errorf("injected client Timeout was mutated to %v, want 60s", c.HTTPClient.Timeout)
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

// TestSendStream_Kimi_ChoiceUsage verifies that Kimi-style usage embedded
// inside the final choice object (rather than at the chunk level) is parsed.
func TestSendStream_Kimi_ChoiceUsage(t *testing.T) {
	// Kimi puts usage inside choices[0].usage on the final chunk.
	kimiStream := "" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"kimi-k2.6","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"kimi-k2.6","choices":[{"index":0,"delta":{"content":"hi "}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"kimi-k2.6","choices":[{"index":0,"delta":{"content":"there"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"kimi-k2.6","choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":19,"completion_tokens":13,"total_tokens":32}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, kimiStream)
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "kimi-k2.6",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want %q", resp.Content, "hi there")
	}
	if resp.InputTokens != 19 || resp.OutputTokens != 13 {
		t.Errorf("Usage = (%d, %d), want (19, 13)", resp.InputTokens, resp.OutputTokens)
	}
}

// Compile-time assertion.
var _ provider.StreamingProvider = (*Client)(nil)
