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

// ToolResult is the return value from a tool execution. Text is the required
// textual summary (shown in the UI and sent to the model as the primary
// result). Blocks holds optional rich content — images for multimodal models,
// structured data, etc. — that the provider adapter serialises into the
// vendor-specific wire format.
type ToolResult struct {
	Text   string         // required textual summary
	Blocks []ContentBlock // optional rich content (images, etc.)
}

// ToolExecutor dispatches tool calls on behalf of the agentic loop. Each
// implementation maps a tool name to a function; unknown names should return
// an error so the LLM sees a clean error result rather than a panic.
type ToolExecutor interface {
	Execute(ctx context.Context, name string, input map[string]any) (ToolResult, error)
}

// PermissionGate decides whether a tool call may proceed. The agent loop
// consults the gate (if one is set on the Agent) immediately before
// executing each tool_use block. A denied call never reaches the executor;
// instead the loop synthesises a tool_result with IsError=true carrying the
// reason, so the LLM sees the denial and can adapt (suggest an alternative,
// ask the user to whitelist, etc.) rather than the run aborting.
//
// Implementations own the interaction model for "ask" decisions: a CLI gate
// prompts the user synchronously, while a non-interactive (server / IM) gate
// resolves ask → deny. The agent package stays ignorant of how the decision
// is reached — it only sees the final allow/deny.
type PermissionGate interface {
	// Check reports whether the named tool call may run. reason is a
	// human/LLM-readable explanation, surfaced in the tool_result when
	// allowed is false; it may be empty when allowed is true.
	Check(ctx context.Context, name string, input map[string]any) (allowed bool, reason string)
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
// as equivalent to non-streaming Execute. The (ToolResult, error) return is
// the FULL aggregated result (same contract as Execute) — progress chunks are
// for UI/observability only.
type StreamingToolExecutor interface {
	ToolExecutor
	ExecuteStream(
		ctx context.Context,
		name string,
		input map[string]any,
		progress func(chunk string),
	) (ToolResult, error)
}
