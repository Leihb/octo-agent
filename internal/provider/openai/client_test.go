package openai

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
		// Headers — OpenAI uses Bearer auth, NOT x-api-key
		if got, want := r.Header.Get("Authorization"), "Bearer test-key"; got != want {
			t.Errorf("Authorization header = %q, want %q", got, want)
		}
		if r.Header.Get("x-api-key") != "" {
			t.Errorf("x-api-key should NOT be set for OpenAI: %q", r.Header.Get("x-api-key"))
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
		if req.Model != "gpt-4o-mini" {
			t.Errorf("model = %q, want gpt-4o-mini", req.Model)
		}

		// OpenAI carries system prompt INSIDE the messages array — opposite
		// direction from Anthropic. Verify the adapter prepended it.
		if len(req.Messages) != 2 {
			t.Fatalf("messages len = %d, want 2 (system + user)", len(req.Messages))
		}
		if req.Messages[0].Role != "system" || req.Messages[0].Content != "you are octo" {
			t.Errorf("first message = %+v, want {system, 'you are octo'}", req.Messages[0])
		}
		if req.Messages[1].Role != "user" || req.Messages[1].Content != "hello" {
			t.Errorf("second message = %+v, want {user, hello}", req.Messages[1])
		}

		// Response
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"gpt-4o-mini",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"hi there"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}
		}`))
	}))
	defer srv.Close()

	c, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL

	resp, err := c.Send(context.Background(), provider.Request{
		Model:        "gpt-4o-mini",
		SystemPrompt: "you are octo",
		Messages:     []agent.Message{agent.NewUserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want %q", resp.Content, "hi there")
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", resp.StopReason)
	}
	if resp.InputTokens != 12 || resp.OutputTokens != 7 {
		t.Errorf("Usage = (%d, %d), want (12, 7)", resp.InputTokens, resp.OutputTokens)
	}
}

func TestSend_SystemMessageInArrayDeduped(t *testing.T) {
	// If the caller stuffs role:"system" into Request.Messages AND sets
	// Request.SystemPrompt, the adapter must use SystemPrompt as the canonical
	// source and drop the in-array system entry. Otherwise OpenAI would see
	// two system messages and the second would override the agent's intent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		_ = json.Unmarshal(bodyBytes, &req)

		systemCount := 0
		for _, m := range req.Messages {
			if m.Role == "system" {
				systemCount++
			}
		}
		if systemCount != 1 {
			t.Errorf("system count = %d, want exactly 1", systemCount)
		}
		if req.Messages[0].Role != "system" || req.Messages[0].Content != "canonical" {
			t.Errorf("first message = %+v, want canonical system", req.Messages[0])
		}

		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.Send(context.Background(), provider.Request{
		Model:        "gpt-4o-mini",
		SystemPrompt: "canonical",
		Messages: []agent.Message{
			agent.NewSystemMessage("ignored-stuffed-system"),
			agent.NewUserMessage("hi"),
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSend_SystemOmittedWhenEmpty(t *testing.T) {
	// When SystemPrompt is empty, no system message should be added.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var req apiRequest
		_ = json.Unmarshal(bodyBytes, &req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				t.Errorf("system role should not be present: %+v", m)
			}
		}
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	_, err := c.Send(context.Background(), provider.Request{
		Model:    "gpt-4o-mini",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSend_HTTPError_Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{
			"error":{"message":"Incorrect API key","type":"invalid_request_error","code":"invalid_api_key"}
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
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error should mention HTTP 401: %v", err)
	}
	if !strings.Contains(err.Error(), "Incorrect API key") {
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

func TestSend_AppendsChatCompletionsPath(t *testing.T) {
	// Mirror of the Anthropic test: BaseURL with or without trailing slash
	// must still land at /v1/chat/completions.
	cases := []struct {
		name string
	}{{"no trailing slash"}, {"trailing slash"}}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = w.Write([]byte(`{"id":"m","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`))
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
			if gotPath != "/v1/chat/completions" {
				t.Errorf("path = %q, want /v1/chat/completions", gotPath)
			}
		})
	}
}

func TestSend_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"x","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	_, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	})
	if err == nil {
		t.Error("expected error when response has no choices")
	}
}

// Compile-time assertion: *Client implements provider.Provider.
var _ provider.Provider = (*Client)(nil)
