package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
)

// toolCallState accumulates streaming fragments for one tool call.
type toolCallState struct {
	id   string
	name string
	args strings.Builder
}

// SendStream implements provider.StreamingProvider against OpenAI's
// Chat Completions API with `stream: true`.
//
// Each non-empty content delta is forwarded to onChunk synchronously. The
// aggregated Content, Blocks, Model, and FinishReason are returned in the
// final Response. InputTokens / OutputTokens are typically zero on streaming
// responses because we don't send `stream_options.include_usage=true` —
// some third-party OpenAI-compatible servers reject it, and the cost of
// missing usage on a single turn is much smaller than losing compatibility.
func (c *Client) SendStream(ctx context.Context, req provider.Request, cb provider.StreamCallbacks) (provider.Response, error) {
	onChunk := cb.OnText
	onToolDelta := cb.OnToolDelta
	if req.Model == "" {
		return provider.Response{}, errors.New("openai: req.Model is required")
	}
	if len(req.Messages) == 0 {
		return provider.Response{}, errors.New("openai: at least one message is required")
	}

	msgs, err := toAPIMessages(req.SystemPrompt, req.Messages)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: serialize messages: %w", err)
	}

	body := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  msgs,
		Stream:    true,
		Tools:     toAPITools(req.Tools),
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: marshal stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(payload))
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: build stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.streamingHTTPClient().Do(httpReq)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: send stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var apiErr apiError
		if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Error.Message != "" {
			return provider.Response{}, fmt.Errorf(
				"openai: HTTP %d (%s): %s",
				resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message,
			)
		}
		return provider.Response{}, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var (
		contentB   strings.Builder
		result     provider.Response
		toolStates = map[int]*toolCallState{} // keyed by tool call index
		toolOrder  []int                      // preserve order
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			// Terminal sentinel. Some compatible servers omit it; we treat
			// EOF as equivalent.
			break
		}

		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return result, fmt.Errorf("openai: parse stream chunk: %w", err)
		}

		if result.Model == "" && ch.Model != "" {
			result.Model = ch.Model
		}
		if ch.Usage != nil {
			result.InputTokens = ch.Usage.PromptTokens
			result.OutputTokens = ch.Usage.CompletionTokens
		}
		if len(ch.Choices) == 0 {
			continue
		}
		choice := ch.Choices[0]
		if choice.Delta.Content != "" {
			contentB.WriteString(choice.Delta.Content)
			if onChunk != nil {
				onChunk(choice.Delta.Content)
			}
		}
		// Accumulate tool call fragments.
		for _, tc := range choice.Delta.ToolCalls {
			st, exists := toolStates[tc.Index]
			if !exists {
				st = &toolCallState{}
				toolStates[tc.Index] = st
				toolOrder = append(toolOrder, tc.Index)
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = tc.Function.Name
			}
			st.args.WriteString(tc.Function.Arguments)
			// Surface argument fragments to the caller as they arrive.
			// st.id/name may not be set yet on the very first chunk (some
			// providers send them in a later chunk) — skip those until the
			// identifiers land. The aggregate is still complete by EOF.
			if onToolDelta != nil && tc.Function.Arguments != "" && st.id != "" {
				onToolDelta(st.id, st.name, tc.Function.Arguments)
			}
		}
		if choice.FinishReason != "" {
			stopReason := choice.FinishReason
			if stopReason == "tool_calls" {
				stopReason = "tool_use"
			}
			result.StopReason = stopReason
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("openai: stream read: %w", err)
	}

	result.Content = contentB.String()

	// Build final block list: text first (if any), then tool_use blocks.
	var blocks []agent.ContentBlock
	if result.Content != "" {
		blocks = append(blocks, agent.NewTextBlock(result.Content))
	}
	for _, idx := range toolOrder {
		st := toolStates[idx]
		var input map[string]any
		if s := st.args.String(); s != "" {
			_ = json.Unmarshal([]byte(s), &input)
		}
		blocks = append(blocks, agent.NewToolUseBlock(st.id, st.name, input))
	}
	if len(blocks) > 0 {
		result.Blocks = blocks
	}

	return result, nil
}

// streamingHTTPClient returns an http.Client suitable for long-lived SSE
// reads. When the caller has injected c.HTTPClient (typically a test using
// httptest), that client is reused. Otherwise we synthesise a fresh client
// with no end-to-end Timeout so multi-minute generations complete;
// cancellation falls back to the request context.
func (c *Client) streamingHTTPClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{}
}

// Compile-time assertion: *Client also satisfies provider.StreamingProvider.
var _ provider.StreamingProvider = (*Client)(nil)
