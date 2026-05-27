// Package anthropic implements the provider.Provider interface against
// Anthropic's native Messages API (POST /v1/messages).
//
// API reference: https://docs.anthropic.com/en/api/messages
package anthropic

import "encoding/json"

// cacheControl marks a prompt-cache breakpoint. Anthropic caches the entire
// prefix up to and including the block carrying this marker and reuses it on
// later requests sharing that prefix (billed at the cheaper cached rate).
// Only "ephemeral" exists today.
type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// ephemeral is the shared marker; it's immutable so sharing the pointer is safe.
var ephemeral = &cacheControl{Type: "ephemeral"}

// apiSystemBlock is one element of the array form of the `system` field.
// Anthropic accepts `system` as a plain string OR an array of text blocks;
// only the array form can carry a cache_control marker.
type apiSystemBlock struct {
	Type         string        `json:"type"` // always "text"
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// apiTool describes one tool the model may invoke, in the Anthropic wire format.
// Note: Anthropic uses "input_schema" where the agent layer uses "parameters".
type apiTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl *cacheControl  `json:"cache_control,omitempty"`
}

// apiRequest is the wire-level JSON body of POST /v1/messages.
//
// System is `any` so it serializes as either a plain string or a
// []apiSystemBlock carrying a cache breakpoint; omitempty drops it when nil.
type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    any          `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
	Stream    bool         `json:"stream,omitempty"`
	Tools     []apiTool    `json:"tools,omitempty"`
}

// apiMessage is one element of apiRequest.Messages.
//
// Anthropic accepts Content either as a plain string or as a slice of content
// blocks (text, tool_use, tool_result). We use json.RawMessage to handle both
// shapes when marshalling outgoing messages and when parsing is not needed
// (we control what we write).
type apiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// apiResponse is the wire-level JSON body of a successful 200 response.
type apiResponse struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Model      string            `json:"model"`
	Content    []apiContentBlock `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      apiUsageBlock     `json:"usage"`
}

// apiContentBlock is a single element of apiResponse.Content.
// For type=="text" only Text is populated; for type=="tool_use" ID, Name, and
// Input are populated.
type apiContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`   // tool_use
	Name  string         `json:"name,omitempty"` // tool_use
	Input map[string]any `json:"input,omitempty"`
}

// apiUsageBlock is the token-count block Anthropic returns on every message.
// The cache fields are present when prompt caching is in play:
// cache_creation_input_tokens is input written into the cache this turn
// (billed at a premium), cache_read_input_tokens is input served from the
// cache (billed cheap).
type apiUsageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// apiError is the body of an Anthropic error response (4xx/5xx).
type apiError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// streamEvent is the per-event payload Anthropic sends on the SSE stream.
// Reference: https://docs.anthropic.com/en/api/messages-streaming
//
// The union covers message_start, content_block_start, content_block_delta,
// content_block_stop, message_delta, message_stop, ping, and error events.
// We only act on a subset; the rest is ignored.
type streamEvent struct {
	Type         string              `json:"type"`
	Index        int                 `json:"index,omitempty"`         // content_block_*
	Message      *streamMessage      `json:"message,omitempty"`       // message_start
	Delta        *streamDelta        `json:"delta,omitempty"`         // content_block_delta, message_delta
	Usage        *apiUsageBlock      `json:"usage,omitempty"`         // message_delta
	ContentBlock *streamContentBlock `json:"content_block,omitempty"` // content_block_start
}

// streamContentBlock is the block metadata emitted with content_block_start.
// For text blocks only Type is set; for tool_use blocks ID and Name are also set.
type streamContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// streamMessage is the abridged message snapshot inside a message_start event.
type streamMessage struct {
	ID    string        `json:"id,omitempty"`
	Model string        `json:"model,omitempty"`
	Usage apiUsageBlock `json:"usage"`
}

// streamDelta is the per-delta payload inside content_block_delta and
// message_delta events.
//   - content_block_delta/text_delta:       Type=="text_delta",       Text holds the new bytes
//   - content_block_delta/input_json_delta: Type=="input_json_delta", PartialJSON accumulates tool input
//   - message_delta:                        StopReason set when the model stops
type streamDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}
