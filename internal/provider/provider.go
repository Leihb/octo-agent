// Package provider defines the contract every LLM backend implements. M1.2
// only exposes a non-streaming Send method; streaming and tool-call
// callbacks land in M2 alongside the Anthropic / OpenAI / Bedrock
// aggregators.
package provider

import (
	"context"

	"github.com/Leihb/octo/internal/agent"
)

// Request bundles everything a provider needs to produce one assistant turn.
// SystemPrompt is carried out of band rather than as a Message because
// Anthropic's API treats it as a top-level field; OpenAI providers convert
// it back into a leading role:"system" message in their adapter.
type Request struct {
	Model        string
	SystemPrompt string
	Messages     []agent.Message
	MaxTokens    int
}

// Response is the assistant reply produced by Send. Content is plain text in
// M1.2 — when M2 adds tool calls the type expands to carry a slice of content
// blocks, with Content remaining as a convenience join for the text portion.
type Response struct {
	Content      string
	Model        string // echoed by the API; useful when "claude-3-5-sonnet-latest" resolves to a dated name
	StopReason   string // e.g. "end_turn", "max_tokens", "stop_sequence"
	InputTokens  int
	OutputTokens int
}

// Provider is the per-backend abstraction. Implementations are kept under
// internal/provider/<name>/ (e.g. anthropic, openai, bedrock).
type Provider interface {
	// Name returns a stable identifier for the provider, used in logs and
	// telemetry — e.g. "anthropic-messages", "openai-completions".
	Name() string

	// Send sends one Request and returns one Response. It must respect
	// ctx cancellation and surface HTTP / decode errors as a wrapped
	// error.
	Send(ctx context.Context, req Request) (Response, error)
}
