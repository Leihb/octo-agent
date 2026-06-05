package tools

import "context"

// SubAgentEvent is a runtime progress event from a sub-agent, forwarded to a
// SubAgentManager's onEvent hook for live display. It is distinct from
// SubAgentNotification, which is the one-shot completion record: events stream
// while the sub-agent works, the notification fires once when it finishes.
//
// Only tool-level activity is carried — not per-token text — so a live panel
// can show each sub-agent's tool-call chain without the event volume of N
// concurrent sub-agents streaming their prose.
type SubAgentEvent struct {
	AgentID     string // manager handle, e.g. "agent_1"
	Description string // human-readable label from sub_agent
	// Kind is one of:
	//   "started"    — the sub-agent began a task (or a Continue round)
	//   "tool"       — it dispatched a tool (ToolName set)
	//   "tool_error" — a tool returned an error (ToolName set)
	Kind     string
	ToolName string
}

type subAgentSinkKey struct{}

// WithSubAgentEventSink returns a context carrying sink, which the execution
// layer (the agentSpawner) pulls out to forward a child's runtime events. The
// SubAgentManager stamps this in before calling Spawn/Continue, so the Spawner
// interface itself stays unchanged and the non-TUI path (no sink) emits
// nothing.
func WithSubAgentEventSink(ctx context.Context, sink func(SubAgentEvent)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, subAgentSinkKey{}, sink)
}

// SubAgentEventSink returns the sink stamped by WithSubAgentEventSink, or nil
// when none is set (headless, tests, or a manager with no onEvent hook).
func SubAgentEventSink(ctx context.Context) func(SubAgentEvent) {
	sink, _ := ctx.Value(subAgentSinkKey{}).(func(SubAgentEvent))
	return sink
}
