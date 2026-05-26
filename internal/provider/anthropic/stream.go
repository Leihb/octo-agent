package anthropic

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

// blockAccumulator holds in-progress state for a single content block while
// the SSE stream is open. Text blocks accumulate their text delta-by-delta;
// tool_use blocks accumulate input_json_delta fragments that are JSON-parsed
// once the stream for that block closes (content_block_stop).
type blockAccumulator struct {
	blockType string
	text      strings.Builder
	id        string
	name      string
	inputJSON strings.Builder
}

// SendStream implements provider.StreamingProvider against Anthropic's
// Messages API with `stream: true`.
//
// Each text delta (content_block_delta of type text_delta) is forwarded to
// onChunk synchronously. The aggregated Content, Blocks, Model, StopReason,
// and token usage are returned in the final Response.
//
// Cancellation is via ctx; no HTTP-level timeout is set, because streaming
// responses can legitimately run for minutes.
func (c *Client) SendStream(ctx context.Context, req provider.Request, cb provider.StreamCallbacks) (provider.Response, error) {
	onChunk := cb.OnText
	onToolDelta := cb.OnToolDelta
	if req.Model == "" {
		return provider.Response{}, errors.New("anthropic: req.Model is required")
	}
	if len(req.Messages) == 0 {
		return provider.Response{}, errors.New("anthropic: at least one message is required")
	}

	msgs, err := toAPIMessages(req.Messages)
	if err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: serialize messages: %w", err)
	}

	body := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    req.SystemPrompt,
		Messages:  msgs,
		Stream:    true,
		Tools:     toAPITools(req.Tools),
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: marshal stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(payload))
	if err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: build stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", c.APIKey)
	apiVer := c.APIVersion
	if apiVer == "" {
		apiVer = DefaultAPIVersion
	}
	httpReq.Header.Set("anthropic-version", apiVer)

	resp, err := c.streamingHTTPClient().Do(httpReq)
	if err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: send stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var apiErr apiError
		if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Error.Message != "" {
			return provider.Response{}, fmt.Errorf(
				"anthropic: HTTP %d (%s): %s",
				resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message,
			)
		}
		return provider.Response{}, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var (
		result     provider.Response
		accByIndex = map[int]*blockAccumulator{}
		// ordered list of block indices to preserve emission order
		blockOrder []int
	)

	scanner := bufio.NewScanner(resp.Body)
	// Default Scanner buffer is 64 KiB which can be undersized for long
	// `data:` lines if Anthropic ever ships an unusually large delta.
	// Cap at 1 MiB.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// SSE frames: "data: <json>" lines, separated by blank lines.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return result, fmt.Errorf("anthropic: parse stream event: %w", err)
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				result.Model = ev.Message.Model
				result.InputTokens = ev.Message.Usage.InputTokens
				result.OutputTokens = ev.Message.Usage.OutputTokens
			}

		case "content_block_start":
			acc := &blockAccumulator{}
			if ev.ContentBlock != nil {
				acc.blockType = ev.ContentBlock.Type
				acc.id = ev.ContentBlock.ID
				acc.name = ev.ContentBlock.Name
			}
			accByIndex[ev.Index] = acc
			blockOrder = append(blockOrder, ev.Index)

		case "content_block_delta":
			acc, ok := accByIndex[ev.Index]
			if !ok {
				// No content_block_start for this index — auto-create a text
				// accumulator so we don't lose text deltas from providers that
				// omit the start event.
				acc = &blockAccumulator{blockType: "text"}
				accByIndex[ev.Index] = acc
				blockOrder = append(blockOrder, ev.Index)
			}
			if ev.Delta == nil {
				break
			}
			switch ev.Delta.Type {
			case "text_delta":
				acc.text.WriteString(ev.Delta.Text)
				if onChunk != nil && ev.Delta.Text != "" {
					onChunk(ev.Delta.Text)
				}
			case "input_json_delta":
				acc.inputJSON.WriteString(ev.Delta.PartialJSON)
				// Stream the JSON fragment to the caller; the
				// fully-aggregated input map is still parsed at
				// content_block_stop. acc.id/name are set when the
				// content_block_start event for this index landed —
				// they're present for tool_use blocks by the time
				// any input_json_delta arrives.
				if onToolDelta != nil && ev.Delta.PartialJSON != "" && acc.blockType == "tool_use" {
					onToolDelta(acc.id, acc.name, ev.Delta.PartialJSON)
				}
			}

		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				result.StopReason = ev.Delta.StopReason
			}
			// Output token count refines as the stream progresses; the
			// final message_delta carries the authoritative total.
			if ev.Usage != nil {
				result.OutputTokens = ev.Usage.OutputTokens
			}

		case "error":
			var apiErr apiError
			_ = json.Unmarshal([]byte(data), &apiErr)
			return result, fmt.Errorf("anthropic: stream error: %s", apiErr.Error.Message)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("anthropic: stream read: %w", err)
	}

	// Build the final block list in order.
	var textB strings.Builder
	blocks := make([]agent.ContentBlock, 0, len(blockOrder))
	for _, idx := range blockOrder {
		acc, ok := accByIndex[idx]
		if !ok {
			continue
		}
		switch acc.blockType {
		case "text":
			t := acc.text.String()
			textB.WriteString(t)
			blocks = append(blocks, agent.NewTextBlock(t))
		case "tool_use":
			var input map[string]any
			if s := acc.inputJSON.String(); s != "" {
				_ = json.Unmarshal([]byte(s), &input)
			}
			blocks = append(blocks, agent.NewToolUseBlock(acc.id, acc.name, input))
		}
	}

	result.Content = textB.String()
	result.Blocks = blocks
	return result, nil
}

// streamingHTTPClient returns an http.Client suitable for long-lived SSE
// reads. When the caller has injected c.HTTPClient (typically a test using
// httptest), that client is reused — httptest responses are fast enough
// that their default behaviour is fine. Otherwise we synthesise a fresh
// client with NO end-to-end Timeout so multi-minute generations complete;
// cancellation falls back to the request context.
func (c *Client) streamingHTTPClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{}
}

// Compile-time assertion: *Client also satisfies provider.StreamingProvider.
var _ provider.StreamingProvider = (*Client)(nil)
