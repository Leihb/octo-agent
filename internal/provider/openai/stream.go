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
	"github.com/Leihb/octo-agent/internal/provider/retry"
	"github.com/Leihb/octo-agent/internal/version"
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
// aggregated Content, Blocks, Model, FinishReason, and token usage are returned
// in the final Response. We send `stream_options.include_usage=true` so the
// server emits a terminal usage chunk: DashScope (and real OpenAI) report no
// usage at all on a stream without it. Servers that omit the usage chunk anyway
// just leave the token counts at zero — no error.
func (c *Client) SendStream(ctx context.Context, req provider.Request, cb provider.StreamCallbacks) (provider.Response, error) {
	onChunk := cb.OnText
	onToolDelta := cb.OnToolDelta
	onThinking := cb.OnThinking
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
		Model:           req.Model,
		MaxTokens:       req.MaxTokens,
		Messages:        msgs,
		Stream:          true,
		StreamOptions:   &apiStreamOptions{IncludeUsage: true},
		Tools:           toAPITools(req.Tools),
		PromptCacheKey:  req.CacheKey,
		ReasoningEffort: req.ReasoningEffort,
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: marshal stream request: %w", err)
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
			return nil, retry.Decision{}, fmt.Errorf("openai: build stream request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("User-Agent", version.UserAgent())
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

		resp, err := c.streamingHTTPClient().Do(httpReq)
		if err != nil {
			return nil, retry.Decision{Retry: retry.RetryableErr(ctx, err)}, fmt.Errorf("openai: send stream: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			dec := retry.Decision{Retry: retry.RetryableStatus(resp.StatusCode), RetryAfter: retry.RetryAfterHeader(resp.Header)}
			var apiErr apiError
			if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Error.Message != "" {
				return nil, dec, fmt.Errorf(
					"openai: HTTP %d (%s): %s",
					resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message,
				)
			}
			return nil, dec, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
		}
		return resp, retry.Decision{}, nil
	})
	if err != nil {
		return provider.Response{}, err
	}
	defer resp.Body.Close()

	var (
		contentB   strings.Builder
		reasoningB strings.Builder
		result     provider.Response
		toolStates = map[int]*toolCallState{} // keyed by tool call index
		toolOrder  []int                      // preserve order
	)

	// Guard the body read against a mid-stream stall: if the server stops
	// sending for longer than the idle window, cancelStream tears down the
	// request and the scanner surfaces retry.ErrStreamIdle instead of blocking
	// forever. The window resets on every received chunk.
	streamBody := retry.IdleTimeoutReader(resp.Body, c.streamIdleTimeout(), cancelStream)
	scanner := bufio.NewScanner(streamBody)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// Per the SSE spec the single space after "data:" is optional. OpenAI
		// and DeepSeek send "data: {...}" but some compatible backends omit
		// the space; strip the prefix and at most one leading space.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
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
			result.InputTokens = ch.Usage.nonCachedInput()
			result.OutputTokens = ch.Usage.CompletionTokens
			result.CacheReadTokens = ch.Usage.cachedTokens()
		}
		if len(ch.Choices) == 0 {
			continue
		}
		choice := ch.Choices[0]
		// Some compatible backends (Kimi) embed usage inside the choice object
		// rather than at the chunk level. Prefer chunk-level but fall back.
		if choice.Usage != nil {
			result.InputTokens = choice.Usage.nonCachedInput()
			result.OutputTokens = choice.Usage.CompletionTokens
			result.CacheReadTokens = choice.Usage.cachedTokens()
		}
		if choice.Delta.Content != "" {
			contentB.WriteString(choice.Delta.Content)
			if onChunk != nil {
				onChunk(choice.Delta.Content)
			}
		}
		// Reasoning trace from thinking models streams in its own delta field.
		// Always accumulate it (attachReasoning pins it to the tool_use block so
		// it round-trips in history); surface it to onThinking too when the
		// caller wants the trace displayed.
		if choice.Delta.ReasoningContent != "" {
			reasoningB.WriteString(choice.Delta.ReasoningContent)
			if onThinking != nil {
				onThinking(choice.Delta.ReasoningContent)
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
			switch stopReason {
			case "tool_calls":
				stopReason = "tool_use"
			case "length":
				// Normalise the output-cap truncation signal to the canonical
				// sentinel the agent loop checks (matches Anthropic's
				// "max_tokens"), so truncation recovery is provider-agnostic.
				stopReason = "max_tokens"
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
	attachReasoning(blocks, reasoningB.String())
	if len(blocks) > 0 {
		result.Blocks = blocks
	}

	return result, nil
}

// streamingHTTPClient returns an http.Client suitable for long-lived SSE
// reads. An injected c.HTTPClient (production New() sets one with a 60s
// Timeout; tests inject httptest clients) is reused for its Transport / jar /
// redirect config, but its end-to-end Timeout is dropped: a Client.Timeout
// applies to the whole request including the streaming body read, so a
// legitimate multi-minute generation would be killed mid-stream with
// "context deadline exceeded". Streaming cancellation instead comes from the
// request context plus the per-read idle timeout in SendStream
// (retry.IdleTimeoutReader). With no injected client we synthesise a fresh
// one, which already has no Timeout.
func (c *Client) streamingHTTPClient() *http.Client {
	if c.HTTPClient != nil {
		clone := *c.HTTPClient
		clone.Timeout = 0
		return &clone
	}
	return &http.Client{}
}

// Compile-time assertion: *Client also satisfies provider.StreamingProvider.
var _ provider.StreamingProvider = (*Client)(nil)
