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
	"github.com/Leihb/octo-agent/internal/provider/retry"
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
	signature strings.Builder // thinking blocks
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
	onThinking := cb.OnThinking
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
		Messages:  msgs,
		Stream:    true,
	}
	cacheableRequest(&body, req.SystemPrompt, toAPITools(req.Tools))
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}
	applyThinking(&body, req.ThinkingBudget)

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: marshal stream request: %w", err)
	}

	// streamCtx lets the idle watchdog (retry.IdleTimeoutReader, below) abort a
	// stalled stream by cancelling the request: the HTTP request is built with
	// streamCtx inside the attempt, so cancelling it tears the connection down
	// and unblocks the blocked body read.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	// Retry only request establishment (build → send → status check). Once the
	// body below starts streaming we can't retry without duplicating emitted
	// tokens. On success the attempt returns the open response for scanning.
	resp, err := retry.Do(streamCtx, c.policy(), func(ctx context.Context) (*http.Response, retry.Decision, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(payload))
		if err != nil {
			return nil, retry.Decision{}, fmt.Errorf("anthropic: build stream request: %w", err)
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
			return nil, retry.Decision{Retry: retry.RetryableErr(ctx, err)}, fmt.Errorf("anthropic: send stream: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			dec := retry.Decision{Retry: retry.RetryableStatus(resp.StatusCode), RetryAfter: retry.RetryAfterHeader(resp.Header)}
			var apiErr apiError
			if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Error.Message != "" {
				return nil, dec, fmt.Errorf(
					"anthropic: HTTP %d (%s): %s",
					resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message,
				)
			}
			return nil, dec, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(respBody))
		}
		return resp, retry.Decision{}, nil
	})
	if err != nil {
		return provider.Response{}, err
	}
	defer resp.Body.Close()

	var (
		result     provider.Response
		accByIndex = map[int]*blockAccumulator{}
		// ordered list of block indices to preserve emission order
		blockOrder []int
	)

	// Guard the body read against a mid-stream stall: if the server stops
	// sending for longer than the idle window, cancelStream tears down the
	// request and the scanner surfaces retry.ErrStreamIdle instead of blocking
	// forever. The window resets on every received chunk.
	streamBody := retry.IdleTimeoutReader(resp.Body, c.streamIdleTimeout(), cancelStream)
	scanner := bufio.NewScanner(streamBody)
	// Default Scanner buffer is 64 KiB which can be undersized for long
	// `data:` lines if Anthropic ever ships an unusually large delta.
	// Cap at 1 MiB.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// SSE frames: "data:<json>" lines, separated by blank lines. Per the
		// SSE spec the single space after "data:" is optional — Anthropic
		// sends "data: {...}" but some compatible backends (Kimi) send
		// "data:{...}". Strip the prefix and at most one leading space.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
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
				// Cache counts arrive in the initial usage block.
				result.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
				result.CacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
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
			case "thinking_delta":
				acc.text.WriteString(ev.Delta.Thinking)
				if onThinking != nil && ev.Delta.Thinking != "" {
					onThinking(ev.Delta.Thinking)
				}
			case "signature_delta":
				acc.signature.WriteString(ev.Delta.Signature)
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
				// Anthropic reports cache accounting in message_start, but some
				// compatible backends (Kimi) only report it in the final
				// message_delta. Take non-zero cache values here so we don't
				// miss those hits; the >0 guard avoids clobbering message_start's
				// counts with a trailing zero. A cache_read here also means this
				// backend sent full final accounting, so adopt its input_tokens
				// (the non-cached remainder) too.
				if ev.Usage.CacheReadInputTokens > 0 {
					result.CacheReadTokens = ev.Usage.CacheReadInputTokens
					result.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.CacheCreationInputTokens > 0 {
					result.CacheWriteTokens = ev.Usage.CacheCreationInputTokens
				}
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
		case "thinking":
			// Thinking text accumulates in acc.text; keep it out of the visible
			// answer (textB) but preserve the block + signature for the
			// tool-call round-trip.
			blocks = append(blocks, agent.NewThinkingBlock(acc.text.String(), acc.signature.String()))
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
