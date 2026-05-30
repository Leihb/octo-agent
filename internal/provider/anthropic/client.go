package anthropic

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

// DefaultBaseURL is Anthropic's API base. The actual Messages endpoint is
// BaseURL + "/v1/messages". Override Client.BaseURL when pointing at an
// Anthropic-compatible third-party (DeepSeek, Kimi, OpenRouter's Anthropic
// shim, …) — for example DeepSeek exposes the protocol at
// `https://api.deepseek.com/anthropic`.
const DefaultBaseURL = "https://api.anthropic.com"

// MessagesPath is the path appended to BaseURL for every send.
const MessagesPath = "/v1/messages"

// DefaultAPIVersion is the value sent as the `anthropic-version` header.
// Pinned to the GA version used by the Messages API since 2023-06-01; bump
// only when a new feature requires it.
const DefaultAPIVersion = "2023-06-01"

// DefaultMaxTokens caps response length when a caller doesn't specify one.
// 4096 is generous for chat-style replies and well below the model ceilings.
const DefaultMaxTokens = 4096

// DefaultStreamIdleTimeout bounds how long a streaming response may go silent
// (no bytes received) before SendStream aborts it as a stall. The Messages API
// emits periodic `ping` events to keep streams alive, so a healthy stream never
// idles this long; 120s is generous enough to ride out a slow first token or a
// briefly congested endpoint while still catching a server that stops sending
// without closing the connection.
const DefaultStreamIdleTimeout = 120 * time.Second

// Client talks to an Anthropic-compatible Messages API. Construct via New();
// zero values are not valid because APIKey is required.
//
// BaseURL is the host + protocol-prefix only (no /v1/messages suffix); the
// client appends MessagesPath itself. This makes pointing at compatible
// endpoints painless — set `BaseURL = "https://api.deepseek.com/anthropic"`
// and the rest works unchanged.
type Client struct {
	APIKey     string
	BaseURL    string       // optional override; defaults to DefaultBaseURL
	APIVersion string       // optional override; defaults to DefaultAPIVersion
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
		return nil, errors.New("anthropic: API key is required")
	}
	return &Client{
		APIKey:     apiKey,
		BaseURL:    DefaultBaseURL,
		APIVersion: DefaultAPIVersion,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "anthropic-messages" }

// Send implements provider.Provider against Anthropic's Messages API.
//
// Non-2xx responses are decoded as apiError and wrapped into a descriptive
// error containing the HTTP status and the upstream error message.
func (c *Client) Send(ctx context.Context, req provider.Request) (provider.Response, error) {
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
	}
	cacheableRequest(&body, req.SystemPrompt, toAPITools(req.Tools))
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}
	applyThinking(&body, req.ThinkingBudget)

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	// Retry the request on transient failures (429/5xx/529/network). The
	// payload is fixed across attempts; each attempt gets a fresh body reader.
	return retry.Do(ctx, c.policy(), func(ctx context.Context) (provider.Response, retry.Decision, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(payload))
		if err != nil {
			return provider.Response{}, retry.Decision{}, fmt.Errorf("anthropic: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", c.APIKey)
		apiVer := c.APIVersion
		if apiVer == "" {
			apiVer = DefaultAPIVersion
		}
		httpReq.Header.Set("anthropic-version", apiVer)

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return provider.Response{}, retry.Decision{Retry: retry.RetryableErr(ctx, err)}, fmt.Errorf("anthropic: send: %w", err)
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return provider.Response{}, retry.Decision{Retry: retry.RetryableErr(ctx, err)}, fmt.Errorf("anthropic: read response: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			dec := retry.Decision{Retry: retry.RetryableStatus(resp.StatusCode), RetryAfter: retry.RetryAfterHeader(resp.Header)}
			var apiErr apiError
			if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Error.Message != "" {
				return provider.Response{}, dec, fmt.Errorf(
					"anthropic: HTTP %d (%s): %s",
					resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message,
				)
			}
			return provider.Response{}, dec, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		var apiResp apiResponse
		if err := json.Unmarshal(respBody, &apiResp); err != nil {
			return provider.Response{}, retry.Decision{}, fmt.Errorf("anthropic: decode response: %w", err)
		}

		blocks := fromAPIContentBlocks(apiResp.Content)
		return provider.Response{
			Content:          joinTextBlocks(apiResp.Content),
			Blocks:           blocks,
			Model:            apiResp.Model,
			StopReason:       apiResp.StopReason,
			InputTokens:      apiResp.Usage.InputTokens,
			OutputTokens:     apiResp.Usage.OutputTokens,
			CacheReadTokens:  apiResp.Usage.CacheReadInputTokens,
			CacheWriteTokens: apiResp.Usage.CacheCreationInputTokens,
		}, retry.Decision{}, nil
	})
}

// endpointURL returns BaseURL + MessagesPath, applying defaults and trimming
// any trailing slash on BaseURL so the join is exactly one slash.
func (c *Client) endpointURL() string {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/") + MessagesPath
}

// buildSystem returns the value for the request's `system` field, placing a
// cache breakpoint on it when non-empty. Because the cache prefix order is
// tools → system → messages, a breakpoint on the system block also caches the
// (stable, every-turn-identical) tools array that precedes it — capturing the
// bulk of the per-turn input-token cost of an agentic loop. Returns nil for an
// empty prompt so the field is omitted.
func buildSystem(prompt string) any {
	if prompt == "" {
		return nil
	}
	return []apiSystemBlock{{Type: "text", Text: prompt, CacheControl: ephemeral}}
}

// markToolsCacheable puts a cache breakpoint on the LAST tool so the tools
// array is cached even with no system prompt to anchor on. No-op when there
// are no tools.
func markToolsCacheable(tools []apiTool) []apiTool {
	if len(tools) > 0 {
		tools[len(tools)-1].CacheControl = ephemeral
	}
	return tools
}

// cacheableRequest places all cache breakpoints on a request: the system/
// tools prefix (via buildSystem / markToolsCacheable) and the conversation
// history (via markMessagesCacheable). Shared by Send and SendStream so both
// paths cache identically. body.Messages must already be populated.
func cacheableRequest(body *apiRequest, systemPrompt string, tools []apiTool) {
	body.System = buildSystem(systemPrompt)
	if body.System == nil {
		tools = markToolsCacheable(tools) // no system block to anchor on
	}
	body.Tools = tools
	markMessagesCacheable(body.Messages)
}

// markMessagesCacheable places cache breakpoints on the last two messages so
// the conversation-history prefix is cached turn-over-turn (not just the
// static system+tools prefix). Two consecutive markers — rather than one —
// keep a cache anchor alive across the "old tail / new tail" boundary as
// history grows and if the final message is dropped on a retry; the older of
// the two still matches the prior request's prefix. The system breakpoint plus
// these two stays within Anthropic's 4-breakpoint budget.
func markMessagesCacheable(msgs []apiMessage) {
	n := len(msgs)
	for i := n - 1; i >= 0 && i >= n-2; i-- {
		msgs[i] = markMessageCacheable(msgs[i])
	}
}

// markMessageCacheable returns m with a cache_control breakpoint on its last
// content block. A bare-string content is promoted to a single text block so
// it can carry the marker (Anthropic only allows cache_control on blocks). On
// any unexpected shape the message is returned unchanged.
func markMessageCacheable(m apiMessage) apiMessage {
	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		var s string
		if json.Unmarshal(m.Content, &s) != nil {
			return m // neither array nor string — leave it alone
		}
		blocks = []map[string]any{{"type": "text", "text": s}}
	}
	if len(blocks) == 0 {
		return m
	}
	blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return m
	}
	m.Content = raw
	return m
}

// applyThinking enables extended thinking on the request when budget > 0.
// Anthropic requires max_tokens to exceed budget_tokens, so it bumps
// max_tokens to leave room for the answer on top of the reasoning budget.
func applyThinking(body *apiRequest, budget int) {
	if budget <= 0 {
		return
	}
	body.Thinking = &apiThinking{Type: "enabled", BudgetTokens: budget}
	if body.MaxTokens <= budget {
		body.MaxTokens = budget + DefaultMaxTokens
	}
}

// toAPITools converts []agent.ToolDefinition to []apiTool.
// Anthropic uses "input_schema" where the agent layer uses "parameters".
func toAPITools(defs []agent.ToolDefinition) []apiTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]apiTool, len(defs))
	for i, d := range defs {
		out[i] = apiTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.Parameters,
		}
	}
	return out
}

// toAPIMessages converts agent.Message slice into the Anthropic wire format.
// The system role is filtered out here because Anthropic carries it as a
// top-level Request.SystemPrompt — sending role:"system" inside the
// messages array would 400.
//
// Messages with Blocks are serialized as a []apiContentBlock JSON array;
// plain-text messages are serialized as a JSON string for compatibility.
func toAPIMessages(in []agent.Message) ([]apiMessage, error) {
	out := make([]apiMessage, 0, len(in))
	for _, m := range in {
		if m.Role == agent.RoleSystem {
			continue
		}

		if len(m.Blocks) > 0 {
			// Serialize as a content-block array.
			blocks, err := marshalBlocks(m.Blocks)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(blocks)
			if err != nil {
				return nil, err
			}
			out = append(out, apiMessage{Role: string(m.Role), Content: raw})
		} else {
			// Plain string content.
			raw, err := json.Marshal(m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, apiMessage{Role: string(m.Role), Content: raw})
		}
	}
	return out, nil
}

// marshalBlocks converts agent.ContentBlock slice to []apiContentBlock.
// tool_result blocks are serialized with a nested "content" string field
// per the Anthropic wire format.
func marshalBlocks(blocks []agent.ContentBlock) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, map[string]any{
				"type": "text",
				"text": b.Text,
			})
		case "thinking":
			// Replayed verbatim with its signature; required before tool_use
			// when thinking is enabled, or the API rejects the request.
			out = append(out, map[string]any{
				"type":      "thinking",
				"thinking":  b.Thinking,
				"signature": b.Signature,
			})
		case "tool_use":
			m := map[string]any{
				"type":  "tool_use",
				"id":    b.ID,
				"name":  b.Name,
				"input": b.Input,
			}
			out = append(out, m)
		case "tool_result":
			m := map[string]any{
				"type":        "tool_result",
				"tool_use_id": b.ToolUseID,
				"content":     b.Result,
			}
			if b.IsError {
				m["is_error"] = true
			}
			out = append(out, m)
		case "image":
			// Anthropic image block: base64-encoded data with source type.
			if b.Image == nil {
				return nil, fmt.Errorf("anthropic: image block missing Image data")
			}
			out = append(out, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.Image.MIMEType,
					"data":       encodeBase64(b.Image.Data),
				},
			})
		default:
			return nil, fmt.Errorf("anthropic: unknown block type %q", b.Type)
		}
	}
	return out, nil
}

// encodeBase64 returns a standard base64 string.
func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// fromAPIContentBlocks converts the response content blocks to agent.ContentBlock.
func fromAPIContentBlocks(blocks []apiContentBlock) []agent.ContentBlock {
	out := make([]agent.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, agent.NewTextBlock(b.Text))
		case "tool_use":
			out = append(out, agent.NewToolUseBlock(b.ID, b.Name, b.Input))
		case "thinking":
			out = append(out, agent.NewThinkingBlock(b.Thinking, b.Signature))
		}
	}
	return out
}

// joinTextBlocks concatenates the text from every "text" content block.
// Non-text blocks (tool_use) are skipped.
func joinTextBlocks(blocks []apiContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
