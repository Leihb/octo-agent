package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
	"github.com/Leihb/octo-agent/internal/provider/retry"
)

func TestNew_EmptyKeyRejected(t *testing.T) {
	for _, k := range []string{"", "   ", "\t\n"} {
		if _, err := New(k); err == nil {
			t.Errorf("New(%q) expected error, got nil", k)
		}
	}
}

func TestSend_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers
		if got, want := r.Header.Get("x-api-key"), "test-key"; got != want {
			t.Errorf("x-api-key header = %q, want %q", got, want)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Errorf("anthropic-version header empty")
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		// Body
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if req.Model != "claude-haiku-4-5-20251001" {
			t.Errorf("model = %q, want claude-haiku-4-5-20251001", req.Model)
		}
		// System is now sent in the cacheable array form: a single text block
		// carrying the prompt plus an ephemeral cache_control breakpoint.
		sysJSON, _ := json.Marshal(req.System)
		if !strings.Contains(string(sysJSON), `"you are octo"`) {
			t.Errorf("system missing prompt text: %s", sysJSON)
		}
		if !strings.Contains(string(sysJSON), `"ephemeral"`) {
			t.Errorf("system missing cache_control breakpoint: %s", sysJSON)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("messages len = %d, want 1", len(req.Messages))
		}
		// The last message carries a cache breakpoint, so its content is sent
		// as a block array (not a bare string) with an ephemeral marker.
		msgJSON := string(req.Messages[0].Content)
		if req.Messages[0].Role != "user" || !strings.Contains(msgJSON, `"hello"`) {
			t.Errorf("first message = %+v, want a user block carrying 'hello'", req.Messages[0])
		}
		if !strings.Contains(msgJSON, `"ephemeral"`) {
			t.Errorf("last message should carry a cache breakpoint: %s", msgJSON)
		}

		// Response
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_01",
			"type": "message",
			"role": "assistant",
			"model": "claude-haiku-4-5-20251001",
			"content": [
				{"type": "text", "text": "hi there"},
				{"type": "text", "text": " — i'm octo"}
			],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 7}
		}`))
	}))
	defer srv.Close()

	c, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL

	resp, err := c.Send(context.Background(), provider.Request{
		Model:        "claude-haiku-4-5-20251001",
		SystemPrompt: "you are octo",
		Messages:     []agent.Message{agent.NewUserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if resp.Content != "hi there — i'm octo" {
		t.Errorf("Content = %q, want %q", resp.Content, "hi there — i'm octo")
	}
	if resp.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if resp.InputTokens != 12 || resp.OutputTokens != 7 {
		t.Errorf("Usage = (%d, %d), want (12, 7)", resp.InputTokens, resp.OutputTokens)
	}
}

// TestSend_UserImage_WireFormat verifies a standalone image block on a user
// message (e.g. a pasted clipboard image) serializes to Anthropic's base64
// image source nested in the user message content array.
func TestSend_UserImage_WireFormat(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.Send(context.Background(), provider.Request{
		Model: "x",
		Messages: []agent.Message{{
			Role: agent.RoleUser,
			Blocks: []agent.ContentBlock{
				agent.NewTextBlock("what is this?"),
				agent.NewImageBlock("image/png", []byte{0x89, 'P', 'N', 'G'}),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &req); err != nil {
		t.Fatalf("decode: %v\n%s", err, capturedBody)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("want 1 message with text+image content: %s", capturedBody)
	}
	img := req.Messages[0].Content[1]
	if img.Type != "image" || img.Source == nil {
		t.Fatalf("content[1] = %+v, want image with source", img)
	}
	if img.Source.Type != "base64" || img.Source.MediaType != "image/png" || img.Source.Data == "" {
		t.Errorf("image source = %+v, want base64 image/png with data", img.Source)
	}
}

func TestSend_SystemMessageStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		_ = json.Unmarshal(bodyBytes, &req)
		// Anthropic forbids role:"system" inside the messages array — verify
		// our adapter strips it and routes content to req.System instead.
		for _, m := range req.Messages {
			if m.Role == "system" {
				t.Errorf("system role leaked into Messages: %+v", m)
			}
		}
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.Send(context.Background(), provider.Request{
		Model: "claude-haiku-4-5-20251001",
		Messages: []agent.Message{
			agent.NewSystemMessage("ignore-me-in-array"),
			agent.NewUserMessage("hi"),
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSend_HTTPError_Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"type":"error",
			"error":{"type":"invalid_request_error","message":"max_tokens must be > 0"}
		}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	})
	if err == nil {
		t.Fatal("Send: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error should mention HTTP 400: %v", err)
	}
	if !strings.Contains(err.Error(), "max_tokens must be > 0") {
		t.Errorf("error should include upstream message: %v", err)
	}
}

func TestSend_ValidatesRequest(t *testing.T) {
	c, _ := New("k")
	c.BaseURL = "http://invalid"

	if _, err := c.Send(context.Background(), provider.Request{}); err == nil {
		t.Error("empty request should error")
	}
	if _, err := c.Send(context.Background(), provider.Request{Model: "x"}); err == nil {
		t.Error("missing messages should error")
	}
}

func TestSend_DefaultMaxTokens(t *testing.T) {
	var capturedMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		_ = json.Unmarshal(bodyBytes, &req)
		capturedMaxTokens = req.MaxTokens
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
		// MaxTokens intentionally left as zero — adapter should apply the default.
	})
	if err != nil {
		t.Fatal(err)
	}
	if capturedMaxTokens != DefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want default %d", capturedMaxTokens, DefaultMaxTokens)
	}
}

func TestSend_AppendsMessagesPath(t *testing.T) {
	// Verify the client always POSTs to BaseURL + "/v1/messages", regardless
	// of whether the caller's BaseURL has a trailing slash. This is the
	// guarantee that makes pointing at Anthropic-compatible third parties
	// (e.g. DeepSeek at https://api.deepseek.com/anthropic) work transparently.
	cases := []struct {
		name string
		base string
	}{
		{"no trailing slash", ""}, // populated below with srv.URL
		{"trailing slash", ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`))
			}))
			defer srv.Close()

			cl, _ := New("k")
			cl.BaseURL = srv.URL
			if i == 1 {
				cl.BaseURL = srv.URL + "/"
			}

			_, err := cl.Send(context.Background(), provider.Request{
				Model:    "x",
				Messages: []agent.Message{agent.NewUserMessage("hi")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != "/v1/messages" {
				t.Errorf("path = %q, want /v1/messages", gotPath)
			}
		})
	}
}

// Compile-time assertion: *Client implements provider.Provider.
var _ provider.Provider = (*Client)(nil)

func TestSend_ParsesCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_01","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":"hi"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":1200,"cache_read_input_tokens":3400}
		}`))
	}))
	defer srv.Close()

	c, err := New("k")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL

	resp, err := c.Send(context.Background(), provider.Request{
		Model:    "m",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.CacheWriteTokens != 1200 {
		t.Errorf("CacheWriteTokens = %d, want 1200 (cache_creation_input_tokens)", resp.CacheWriteTokens)
	}
	if resp.CacheReadTokens != 3400 {
		t.Errorf("CacheReadTokens = %d, want 3400 (cache_read_input_tokens)", resp.CacheReadTokens)
	}
}

func TestSend_RetriesTransientThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// First attempt: transient 503 with a Retry-After the policy caps.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"overloaded_error","message":"try again"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x",
			"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	c.Retry = retry.Policy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

	resp, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Content)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 HTTP attempts (1 retry), got %d", got)
	}
}

func TestSend_DoesNotRetryClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest) // 400: not retryable
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	c.Retry = retry.Policy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}

	if _, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	}); err == nil {
		t.Fatal("expected error for 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("400 should not be retried; attempts=%d", got)
	}
}

// TestSend_ToolUseWithNonToolUseStopReason verifies a response carrying a
// tool_use block is treated as a tool-use turn even when a (misbehaving)
// compatible backend reports stop_reason "end_turn" instead of "tool_use".
func TestSend_ToolUseWithNonToolUseStopReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"x","content":[{"type":"tool_use","id":"toolu_1","name":"edit_file","input":{"path":"a.go"}}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c, _ := New("test-key")
	c.BaseURL = srv.URL
	resp, err := c.Send(context.Background(), provider.Request{
		Model: "x", Messages: []agent.Message{agent.NewUserMessage("edit it")},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use (tool_use block present despite stop_reason end_turn)", resp.StopReason)
	}
	gotTool := false
	for _, b := range resp.Blocks {
		if b.Type == "tool_use" && b.Name == "edit_file" {
			gotTool = true
		}
	}
	if !gotTool {
		t.Error("expected an edit_file tool_use block")
	}
}
