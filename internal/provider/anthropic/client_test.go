package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Leihb/octo/internal/agent"
	"github.com/Leihb/octo/internal/provider"
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
		if req.System != "you are octo" {
			t.Errorf("system = %q, want 'you are octo'", req.System)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("messages len = %d, want 1", len(req.Messages))
		}
		if req.Messages[0].Role != "user" || req.Messages[0].Content != "hello" {
			t.Errorf("first message = %+v, want {user, hello}", req.Messages[0])
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
	c.Endpoint = srv.URL

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
	c.Endpoint = srv.URL

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
	c.Endpoint = srv.URL

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
	c.Endpoint = "http://invalid"

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
	c.Endpoint = srv.URL

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

// Compile-time assertion: *Client implements provider.Provider.
var _ provider.Provider = (*Client)(nil)
