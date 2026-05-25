// Package openai implements provider.Provider against OpenAI's Chat
// Completions API (POST /v1/chat/completions).
//
// API reference: https://platform.openai.com/docs/api-reference/chat
//
// The package is interchangeable with any OpenAI-compatible service —
// DeepSeek's main endpoint, Kimi, Together, OpenRouter, vLLM, etc. — by
// pointing Client.BaseURL at the alternative host.
package openai

// apiRequest is the wire-level JSON body of POST /v1/chat/completions.
//
// Only the fields M2 needs are wired up. Tool calls, response_format,
// reasoning_effort, and streaming arrive in later milestones.
type apiRequest struct {
	Model     string       `json:"model"`
	Messages  []apiMessage `json:"messages"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

// apiMessage is one element of apiRequest.Messages.
//
// OpenAI accepts Content either as a plain string or as an array of content
// parts (for vision). M2 always sends strings; image attachments arrive in
// a later milestone alongside the tool-use work.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// apiResponse is the wire-level JSON body of a successful 200 response.
type apiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   apiUsage    `json:"usage"`
}

// apiChoice is one element of apiResponse.Choices. M2 always reads the first.
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
}

// apiError is the body of an OpenAI error response (4xx/5xx).
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}
