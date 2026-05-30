package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
	"github.com/Leihb/octo-agent/internal/provider/retry"
)

// DefaultBaseURL is OpenAI's API host. The actual Chat Completions endpoint
// is BaseURL + ChatCompletionsPath. Override Client.BaseURL when pointing at
// an OpenAI-compatible third party — e.g. DeepSeek at https://api.deepseek.com,
// Kimi at https://api.moonshot.cn, vLLM, OpenRouter, Together, etc.
const DefaultBaseURL = "https://api.openai.com"

// ChatCompletionsPath is the path appended to BaseURL for every send.
const ChatCompletionsPath = "/v1/chat/completions"

// DefaultMaxTokens caps response length when a caller doesn't specify one.
// 4096 mirrors the Anthropic provider's default so behaviour stays consistent
// across backends. OpenAI itself treats max_tokens as optional (model
// maximum if omitted); we send 4096 anyway for predictability.
const DefaultMaxTokens = 4096

// DefaultStreamIdleTimeout bounds how long a streaming response may go silent
// (no bytes received) before SendStream aborts it as a stall. Chat Completions
// backends stream chunks continuously while generating, so a healthy stream
// never idles this long; 120s is generous enough to ride out a slow first token
// or a briefly congested endpoint while still catching a server that stops
// sending without closing the connection.
const DefaultStreamIdleTimeout = 120 * time.Second

// Client talks to an OpenAI-compatible Chat Completions API. Construct via
// New(); zero values are not valid because APIKey is required.
//
// BaseURL is the host + protocol-prefix only (no /v1/chat/completions
// suffix); the client appends ChatCompletionsPath itself, so pointing at
// compatible endpoints stays painless — `BaseURL = "https://api.deepseek.com"`
// and the rest works unchanged.
type Client struct {
	APIKey     string
	BaseURL    string       // optional override; defaults to DefaultBaseURL
	HTTPClient *http.Client // optional; defaults to http.Client with a 60s timeout
	Retry      retry.Policy // optional; zero value falls back to retry.Default()

	// StreamIdleTimeout overrides DefaultStreamIdleTimeout for SendStream. Zero
	// uses the default; a negative value disables the idle guard entirely.
	StreamIdleTimeout time.Duration
}

// policy returns the configured retry policy, or the package default when the
// caller left Client.Retry zero.
func (c *Client) policy() retry.Policy {
	if c.Retry.MaxAttempts > 0 {
		return c.Retry
	}
	return retry.Default()
}

// streamIdleTimeout returns the configured streaming idle timeout, or the
// package default when the caller left Client.StreamIdleTimeout zero. A
// negative value is passed through to disable the guard.
func (c *Client) streamIdleTimeout() time.Duration {
	if c.StreamIdleTimeout != 0 {
		return c.StreamIdleTimeout
	}
	return DefaultStreamIdleTimeout
}

// New constructs a Client with the given API key and the standard defaults.
// Returns an error when apiKey is empty so misconfiguration is caught at
// startup rather than at the first request.
func New(apiKey string) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("openai: API key is required")
	}
	return &Client{
		APIKey:     apiKey,
		BaseURL:    DefaultBaseURL,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "openai-completions" }

// Send implements provider.Provider against OpenAI's Chat Completions API.
//
// Non-2xx responses are decoded as apiError and wrapped into a descriptive
// error containing the HTTP status and the upstream error message.
func (c *Client) Send(ctx context.Context, req provider.Request) (provider.Response, error) {
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
		Model:          req.Model,
		MaxTokens:      req.MaxTokens,
		Messages:       msgs,
		Tools:          toAPITools(req.Tools),
		PromptCacheKey: req.CacheKey,
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	// Retry request establishment + body read on transient failures; parse the
	// (fixed) response body below. Each attempt gets a fresh body reader.
	respBody, err := retry.Do(ctx, c.policy(), func(ctx context.Context) ([]byte, retry.Decision, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(payload))
		if err != nil {
			return nil, retry.Decision{}, fmt.Errorf("openai: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return nil, retry.Decision{Retry: retry.RetryableErr(ctx, err)}, fmt.Errorf("openai: send: %w", err)
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, retry.Decision{Retry: retry.RetryableErr(ctx, err)}, fmt.Errorf("openai: read response: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
		return respBody, retry.Decision{}, nil
	})
	if err != nil {
		return provider.Response{}, err
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return provider.Response{}, fmt.Errorf("openai: decode response: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return provider.Response{}, errors.New("openai: response has no choices")
	}
	first := apiResp.Choices[0]

	// Convert tool calls to agent.ContentBlock.
	var blocks []agent.ContentBlock
	var stopReason = first.FinishReason
	if len(first.Message.ToolCalls) > 0 {
		for _, tc := range first.Message.ToolCalls {
			var input map[string]any
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			blocks = append(blocks, agent.NewToolUseBlock(tc.ID, tc.Function.Name, input))
		}
		// Normalize finish_reason to Anthropic's naming.
		if stopReason == "tool_calls" {
			stopReason = "tool_use"
		}
	}
	if first.Message.Content != "" {
		blocks = append([]agent.ContentBlock{agent.NewTextBlock(first.Message.Content)}, blocks...)
	}
	// Stash the reasoning trace on the tool_use block so it round-trips back to
	// the API on the follow-up request (required by thinking models).
	attachReasoning(blocks, first.Message.ReasoningContent)

	return provider.Response{
		Content:      first.Message.Content,
		Blocks:       blocks,
		Model:        apiResp.Model,
		StopReason:   stopReason,
		InputTokens:  apiResp.Usage.PromptTokens,
		OutputTokens: apiResp.Usage.CompletionTokens,
		// OpenAI/DeepSeek report only cached (read) input; no write count.
		CacheReadTokens: apiResp.Usage.cachedTokens(),
	}, nil
}

// endpointURL returns BaseURL + ChatCompletionsPath, applying defaults and
// trimming any trailing slash on BaseURL so the join is exactly one slash.
func (c *Client) endpointURL() string {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/") + ChatCompletionsPath
}

// toAPITools converts []agent.ToolDefinition to []apiTool (function format).
func toAPITools(defs []agent.ToolDefinition) []apiTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]apiTool, len(defs))
	for i, d := range defs {
		out[i] = apiTool{
			Type: "function",
			Function: apiFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
		}
	}
	return out
}

// toAPIMessages converts agent.Message slice into the OpenAI wire format.
//
// OpenAI carries the system prompt as the FIRST element of the messages
// array with role:"system". Tool result messages in agent History have
// role=user + Blocks containing tool_result blocks — these are exploded into
// individual role="tool" messages (one per result) because that's what the
// OpenAI protocol expects.
func toAPIMessages(systemPrompt string, in []agent.Message) ([]apiMessage, error) {
	out := make([]apiMessage, 0, len(in)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, apiMessage{Role: "system", Content: systemPrompt})
	}
	for _, m := range in {
		if m.Role == agent.RoleSystem {
			continue
		}

		// Assistant message with tool calls.
		if m.Role == agent.RoleAssistant && len(m.Blocks) > 0 {
			msg := apiMessage{Role: "assistant"}
			for _, b := range m.Blocks {
				switch b.Type {
				case "text":
					msg.Content = b.Text
				case "tool_use":
					if b.Reasoning != "" {
						msg.ReasoningContent = b.Reasoning
					}
					msg.ToolCalls = append(msg.ToolCalls, apiToolCall{
						ID:   b.ID,
						Type: "function",
						Function: apiToolCallFunction{
							Name:      b.Name,
							Arguments: marshalInput(b.Input),
						},
					})
				}
			}
			out = append(out, msg)
			continue
		}

		// User message with tool results — explode into individual role="tool"
		// messages. A trailing text block (a mid-turn "steer" the agent folded
		// into the tool_result message — see dev-docs/tui-input-modes-design.md
		// §5) can't ride on a role="tool" message, so it's emitted as a separate
		// role="user" message AFTER the tool outputs, which is the OpenAI-shaped
		// equivalent of Anthropic's [tool_result…, text] user message.
		//
		// Image blocks are folded into the user message as content parts (OpenAI
		// vision format: [{type:"text",text:"..."},{type:"image_url",image_url:{url:"data:..."}}]).
		if m.Role == agent.RoleUser && len(m.Blocks) > 0 {
			var steerText strings.Builder
			var contentParts []apiContentPart
			for _, b := range m.Blocks {
				switch b.Type {
				case "tool_result":
					out = append(out, apiMessage{
						Role:       "tool",
						Content:    b.Result,
						ToolCallID: b.ToolUseID,
					})
				case "text":
					if steerText.Len() > 0 {
						steerText.WriteString("\n\n")
					}
					steerText.WriteString(b.Text)
				case "image":
					if b.Image != nil {
						dataURL := fmt.Sprintf("data:%s;base64,%s", b.Image.MIMEType, base64.StdEncoding.EncodeToString(b.Image.Data))
						contentParts = append(contentParts, apiContentPart{
							Type: "image_url",
							ImageURL: &struct {
								URL string `json:"url"`
							}{URL: dataURL},
						})
					}
				}
			}
			if steerText.Len() > 0 {
				contentParts = append([]apiContentPart{{Type: "text", Text: steerText.String()}}, contentParts...)
			}
			if len(contentParts) > 0 {
				// Use array format only when images are present; otherwise stick to
				// plain string content for compatibility with tests and simpler wire.
				if len(contentParts) == 1 && contentParts[0].Type == "text" {
					out = append(out, apiMessage{Role: "user", Content: contentParts[0].Text})
				} else {
					out = append(out, apiMessage{Role: "user", ContentParts: contentParts})
				}
			}
			continue
		}

		// Plain text message.
		out = append(out, apiMessage{Role: string(m.Role), Content: m.Content})
	}
	return out, nil
}

// attachReasoning records a thinking model's reasoning trace on the first
// tool_use block so it survives in history and is re-sent on the follow-up
// request. No-op when reasoning is empty or there is no tool_use block — which
// keeps reasoning_content off plain text turns (some reasoning models reject it
// there).
func attachReasoning(blocks []agent.ContentBlock, reasoning string) {
	if reasoning == "" {
		return
	}
	for i := range blocks {
		if blocks[i].Type == "tool_use" {
			blocks[i].Reasoning = reasoning
			return
		}
	}
}

// marshalInput encodes a tool input map back to a compact JSON string.
// Returns "{}" on nil/empty input so the wire value is always valid JSON.
func marshalInput(input map[string]any) string {
	if len(input) == 0 {
		return "{}"
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(b)
}
