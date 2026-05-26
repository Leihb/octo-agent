package agent

// EventKind tags an AgentEvent by what happened.
type EventKind string

const (
	// EventTextDelta carries one piece of the assistant's text reply, as it
	// arrives off the provider stream. Multiple deltas concatenate to form
	// the full reply text.
	EventTextDelta EventKind = "text_delta"

	// EventToolStarted fires immediately before a tool is dispatched.
	// ToolID / ToolName / Input identify the call.
	EventToolStarted EventKind = "tool_started"

	// EventToolProgress fires zero or more times between EventToolStarted and
	// EventToolDone, surfacing incremental tool output (e.g. a long shell
	// command's stdout line-by-line). Only tools that implement
	// StreamingToolExecutor emit these; tools that don't are silent until
	// EventToolDone. Chunk carries the new fragment, NOT the running total —
	// it's not truncated (the consumer is responsible for any rate limiting
	// or batching).
	EventToolProgress EventKind = "tool_progress"

	// EventToolDone fires after a successful tool execution.
	// Output carries the tool's combined stdout/stderr text (truncated to
	// EventToolOutputCap if longer).
	EventToolDone EventKind = "tool_done"

	// EventToolError fires when the tool executor reports an error result
	// (IsError=true on the underlying ToolResultBlock). Err carries the
	// failure message; Output may still contain partial stdout from the
	// failing process.
	EventToolError EventKind = "tool_error"

	// EventTurnDone fires once at the end of a successful Run/RunStream,
	// after the assistant's final reply is committed to history. Reply
	// carries the aggregated final Reply.
	EventTurnDone EventKind = "turn_done"
)

// EventToolOutputCap is the maximum length of the Output field emitted on
// EventToolDone / EventToolError. The agent loop never truncates the actual
// tool result going back into the conversation — this cap only applies to
// what's surfaced to event observers (Web UI cards, IM previews), where a
// 100KB shell dump would be useless noise.
const EventToolOutputCap = 512

// AgentEvent is the union shape carried over the EventHandler callback.
//
// All fields are populated only for the EventKinds that need them; the rest
// stay at zero values. The contract for each kind:
//
//   - EventTextDelta:    Text
//   - EventToolStarted:  ToolID, ToolName, Input
//   - EventToolProgress: ToolID, ToolName, Chunk
//   - EventToolDone:     ToolID, ToolName, Output
//   - EventToolError:    ToolID, ToolName, Output (may be empty), Err
//   - EventTurnDone:     Reply
//
// JSON tags are included so HTTP/SSE transports (M8 web server) can
// marshal events directly without an intermediate type.
type AgentEvent struct {
	Kind     EventKind      `json:"kind"`
	Text     string         `json:"text,omitempty"`
	ToolID   string         `json:"tool_id,omitempty"`
	ToolName string         `json:"tool_name,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
	Chunk    string         `json:"chunk,omitempty"`
	Output   string         `json:"output,omitempty"`
	Err      string         `json:"err,omitempty"`
	Reply    *Reply         `json:"reply,omitempty"`
}

// EventHandler is the callback type passed into Agent.RunStream. The handler
// is invoked synchronously from the agent loop — if it blocks, the loop
// blocks. Implementations that need async fan-out (e.g. SSE to multiple
// clients) should buffer into a channel and return immediately.
type EventHandler func(AgentEvent)

// truncateOutput is the helper used by the agent loop before placing tool
// output into an AgentEvent. Keeping it here rather than in agent.go puts
// the size policy next to the cap constant.
func truncateOutput(s string) string {
	if len(s) <= EventToolOutputCap {
		return s
	}
	// Add a clear marker so consumers know they only got a slice.
	return s[:EventToolOutputCap] + "…[truncated]"
}
