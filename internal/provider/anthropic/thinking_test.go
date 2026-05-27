package anthropic

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

// thinkingStream mimics an extended-thinking SSE response: a thinking block
// (text via thinking_delta, signature via signature_delta) followed by a
// tool_use block — the shape Kimi/Claude emit when thinking + tools are on.
const thinkingStream = "" +
	`data: {"type":"message_start","message":{"id":"m","model":"k2.6","usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check."}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG123"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"read_file"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"go.mod\"}"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":0,"output_tokens":12}}` + "\n\n"

// TestSendStream_Thinking captures the thinking block (text + signature) and
// fires OnThinking, while keeping the trace out of the visible answer text, and
// verifies the request enabled thinking with a bumped max_tokens.
func TestSendStream_Thinking(t *testing.T) {
	var body apiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, thinkingStream)
	}))
	defer srv.Close()

	c, _ := New("test-key")
	c.BaseURL = srv.URL

	var thoughts []string
	resp, err := c.SendStream(context.Background(), provider.Request{
		Model:          "k2.6",
		Messages:       []agent.Message{agent.NewUserMessage("read go.mod")},
		MaxTokens:      100, // below budget on purpose; applyThinking must bump it
		ThinkingBudget: 1024,
		Tools:          []agent.ToolDefinition{{Name: "read_file"}},
	}, provider.StreamCallbacks{OnThinking: func(d string) { thoughts = append(thoughts, d) }})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	// Request must have enabled thinking and bumped max_tokens above the budget.
	if body.Thinking == nil || body.Thinking.Type != "enabled" || body.Thinking.BudgetTokens != 1024 {
		t.Fatalf("request thinking = %+v, want enabled/1024", body.Thinking)
	}
	if body.MaxTokens <= 1024 {
		t.Errorf("MaxTokens = %d, want > budget 1024", body.MaxTokens)
	}

	// OnThinking fired per thinking_delta, in order.
	if got := strings.Join(thoughts, ""); got != "Let me check." {
		t.Errorf("thinking deltas = %q, want 'Let me check.'", got)
	}
	// Thinking text must NOT leak into the visible answer.
	if resp.Content != "" {
		t.Errorf("Content = %q, want empty (thinking is not answer text)", resp.Content)
	}
	// Blocks: thinking (with signature) then tool_use, in order.
	if len(resp.Blocks) != 2 {
		t.Fatalf("Blocks len = %d, want 2: %+v", len(resp.Blocks), resp.Blocks)
	}
	if resp.Blocks[0].Type != "thinking" || resp.Blocks[0].Thinking != "Let me check." || resp.Blocks[0].Signature != "SIG123" {
		t.Errorf("Blocks[0] = %+v, want thinking with signature", resp.Blocks[0])
	}
	if resp.Blocks[1].Type != "tool_use" {
		t.Errorf("Blocks[1].Type = %q, want tool_use", resp.Blocks[1].Type)
	}
}

// TestSend_ThinkingBlock_RoundTrips verifies a thinking block in history is
// re-serialized to the wire with its signature, ahead of the tool_use block —
// the contract the API enforces for thinking + tools.
func TestSend_ThinkingBlock_RoundTrips(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","model":"k2.6","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c, _ := New("test-key")
	c.BaseURL = srv.URL

	msgs := []agent.Message{
		agent.NewUserMessage("read go.mod"),
		agent.NewToolUseMessage([]agent.ContentBlock{
			agent.NewThinkingBlock("Let me check.", "SIG123"),
			agent.NewToolUseBlock("call-1", "read_file", map[string]any{"path": "go.mod"}),
		}),
		agent.NewToolResultMessage([]agent.ContentBlock{
			agent.NewToolResultBlock("call-1", "module github.com/Leihb/octo-agent", false),
		}),
	}
	if _, err := c.Send(context.Background(), provider.Request{Model: "k2.6", Messages: msgs}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Content shape varies per message (string for plain turns, array for
	// block turns), so decode it lazily as RawMessage.
	var wire struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var found bool
	for _, m := range wire.Messages {
		if m.Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			t.Fatalf("assistant content not an array: %v (%s)", err, m.Content)
		}
		found = true
		if len(blocks) == 0 || blocks[0].Type != "thinking" || blocks[0].Thinking != "Let me check." || blocks[0].Signature != "SIG123" {
			t.Errorf("assistant block[0] = %+v, want thinking with signature", blocks)
		}
		if len(blocks) < 2 || blocks[1].Type != "tool_use" {
			t.Errorf("thinking block must precede tool_use; got %+v", blocks)
		}
	}
	if !found {
		t.Fatalf("no assistant message in wire body: %s", capturedBody)
	}
}
