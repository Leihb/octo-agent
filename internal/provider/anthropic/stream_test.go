package anthropic

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

// canonicalStream is a realistic Anthropic SSE transcript:
// message_start → content_block_start → two text deltas → content_block_stop
// → message_delta (with stop_reason and final usage) → message_stop.
//
// Each event is preceded by `event: <type>` for fidelity to the wire format;
// the parser ignores those and only reads `data:` lines, but including them
// guards against a regression that would skip JSON parsing when an event:
// line is present.
const canonicalStream = "" +
	"event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":12,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi "}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestSendStream_AggregatesAndCallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers: streaming requests must announce SSE acceptance and the
		// JSON body must carry stream:true.
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
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
		_, _ = io.WriteString(w, canonicalStream)
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
		Model:    "claude-haiku-4-5-20251001",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{OnText: func(d string) { chunks = append(chunks, d) }})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	// onChunk must fire once per text_delta, in order.
	if got, want := strings.Join(chunks, "|"), "hi |there"; got != want {
		t.Errorf("chunks = %q, want %q", got, want)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want 'hi there'", resp.Content)
	}
	if resp.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	// InputTokens comes from message_start; OutputTokens from the final
	// message_delta usage block.
	if resp.InputTokens != 12 || resp.OutputTokens != 7 {
		t.Errorf("Usage = (%d, %d), want (12, 7)", resp.InputTokens, resp.OutputTokens)
	}
}

func TestSendStream_NilCallbackTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, canonicalStream)
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

// TestSendStream_IdleTimeout verifies that a server which sends a partial
// stream and then goes silent (without closing) trips the idle guard instead
// of blocking forever. The handler streams one event, flushes, then blocks
// until the client aborts the request.
func TestSendStream_IdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
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
			Model:    "x",
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

func TestSendStream_HTTPError_Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
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
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("error should include upstream message: %v", err)
	}
}

func TestSendStream_ValidatesRequest(t *testing.T) {
	c, _ := New("k")
	c.BaseURL = "http://invalid"

	if _, err := c.SendStream(context.Background(), provider.Request{}, provider.StreamCallbacks{}); err == nil {
		t.Error("empty request should error")
	}
	if _, err := c.SendStream(context.Background(), provider.Request{Model: "x"}, provider.StreamCallbacks{}); err == nil {
		t.Error("missing messages should error")
	}
}

func TestSendStream_NonTextDeltasIgnored(t *testing.T) {
	// Future Anthropic versions emit input_json_delta for tool_use blocks.
	// The text aggregator must skip any delta whose type is not "text_delta",
	// so we don't accidentally splice JSON fragments into the assistant text.
	mixed := "" +
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"keep"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" me"}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":2}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mixed)
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
	if resp.Content != "keep me" {
		t.Errorf("Content = %q, want 'keep me' (input_json_delta should be skipped)", resp.Content)
	}
}

// kimiStream mimics Kimi's Anthropic-compatible SSE: the single space after
// "data:" is omitted (valid per the SSE spec), and cache accounting lands in
// the final message_delta rather than in message_start.
const kimiStream = "" +
	`event:message_start` + "\n" +
	`data:{"type":"message_start","message":{"id":"m","model":"kimi-for-coding","usage":{"input_tokens":1988,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n\n" +
	`event:content_block_start` + "\n" +
	`data:{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`event:content_block_delta` + "\n" +
	`data:{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cached"}}` + "\n\n" +
	`event:content_block_stop` + "\n" +
	`data:{"type":"content_block_stop","index":0}` + "\n\n" +
	`event:message_delta` + "\n" +
	`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":3,"cache_read_input_tokens":1988,"cache_creation_input_tokens":0}}` + "\n\n" +
	`event:message_stop` + "\n" +
	`data:{"type":"message_stop"}` + "\n\n"

// TestSendStream_KimiNoSpaceAndDeltaCache guards two compatible-backend quirks:
// SSE "data:" lines without a trailing space must still be parsed, and cache
// accounting reported only in message_delta must be picked up.
func TestSendStream_KimiNoSpaceAndDeltaCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, kimiStream)
	}))
	defer srv.Close()

	c, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "k2.6",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	if resp.Content != "cached" {
		t.Errorf("Content = %q, want 'cached' (no-space data: lines must parse)", resp.Content)
	}
	if resp.CacheReadTokens != 1988 {
		t.Errorf("CacheReadTokens = %d, want 1988 (message_delta cache must be captured)", resp.CacheReadTokens)
	}
	// message_delta carries the non-cached remainder; on a full cache hit it's 0.
	if resp.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", resp.InputTokens)
	}
}

// Compile-time assertion: *Client implements provider.StreamingProvider in the
// test package too, in case future refactors split the file layout.
var _ provider.StreamingProvider = (*Client)(nil)
