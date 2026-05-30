// Package openai implements provider.Provider against OpenAI's Chat
// Completions API (POST /v1/chat/completions).
//
// API reference: https://platform.openai.com/docs/api-reference/chat
//
// The package is interchangeable with any OpenAI-compatible service —
// DeepSeek's main endpoint, Kimi, Together, OpenRouter, vLLM, etc. — by
// pointing Client.BaseURL at the alternative host.
package openai

import (
	"encoding/json"
	"fmt"
)

// apiFunction describes the callable function part of a tool.
type apiFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// apiTool wraps an apiFunction with a type discriminator (always "function").
type apiTool struct {
	Type     string      `json:"type"` // "function"
	Function apiFunction `json:"function"`
}

// apiRequest is the wire-level JSON body of POST /v1/chat/completions.
type apiRequest struct {
	Model     string       `json:"model"`
	Messages  []apiMessage `json:"messages"`
	MaxTokens int          `json:"max_tokens,omitempty"`
	Stream    bool         `json:"stream,omitempty"`
	Tools     []apiTool    `json:"tools,omitempty"`
	// PromptCacheKey routes the request to a consistent prompt cache. Stable
	// across a conversation's turns → higher cache hit-rate. Omitted when empty.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// apiMessage is one element of apiRequest.Messages.
//
// For plain turns Content is a string. For assistant turns with tool calls
// ToolCalls is populated and Content may be empty. For tool result turns
// Role is "tool" and ToolCallID identifies which call is being answered.
//
// When image blocks are present, ContentParts carries the array of text/image
// content items and Content is left empty. The JSON serialization uses
// "content" for both shapes (string or array) via custom MarshalJSON/UnmarshalJSON.
type apiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"-"`
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	// ReasoningContent is the thinking trace returned by reasoning models
	// (deepseek-v4 etc.). It must be echoed back on the assistant message that
	// carries tool_calls, or the next request is rejected; omitted otherwise.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ContentParts is the array form of content used for vision/multimodal
	// messages. When non-empty it overrides Content in JSON serialization.
	ContentParts []apiContentPart `json:"-"`
}

// apiContentPart is one element of a multimodal content array.
type apiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// MarshalJSON emits Content as a string when ContentParts is empty, or as
// an array when ContentParts is populated. This matches the OpenAI wire
// format where "content" can be either shape.
func (m apiMessage) MarshalJSON() ([]byte, error) {
	type alias apiMessage // prevent recursion
	raw := struct {
		alias
		Content any `json:"content,omitempty"`
	}{
		alias: alias(m),
	}
	if len(m.ContentParts) > 0 {
		raw.Content = m.ContentParts
	} else {
		raw.Content = m.Content
	}
	return json.Marshal(raw)
}

// UnmarshalJSON reads "content" as either a string or an array of content
// parts, populating Content or ContentParts accordingly.
func (m *apiMessage) UnmarshalJSON(data []byte) error {
	type alias apiMessage
	var raw struct {
		alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = apiMessage(raw.alias)

	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		return nil
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	// Fall back to array of content parts.
	var parts []apiContentPart
	if err := json.Unmarshal(raw.Content, &parts); err == nil {
		m.ContentParts = parts
		return nil
	}
	return fmt.Errorf("apiMessage.content: unsupported type: %s", string(raw.Content))
}

// apiToolCall is one element of apiMessage.ToolCalls (assistant turns).
type apiToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"` // "function"
	Function apiToolCallFunction `json:"function"`
}

// apiToolCallFunction carries the name and JSON-encoded arguments.
type apiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string, needs json.Unmarshal to a map
}

// apiResponse is the wire-level JSON body of a successful 200 response.
type apiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   apiUsage    `json:"usage"`
}

// apiChoice is one element of apiResponse.Choices.
type apiChoice struct {
	Index        int        `json:"index"`
	Message      apiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

// apiUsage is the token-count block OpenAI returns. Field names differ from
// Anthropic (prompt_tokens / completion_tokens vs input_tokens / output_tokens);
// the adapter normalises them onto provider.Response.{InputTokens,OutputTokens}.
type apiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// Cache accounting. DeepSeek reports prompt_cache_hit_tokens /
	// prompt_cache_miss_tokens at the top level; OpenAI reports cached input
	// under prompt_tokens_details.cached_tokens. We read whichever is present.
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// cachedTokens returns the number of cached (read-hit) prompt tokens this
// usage block reports, preferring DeepSeek's explicit hit count and falling
// back to OpenAI's prompt_tokens_details.cached_tokens.
func (u apiUsage) cachedTokens() int {
	if u.PromptCacheHitTokens > 0 {
		return u.PromptCacheHitTokens
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

// apiError is the body of an OpenAI error response (4xx/5xx).
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// streamChunk is one element of an OpenAI streaming response. Each chunk
// arrives as a single SSE `data:` line carrying this JSON, terminated by a
// final `data: [DONE]` sentinel.
//
// Reference: https://platform.openai.com/docs/api-reference/chat-streaming
type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	// Usage is populated only on the final chunk when the request was sent
	// with stream_options.include_usage=true. We don't send that option so
	// most chunks have Usage zero; we keep the field so an upstream that
	// chooses to emit it anyway still gets parsed.
	Usage *apiUsage `json:"usage,omitempty"`
}

// streamChoice mirrors apiChoice but with Delta in place of Message.
type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// streamDelta carries the incremental fields of an assistant message.
// ToolCalls carries incremental tool call fragments (index-keyed).
type streamDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []streamToolCallDelta `json:"tool_calls,omitempty"`
}

// streamToolCallDelta is one incremental fragment of a tool call in a stream
// chunk. Fragments for the same call share the same Index; ID and Type are
// only present in the first fragment.
type streamToolCallDelta struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"` // "function"
	Function streamFunctionDelta `json:"function"`
}

// streamFunctionDelta carries incremental name and arguments fragments.
type streamFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
