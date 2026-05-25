package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Leihb/octo/internal/agent"
	"github.com/Leihb/octo/internal/provider"
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

	body := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  toAPIMessages(req.SystemPrompt, req.Messages),
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = DefaultMaxTokens
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(payload))
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: send: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.Response{}, fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiError
		if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Error.Message != "" {
			return provider.Response{}, fmt.Errorf(
				"openai: HTTP %d (%s): %s",
				resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message,
			)
		}
		return provider.Response{}, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return provider.Response{}, fmt.Errorf("openai: decode response: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return provider.Response{}, errors.New("openai: response has no choices")
	}
	first := apiResp.Choices[0]

	return provider.Response{
		Content:      first.Message.Content,
		Model:        apiResp.Model,
		StopReason:   first.FinishReason,
		InputTokens:  apiResp.Usage.PromptTokens,
		OutputTokens: apiResp.Usage.CompletionTokens,
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

// toAPIMessages converts agent.Message slice into the OpenAI wire format.
//
// OpenAI carries the system prompt as the FIRST element of the messages
// array with role:"system" — the opposite direction from Anthropic, which
// uses a separate top-level field. If a non-empty systemPrompt is passed we
// prepend it; any role:"system" entries already in `in` are dropped so the
// agent's SystemPrompt is the single source of truth.
func toAPIMessages(systemPrompt string, in []agent.Message) []apiMessage {
	out := make([]apiMessage, 0, len(in)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, apiMessage{Role: "system", Content: systemPrompt})
	}
	for _, m := range in {
		if m.Role == agent.RoleSystem {
			continue
		}
		out = append(out, apiMessage{Role: string(m.Role), Content: m.Content})
	}
	return out
}
