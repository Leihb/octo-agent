package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
)

// TestSend_ToolDefinitions_WireFormat verifies that ToolDefinition slices are
// converted to OpenAI's function-tool wire format.
func TestSend_ToolDefinitions_WireFormat(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	_, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
		Tools: []agent.ToolDefinition{
			{
				Name:        "bash",
				Description: "Run shell",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var wireReq struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(capturedBody, &wireReq); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wireReq.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(wireReq.Tools))
	}
	tool := wireReq.Tools[0]
	if tool.Type != "function" {
		t.Errorf("tool.type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "bash" {
		t.Errorf("function.name = %q", tool.Function.Name)
	}
	if tool.Function.Parameters["type"] != "object" {
		t.Errorf("function.parameters.type = %v", tool.Function.Parameters["type"])
	}
}

// TestSend_ToolCall_Response verifies that a response with tool_calls is
// converted to agent.ContentBlock(tool_use) and StopReason normalised.
func TestSend_ToolCall_Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"x","object":"chat.completion","model":"gpt-4o",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":null,
					"tool_calls":[{
						"id":"call-42",
						"type":"function",
						"function":{"name":"bash","arguments":"{\"command\":\"echo hello\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":15,"total_tokens":20}
		}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	resp, err := c.Send(context.Background(), provider.Request{
		Model:    "gpt-4o",
		Messages: []agent.Message{agent.NewUserMessage("echo hello")},
		Tools:    []agent.ToolDefinition{{Name: "bash"}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// finish_reason:"tool_calls" → "tool_use"
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Blocks) != 1 {
		t.Fatalf("Blocks len = %d, want 1", len(resp.Blocks))
	}
	b := resp.Blocks[0]
	if b.Type != "tool_use" {
		t.Errorf("Blocks[0].Type = %q", b.Type)
	}
	if b.ID != "call-42" || b.Name != "bash" {
		t.Errorf("Blocks[0] = %+v", b)
	}
	if b.Input["command"] != "echo hello" {
		t.Errorf("Blocks[0].Input = %v", b.Input)
	}
}

// TestSend_ToolResultMessages_WireFormat verifies that tool_result blocks are
// serialized as individual role="tool" messages (OpenAI format).
func TestSend_ToolResultMessages_WireFormat(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	msgs := []agent.Message{
		agent.NewUserMessage("run echo hi"),
		agent.NewToolUseMessage([]agent.ContentBlock{
			agent.NewToolUseBlock("call-1", "bash", map[string]any{"command": "echo hi"}),
		}),
		agent.NewToolResultMessage([]agent.ContentBlock{
			agent.NewToolResultBlock("call-1", "hi", false),
		}),
	}

	_, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: msgs,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var wireReq struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &wireReq); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// user, assistant(tool_call), tool(result)
	if len(wireReq.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3: %s", len(wireReq.Messages), capturedBody)
	}
	if wireReq.Messages[0].Role != "user" {
		t.Errorf("msg[0].role = %q", wireReq.Messages[0].Role)
	}
	if wireReq.Messages[1].Role != "assistant" {
		t.Errorf("msg[1].role = %q", wireReq.Messages[1].Role)
	}
	if wireReq.Messages[2].Role != "tool" {
		t.Errorf("msg[2].role = %q, want tool", wireReq.Messages[2].Role)
	}
	if wireReq.Messages[2].ToolCallID != "call-1" {
		t.Errorf("msg[2].tool_call_id = %q", wireReq.Messages[2].ToolCallID)
	}
	if wireReq.Messages[2].Content != "hi" {
		t.Errorf("msg[2].content = %q", wireReq.Messages[2].Content)
	}
}

// TestSend_NoTools_FieldAbsent ensures we don't send a "tools" key at all
// when no tools are defined (some OpenAI-compatible servers reject the field
// even when empty).
func TestSend_NoTools_FieldAbsent(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL
	_, err := c.Send(context.Background(), provider.Request{
		Model:    "x",
		Messages: []agent.Message{agent.NewUserMessage("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(capturedBody), `"tools"`) {
		t.Errorf("tools field should be absent: %s", capturedBody)
	}
}

// TestSendStream_ToolCalls_Accumulated verifies that streaming tool_calls
// fragments are assembled into the correct Blocks at stream end.
func TestSendStream_ToolCalls_Accumulated(t *testing.T) {
	// Simulated OpenAI stream with a tool_calls delta.
	toolStream := "" +
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-7","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, toolStream)
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "gpt-4o",
		Messages: []agent.Message{agent.NewUserMessage("list files")},
		Tools:    []agent.ToolDefinition{{Name: "bash"}},
	}, provider.StreamCallbacks{})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Blocks) != 1 {
		t.Fatalf("Blocks len = %d, want 1", len(resp.Blocks))
	}
	b := resp.Blocks[0]
	if b.Type != "tool_use" || b.ID != "call-7" || b.Name != "bash" {
		t.Errorf("Blocks[0] = %+v", b)
	}
	if b.Input["command"] != "ls" {
		t.Errorf("Blocks[0].Input = %v", b.Input)
	}
}

// TestSendStream_OpenAI_ToolInputDeltaCallbackFires verifies that
// tool_calls[].function.arguments fragments arrive on cb.OnToolDelta as
// they stream. OpenAI chunks the arguments JSON across multiple chunks;
// each fragment is forwarded with ToolID + ToolName (once known) + the
// raw partial string.
func TestSendStream_OpenAI_ToolInputDeltaCallbackFires(t *testing.T) {
	// Canned SSE: first chunk carries id+name+first arg fragment, two
	// follow-on chunks carry only the next arg fragments, then
	// finish_reason=tool_calls.
	sse := "" +
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-77","type":"function","function":{"name":"terminal","arguments":"{\"comm"}}]}}]}` + "\n\n" +
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\""}}]}}]}` + "\n\n" +
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls -la\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c, _ := New("k")
	c.BaseURL = srv.URL

	type deltaCall struct{ id, name, partialJSON string }
	var deltas []deltaCall

	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:    "gpt-4o",
		Messages: []agent.Message{agent.NewUserMessage("ls please")},
		Tools:    []agent.ToolDefinition{{Name: "terminal"}},
	}, provider.StreamCallbacks{
		OnToolDelta: func(id, name, partialJSON string) {
			deltas = append(deltas, deltaCall{id, name, partialJSON})
		},
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	// 3 fragments — all tagged call-77 / terminal once known.
	if len(deltas) != 3 {
		t.Fatalf("expected 3 delta callbacks, got %d: %+v", len(deltas), deltas)
	}
	for i, want := range []string{`{"comm`, `and":"`, `ls -la"}`} {
		if deltas[i].partialJSON != want {
			t.Errorf("delta[%d].partialJSON = %q, want %q", i, deltas[i].partialJSON, want)
		}
		if deltas[i].id != "call-77" {
			t.Errorf("delta[%d].id = %q", i, deltas[i].id)
		}
	}

	// Normalised stop_reason and aggregated tool_use block both intact.
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q (should normalise tool_calls → tool_use)", resp.StopReason)
	}
}
