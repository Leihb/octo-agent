// Package provider defines the contract every LLM backend implements.
//
// Two interfaces live here:
//   - Provider — non-streaming Send. Required.
//   - StreamingProvider — adds SendStream, which delivers the assistant
//     reply chunk-by-chunk via a callback. Optional; callers type-assert
//     to detect support and fall back to Provider.Send if absent.
//
// Tool-call dispatch lands in a later milestone alongside the per-provider
// content-block aggregators.
package provider

import (
	"context"

	"github.com/Leihb/octo-agent/internal/agent"
)

// Request bundles everything a provider needs to produce one assistant turn.
// SystemPrompt is carried out of band rather than as a Message because
// Anthropic's API treats it as a top-level field; OpenAI providers convert
// it back into a leading role:"system" message in their adapter.
//
// Tools, when non-empty, declares the tools the model may invoke. Providers
// that don't support tool calling should ignore this field (the model will
// simply never emit tool_use blocks).
type Request struct {
	Model        string
	SystemPrompt string
	Messages     []agent.Message
	MaxTokens    int
	Tools        []agent.ToolDefinition

	// CacheKey is an optional, stable-per-conversation prompt-cache hint.
	// Providers with an explicit cache key (OpenAI's prompt_cache_key)
	// forward it; others (Anthropic, which uses inline cache_control
	// breakpoints) ignore it. Empty means "no hint".
	CacheKey string

	// ThinkingBudget, when > 0, enables extended thinking and sets the token
	// budget for the reasoning trace. Only the Anthropic provider honors it
	// (Claude / Kimi k2.6 via the thinking request field); others ignore it.
	ThinkingBudget int

	// ReasoningEffort, when non-empty ("low" | "medium" | "high"), is forwarded
	// as the OpenAI-protocol reasoning_effort request field. Only the OpenAI
	// provider honors it; the Anthropic provider expresses reasoning intensity
	// through ThinkingBudget instead and ignores this field.
	ReasoningEffort string
}

// Response is the assistant reply produced by Send.
//
// Content is the concatenated text from all text blocks — a convenience join
// for callers that only care about the prose portion of the reply.
//
// Blocks holds the full list of content blocks in the order the model emitted
// them. This includes both text and tool_use blocks; callers that drive an
// agentic loop should inspect Blocks to find tool_use blocks and dispatch them.
type Response struct {
	Content      string
	Blocks       []agent.ContentBlock // full content-block list (text + tool_use)
	Model        string               // echoed by the API; useful when "claude-3-5-sonnet-latest" resolves to a dated name
	StopReason   string               // "end_turn" | "tool_use" | "max_tokens" | "stop_sequence"
	InputTokens  int
	OutputTokens int

	// Cache accounting, when the backend reports it. CacheReadTokens is input
	// served from a prompt cache (cheap); CacheWriteTokens is input written
	// into the cache this turn (Anthropic only — OpenAI/DeepSeek don't report
	// a write count). Both zero when the backend reports no cache info.
	CacheReadTokens  int
	CacheWriteTokens int
}

// Provider is the per-backend abstraction. Implementations are kept under
// internal/provider/<name>/ (e.g. anthropic, openai).
type Provider interface {
	// Name returns a stable identifier for the provider, used in logs and
	// telemetry — e.g. "anthropic-messages", "openai-completions".
	Name() string

	// Send sends one Request and returns one Response. It must respect
	// ctx cancellation and surface HTTP / decode errors as a wrapped
	// error.
	Send(ctx context.Context, req Request) (Response, error)
}

// StreamCallbacks bundles every per-event callback a streaming provider may
// fire. Adding a new event class (citations, thinking blocks, image deltas)
// in the future means adding another field here — no interface bump.
//
// All fields are nil-safe; implementations should skip the call when the
// corresponding callback is nil.
type StreamCallbacks struct {
	// OnText is invoked for each text fragment of the assistant's reply.
	OnText func(textDelta string)

	// OnToolDelta is invoked while a tool_use block's input arguments are
	// streaming in. partialJSON is the raw fragment (NOT a running total);
	// fragments concatenate to form the final JSON object. toolID and
	// toolName identify the tool call (both known once the block starts).
	OnToolDelta func(toolID, toolName, partialJSON string)

	// OnThinking is invoked for each fragment of a reasoning model's
	// extended-thinking trace (Anthropic thinking_delta). Fragments concatenate
	// to form the full trace. Fires before any OnText/OnToolDelta for the turn,
	// since the thinking block streams first. Nil-safe.
	OnThinking func(thinkingDelta string)
}

// StreamingProvider extends Provider with the ability to stream the
// assistant reply chunk-by-chunk via callbacks.
//
// Implementations must invoke the callbacks synchronously as each event
// arrives off the wire. After the stream closes they return the
// aggregated Response — Content is the full joined text, plus whatever
// usage / model / stop-reason / Blocks metadata the protocol surfaces.
//
// Callers detect streaming support via a type assertion:
//
//	if sp, ok := p.(provider.StreamingProvider); ok {
//	    return sp.SendStream(ctx, req, provider.StreamCallbacks{OnText: f})
//	}
//	return p.Send(ctx, req)  // fall back to batch
type StreamingProvider interface {
	Provider
	SendStream(ctx context.Context, req Request, cb StreamCallbacks) (Response, error)
}
