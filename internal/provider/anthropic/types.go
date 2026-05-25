// Package anthropic implements the provider.Provider interface against
// Anthropic's native Messages API (POST /v1/messages).
//
// API reference: https://docs.anthropic.com/en/api/messages
package anthropic

// apiRequest is the wire-level JSON body of POST /v1/messages.
//
// Only the fields M1.2 needs are wired up. Tool definitions, streaming,
// metadata, and the rest land in M2.
type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
}

// apiMessage is one element of apiRequest.Messages.
//
// Anthropic accepts either a plain string Content or a slice of content blocks
// (text, image, tool_use, tool_result). M1.2 always sends strings; the
// adapter converts agent.Message → apiMessage one-to-one.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

// apiContentBlock is a single element of apiResponse.Content. M1.2 only
// reads text blocks; tool_use blocks are returned in later milestones once
// the tool layer can dispatch them.
type apiContentBlock struct {
	Type string `json:"type"`           // "text" or "tool_use" in M2+
	Text string `json:"text,omitempty"` // populated when Type == "text"
}

// apiUsageBlock is the token-count block Anthropic returns on every message.
type apiUsageBlock struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// apiError is the body of an Anthropic error response (4xx/5xx).
type apiError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
