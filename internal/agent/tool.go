package agent

import "context"

// ToolDefinition describes a tool the LLM may invoke. The Parameters field
// must be a valid JSON Schema "object" definition; most tools only need
// "type", "properties", and "required".
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

// ToolExecutor dispatches tool calls on behalf of the agentic loop. Each
// implementation maps a tool name to a function; unknown names should return
// an error so the LLM sees a clean error result rather than a panic.
type ToolExecutor interface {
	Execute(ctx context.Context, name string, input map[string]any) (string, error)
}

// StreamingToolExecutor is an optional extension to ToolExecutor: tools that
// produce incremental output (e.g. a long shell command writing stdout line
// by line) can implement ExecuteStream and surface chunks as they happen.
//
// The agent loop type-asserts the executor at dispatch time. If the executor
// supports streaming AND the caller provided an EventHandler, the loop calls
// ExecuteStream and forwards each chunk as an EventToolProgress event.
// Otherwise the loop falls back to Execute.
//
// progress may be nil; implementations should treat a nil progress callback
// as equivalent to non-streaming Execute. The (string, error) return is the
// FULL aggregated output (same contract as Execute) — progress chunks are
// for UI/observability only.
type StreamingToolExecutor interface {
	ToolExecutor
	ExecuteStream(
		ctx context.Context,
		name string,
		input map[string]any,
		progress func(chunk string),
	) (string, error)
}
