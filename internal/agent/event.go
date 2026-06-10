package agent

// EventKind tags an AgentEvent by what happened.
type EventKind string

const (
	// EventTextDelta carries one piece of the assistant's text reply, as it
	// arrives off the provider stream. Multiple deltas concatenate to form
	// the full reply text.
	EventTextDelta EventKind = "text_delta"

	// EventThinkingDelta carries one fragment of a reasoning model's thinking
	// trace, streamed before the visible reply. Text holds the fragment;
	// fragments concatenate to form the full trace. Emitted only when the Sender
	// surfaces reasoning (e.g. reasoning display is enabled); observers render it
	// dimmed and distinct from the answer. It is NOT part of Reply.Content.
	EventThinkingDelta EventKind = "thinking_delta"

	// EventToolInputDelta fires zero or more times while the LLM is
	// streaming a tool_use block's input arguments (e.g. write_file's
	// content field). ToolID / ToolName identify the call; InputDelta is
	// the raw JSON fragment as it arrived on the wire — fragments
	// concatenate to form the final JSON object. EventToolStarted (with
	// the fully-parsed Input map) still fires after the arguments are
	// complete and parsed.
	//
	// These events are useful for live-rendering large tool arguments
	// (e.g. showing a file's content as it's being written) in a Web UI.
	// Most CLI consumers can ignore them.
	EventToolInputDelta EventKind = "tool_input_delta"

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

	// EventSteerInjected fires when the agent loop drains the inbox and
	// injects mid-turn user messages into history. Messages carries the
	// drained texts so observers (e.g. the TUI) can render them in the
	// transcript at the correct chronological position — before the next
	// assistant reply, not after the turn ends.
	EventSteerInjected EventKind = "steer_injected"

	// EventCompactStarted fires when history compaction begins, just before
	// the summarization side-call. Compact carries the pre-compaction context
	// estimate and how much is being folded so observers can show a "compacting
	// conversation history" indicator. Compaction is silent to the model; these
	// events exist only to keep the user informed.
	EventCompactStarted EventKind = "compact_started"

	// EventCompactProgress fires repeatedly while the summary streams back from
	// the model. Chunk carries the newest text fragment of the summary;
	// Compact.SummaryTokens is the running estimate of summary length so far.
	// Observers can show a live "generated ~N tokens" indicator. Fires only
	// when the underlying Sender streams; otherwise compaction jumps straight
	// from started to done.
	EventCompactProgress EventKind = "compact_progress"

	// EventCompactDone fires once compaction finishes (or fails). Compact
	// carries the before/after context estimates; when they are equal the
	// compaction was a no-op (summarization failed or returned nothing) and the
	// full history was kept. Observers should clear any compaction indicator.
	EventCompactDone EventKind = "compact_done"
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
//   - EventTextDelta:      Text
//   - EventThinkingDelta:  Text
//   - EventToolInputDelta: ToolID, ToolName, InputDelta
//   - EventToolStarted:    ToolID, ToolName, Input
//   - EventToolProgress:   ToolID, ToolName, Chunk
//   - EventToolDone:       ToolID, ToolName, Output
//   - EventToolError:      ToolID, ToolName, Output (may be empty), Err
//   - EventTurnDone:       Reply
//   - EventSteerInjected:  Messages
//   - EventCompactStarted:  Compact (BeforeTokens, FoldedMsgs, KeptTurns, MaxTokens)
//   - EventCompactProgress: Chunk, Compact (SummaryTokens, MaxTokens)
//   - EventCompactDone:     Compact (BeforeTokens, AfterTokens, FoldedMsgs)
//
// JSON tags are included so HTTP/SSE transports (M8 web server) can
// marshal events directly without an intermediate type.
type AgentEvent struct {
	Kind       EventKind      `json:"kind"`
	Text       string         `json:"text,omitempty"`
	ToolID     string         `json:"tool_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	InputDelta string         `json:"input_delta,omitempty"`
	Chunk      string         `json:"chunk,omitempty"`
	Output     string         `json:"output,omitempty"`
	Err        string         `json:"err,omitempty"`
	Reply      *Reply         `json:"reply,omitempty"`
	Messages   []string       `json:"messages,omitempty"`
	Compact    *CompactStats  `json:"compact,omitempty"`

	// Steer carries the full inbox items behind an EventSteerInjected —
	// including attachment blocks — for handlers that render more than the
	// plain texts in Messages.
	Steer []InboxItem `json:"-"`
}

// CompactStats carries the numbers behind the compaction events. BeforeTokens
// and AfterTokens prefer the provider's real input token count when available
// (lastInputTokens) and fall back to a heuristic estimate otherwise. They exist
// for a human-readable progress indicator, nothing more.
type CompactStats struct {
	// BeforeTokens is the context size before compaction (real when available).
	BeforeTokens int `json:"before_tokens,omitempty"`
	// AfterTokens is the context size after compaction (real when available, done only).
	AfterTokens int `json:"after_tokens,omitempty"`
	// FoldedMsgs is how many leading messages were folded into the summary.
	FoldedMsgs int `json:"folded_msgs,omitempty"`
	// KeptTurns is how many recent user turns were kept verbatim.
	KeptTurns int `json:"kept_turns,omitempty"`
	// SummaryTokens is the running estimate of the summary generated so far
	// (progress only).
	SummaryTokens int `json:"summary_tokens,omitempty"`
	// MaxTokens is the summary's output-token cap, for a "N / max" readout.
	MaxTokens int `json:"max_tokens,omitempty"`
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
