package agent

import (
	"context"
	"fmt"
	"strings"
)

// Sender is the minimal slice of provider.Provider the Agent depends on:
// it accepts a model, system prompt, messages, and optional max-tokens, and
// returns the assistant's text reply.
//
// Declaring this interface here (rather than depending on provider.Provider
// directly) keeps the agent package free of an import on provider, which in
// turn keeps the dependency graph one-directional: provider → agent, never
// the other way. The chat subcommand and any future caller is responsible
// for adapting a provider.Provider into a Sender (trivial — provider's
// Request/Response already match).
type Sender interface {
	SendMessages(ctx context.Context, model, system string, messages []Message, maxTokens int) (Reply, error)
}

// StreamingSender extends Sender with the ability to deliver the assistant
// reply chunk-by-chunk via a callback while the upstream stream is open.
//
// The agent type-asserts its Sender to StreamingSender at the start of
// TurnStream; if the underlying provider doesn't implement it, TurnStream
// falls back to the buffered Sender.SendMessages and invokes onChunk once
// with the full content for callers that don't want to branch on capability.
type StreamingSender interface {
	Sender
	StreamMessages(
		ctx context.Context,
		model, system string,
		messages []Message,
		maxTokens int,
		onChunk func(textDelta string),
	) (Reply, error)
}

// ToolSender extends Sender with a tool-aware variant that carries tool
// definitions alongside the messages. Implementations return the full
// content-block list (including tool_use blocks) in Reply.Blocks.
type ToolSender interface {
	Sender
	SendMessagesWithTools(
		ctx context.Context,
		model, system string,
		messages []Message,
		maxTokens int,
		tools []ToolDefinition,
	) (Reply, error)
}

// ToolInputDeltaFunc receives raw JSON fragments of a tool_use block's
// arguments as they stream in. Fragments concatenate to form the final
// JSON object. May be nil; implementations should treat nil as "don't
// surface tool-input deltas" and skip the callback.
type ToolInputDeltaFunc func(toolID, toolName, partialJSON string)

// ToolStreamingSender extends ToolSender and StreamingSender with a streaming
// tool-aware variant. Implementations stream text deltas via onChunk,
// stream tool-argument JSON fragments via onToolDelta (optional, may be
// nil), and accumulate tool_use blocks. The final Reply carries Blocks for
// dispatch.
type ToolStreamingSender interface {
	ToolSender
	StreamMessagesWithTools(
		ctx context.Context,
		model, system string,
		messages []Message,
		maxTokens int,
		tools []ToolDefinition,
		onChunk func(textDelta string),
		onToolDelta ToolInputDeltaFunc,
	) (Reply, error)
}

// Reply is the agent-level view of a provider response. It deliberately
// mirrors provider.Response field-for-field (same names, same types) but
// lives in this package so users of the agent API don't have to import
// provider.
//
// Blocks is populated when the provider returns content blocks — in
// particular when stop_reason=="tool_use", Blocks will contain the
// tool_use blocks that the agentic loop should dispatch.
type Reply struct {
	Content      string
	Blocks       []ContentBlock
	Model        string
	StopReason   string
	InputTokens  int
	OutputTokens int
}

// Agent owns one conversation: the system prompt, the history of turns, the
// model name, and the LLM transport (Sender).
type Agent struct {
	System    string
	Model     string
	MaxTokens int
	History   *History
	Sender    Sender

	// Gate, when non-nil, vets every tool call before execution. A nil
	// Gate means no gating — all tool calls run (the pre-M6.5 behaviour).
	Gate PermissionGate

	// MaxTurns caps the number of provider round-trips in a single Run/
	// RunStream. <= 0 uses defaultMaxTurns. Hitting the cap ends the run
	// with a friendly budget reply (StopReason "max_turns"), not an error.
	MaxTurns int

	// MaxCostUSD caps cumulative session spend. 0 = unlimited. When the
	// running estimate reaches the cap the loop stops before the next
	// provider call with StopReason "max_cost".
	MaxCostUSD float64

	// CompactThreshold triggers history compaction: when the most recent
	// context sent (lastInputTokens) exceeds this, the next Run/RunStream
	// summarizes the older turns before continuing. 0 disables compaction.
	CompactThreshold int

	// Cumulative token counts for this session (all turns combined).
	sessionInputTokens  int
	sessionOutputTokens int
	// lastInputTokens is the size of the most recently sent context, used as
	// the compaction trigger.
	lastInputTokens int
}

// StopReason sentinels set on the Reply when a loop budget is exhausted.
// They are NOT provider stop reasons — the agent synthesises them so callers
// can distinguish "the model finished" from "we cut it off".
const (
	StopReasonMaxTurns = "max_turns"
	StopReasonMaxCost  = "max_cost"
)

// New constructs an Agent with a fresh History.
//
// Required: sender (otherwise Turn returns an error), model (otherwise the
// provider rejects the request). System and MaxTokens are optional.
func New(sender Sender, model string) *Agent {
	return &Agent{
		Model:   model,
		History: NewHistory(),
		Sender:  sender,
	}
}

// Turn appends the user's input to history, asks the Sender for a reply,
// appends the reply to history, and returns it. Errors leave History
// unchanged from before the call.
func (a *Agent) Turn(ctx context.Context, userInput string) (Reply, error) {
	if a.Sender == nil {
		return Reply{}, fmt.Errorf("agent: no Sender configured")
	}
	if a.Model == "" {
		return Reply{}, fmt.Errorf("agent: Model is required")
	}
	if userInput == "" {
		return Reply{}, fmt.Errorf("agent: userInput must be non-empty")
	}

	// Append user message first so the snapshot the Sender sees includes it.
	a.History.Append(NewUserMessage(userInput))

	reply, err := a.Sender.SendMessages(ctx, a.Model, a.System, a.History.Snapshot(), a.MaxTokens)
	if err != nil {
		// Pop the user message we just appended so retrying with the same
		// History doesn't duplicate it. Cheaper than transactional locking
		// since History is only mutated from this goroutine in M1.2.
		a.History.popLast()
		return Reply{}, fmt.Errorf("agent: send: %w", err)
	}

	a.History.Append(NewAssistantMessage(reply.Content))
	a.sessionInputTokens += reply.InputTokens
	a.sessionOutputTokens += reply.OutputTokens
	a.lastInputTokens = reply.InputTokens
	return reply, nil
}

// TurnStream is the streaming counterpart of Turn. It appends the user input
// to history, calls the Sender (streaming if supported, otherwise falling
// back to SendMessages), invokes onChunk for each text delta, appends the
// final assistant reply to history, and returns it.
//
// onChunk may be nil, in which case the stream is still consumed end-to-end
// but no per-delta callback fires — useful for tests and for callers that
// only want the aggregated Reply.
//
// On error, the user message is popped from History (same contract as Turn),
// so a retry with the same History doesn't duplicate the user turn.
func (a *Agent) TurnStream(
	ctx context.Context,
	userInput string,
	onChunk func(textDelta string),
) (Reply, error) {
	if a.Sender == nil {
		return Reply{}, fmt.Errorf("agent: no Sender configured")
	}
	if a.Model == "" {
		return Reply{}, fmt.Errorf("agent: Model is required")
	}
	if userInput == "" {
		return Reply{}, fmt.Errorf("agent: userInput must be non-empty")
	}

	a.History.Append(NewUserMessage(userInput))

	var (
		reply Reply
		err   error
	)
	if ss, ok := a.Sender.(StreamingSender); ok {
		reply, err = ss.StreamMessages(ctx, a.Model, a.System, a.History.Snapshot(), a.MaxTokens, onChunk)
	} else {
		// Fallback: buffer the call and surface a single "chunk" with the
		// full content. Keeps callers from having to branch on capability
		// at the cost of losing real-time visibility on this backend.
		reply, err = a.Sender.SendMessages(ctx, a.Model, a.System, a.History.Snapshot(), a.MaxTokens)
		if err == nil && onChunk != nil && reply.Content != "" {
			onChunk(reply.Content)
		}
	}
	if err != nil {
		a.History.popLast()
		return Reply{}, fmt.Errorf("agent: stream: %w", err)
	}

	a.History.Append(NewAssistantMessage(reply.Content))
	a.sessionInputTokens += reply.InputTokens
	a.sessionOutputTokens += reply.OutputTokens
	a.lastInputTokens = reply.InputTokens
	return reply, nil
}

// defaultMaxTurns is the fallback per-Run loop cap when Agent.MaxTurns is
// unset (<= 0). A "turn" here is one provider round-trip inside the agentic
// loop; the cap stops a misbehaving model from looping on tools forever.
const defaultMaxTurns = 20

// Run is the agentic loop: it appends the user message to history then
// repeatedly calls the provider until the model reaches end_turn (no more
// tool calls) or the iteration cap is hit. Run is the buffered, no-event
// counterpart of RunStream — both drive the same runLoop, Run with a nil
// handler so no AgentEvents are emitted.
//
// If tools is nil or executor is nil, Run is equivalent to Turn (single-turn,
// no tool dispatch).
func (a *Agent) Run(ctx context.Context, userInput string, tools []ToolDefinition, executor ToolExecutor) (Reply, error) {
	if a.Sender == nil {
		return Reply{}, fmt.Errorf("agent: no Sender configured")
	}
	if a.Model == "" {
		return Reply{}, fmt.Errorf("agent: Model is required")
	}
	if userInput == "" {
		return Reply{}, fmt.Errorf("agent: userInput must be non-empty")
	}

	// No tools (or a Sender that can't do tools) → plain Turn.
	if len(tools) == 0 || executor == nil {
		return a.Turn(ctx, userInput)
	}
	ts, ok := a.Sender.(ToolSender)
	if !ok {
		return a.Turn(ctx, userInput)
	}

	// Buffered send + nil handler: runLoop runs the same dispatch/history
	// machinery as the streaming path but emits no events.
	return a.runLoop(ctx, userInput, tools, executor, nil,
		func(ctx context.Context, msgs []Message) (Reply, error) {
			return ts.SendMessagesWithTools(ctx, a.Model, a.System, msgs, a.MaxTokens, tools)
		})
}

// RunStream is the streaming agentic loop. Behaves like Run but emits
// structured AgentEvents to handler as work progresses — text deltas, tool
// start/done/error, and a final EventTurnDone carrying the aggregated Reply.
//
// If tools is nil or executor is nil, RunStream falls back to TurnStream and
// adapts text deltas into EventTextDelta events. handler may be nil, in
// which case events are discarded but the run completes normally.
func (a *Agent) RunStream(
	ctx context.Context,
	userInput string,
	tools []ToolDefinition,
	executor ToolExecutor,
	handler EventHandler,
) (Reply, error) {
	if a.Sender == nil {
		return Reply{}, fmt.Errorf("agent: no Sender configured")
	}
	if a.Model == "" {
		return Reply{}, fmt.Errorf("agent: Model is required")
	}
	if userInput == "" {
		return Reply{}, fmt.Errorf("agent: userInput must be non-empty")
	}

	// onChunk adapts text deltas from provider streams into EventTextDelta
	// events. Nil-safe; empty deltas are silently dropped.
	onChunk := func(delta string) {
		if handler == nil || delta == "" {
			return
		}
		handler(AgentEvent{Kind: EventTextDelta, Text: delta})
	}

	// No tools → plain TurnStream with the event-adapting onChunk. The
	// terminal EventTurnDone is fired here so the caller's contract is
	// identical regardless of whether tools were used.
	if len(tools) == 0 || executor == nil {
		reply, err := a.TurnStream(ctx, userInput, onChunk)
		if err == nil && handler != nil {
			r := reply
			handler(AgentEvent{Kind: EventTurnDone, Reply: &r})
		}
		return reply, err
	}

	// Tool-argument JSON fragments → EventToolInputDelta. Nil-safe.
	onToolDelta := func(toolID, toolName, partialJSON string) {
		if handler == nil || partialJSON == "" {
			return
		}
		handler(AgentEvent{
			Kind:       EventToolInputDelta,
			ToolID:     toolID,
			ToolName:   toolName,
			InputDelta: partialJSON,
		})
	}

	// Try ToolStreamingSender first, then fall back to ToolSender (buffered).
	if tss, ok := a.Sender.(ToolStreamingSender); ok {
		return a.runLoop(ctx, userInput, tools, executor, handler,
			func(ctx context.Context, msgs []Message) (Reply, error) {
				return tss.StreamMessagesWithTools(ctx, a.Model, a.System, msgs, a.MaxTokens, tools, onChunk, onToolDelta)
			})
	}
	if ts, ok := a.Sender.(ToolSender); ok {
		return a.runLoop(ctx, userInput, tools, executor, handler,
			func(ctx context.Context, msgs []Message) (Reply, error) {
				reply, err := ts.SendMessagesWithTools(ctx, a.Model, a.System, msgs, a.MaxTokens, tools)
				if err == nil && reply.Content != "" {
					onChunk(reply.Content)
				}
				return reply, err
			})
	}

	// Neither tool-aware interface available → plain TurnStream with the
	// event-adapting onChunk. EventTurnDone fires on success.
	reply, err := a.TurnStream(ctx, userInput, onChunk)
	if err == nil && handler != nil {
		r := reply
		handler(AgentEvent{Kind: EventTurnDone, Reply: &r})
	}
	return reply, err
}

// runLoop is the single agentic loop shared by Run and RunStream. The send
// function encapsulates the provider call (streaming or buffered) and is
// responsible for surfacing text deltas itself; this loop owns tool dispatch,
// history bookkeeping, and tool-level event emission.
//
// handler may be nil (the Run path): every emit* helper and the dispatch
// progress path short-circuit on nil, so no events fire.
func (a *Agent) runLoop(
	ctx context.Context,
	userInput string,
	tools []ToolDefinition,
	executor ToolExecutor,
	handler EventHandler,
	send func(ctx context.Context, msgs []Message) (Reply, error),
) (Reply, error) {
	// Compact older history before starting a new turn, if the last context
	// crossed the threshold. Done here (a safe between-turns boundary, history
	// ends on a complete assistant message) rather than mid-loop, where a
	// tool_use/tool_result pair could be split. A summarization failure is
	// non-fatal — we log nothing and proceed with the full history.
	_ = a.maybeCompact(ctx)

	a.History.Append(NewUserMessage(userInput))

	limit := a.turnLimit()
	for i := 0; i < limit; i++ {
		// Cost gate: checked before each provider call. Cost is only known
		// after a response, so the worst case is one call that tips over the
		// budget; we stop before the next.
		if a.MaxCostUSD > 0 && a.SessionCostUSD() >= a.MaxCostUSD {
			return a.budgetStop(handler, StopReasonMaxCost, fmt.Sprintf(
				"[octo] Stopped: session cost budget ($%.4f) reached. The task may be "+
					"incomplete — raise --max-cost or start a new session to continue.", a.MaxCostUSD))
		}

		reply, err := send(ctx, a.History.Snapshot())
		if err != nil {
			if i == 0 {
				a.History.popLast()
			}
			return Reply{}, fmt.Errorf("agent: loop[%d]: %w", i, err)
		}
		a.sessionInputTokens += reply.InputTokens
		a.sessionOutputTokens += reply.OutputTokens
		a.lastInputTokens = reply.InputTokens

		if reply.StopReason == "tool_use" {
			a.History.Append(NewToolUseMessage(reply.Blocks))

			// Emit EventToolStarted before dispatch so observers see the
			// "thinking → tool call" boundary even if the tool blocks.
			emitToolStartedEvents(handler, reply.Blocks)

			// handler is threaded through to dispatchTools so streaming
			// tools (StreamingToolExecutor) can fire EventToolProgress as
			// output arrives mid-execution.
			resultBlocks, err := dispatchTools(ctx, executor, reply.Blocks, handler, a.Gate)
			if err != nil {
				return Reply{}, fmt.Errorf("agent: dispatch tools[%d]: %w", i, err)
			}

			// Emit EventToolDone / EventToolError per result, pairing
			// each result with the originating tool_use block so ToolName
			// can be carried through (tool_result blocks don't carry it
			// themselves).
			emitToolResultEvents(handler, reply.Blocks, resultBlocks)

			a.History.Append(NewToolResultMessage(resultBlocks))
			continue
		}

		content := reply.Content
		if content == "" {
			content = textFromBlocks(reply.Blocks)
		}
		a.History.Append(NewAssistantMessage(content))
		reply.Content = content
		if handler != nil {
			r := reply
			handler(AgentEvent{Kind: EventTurnDone, Reply: &r})
		}
		return reply, nil
	}

	// Loop cap reached while the model still wanted to keep going. End the
	// run gracefully rather than erroring — the history holds the partial
	// progress and the caller gets a clear, non-fatal explanation.
	return a.budgetStop(handler, StopReasonMaxTurns, fmt.Sprintf(
		"[octo] Stopped: reached the max-turns limit (%d). The task may be incomplete — "+
			"raise --max-turns or send another message to continue.", limit))
}

// turnLimit resolves the per-Run loop cap, applying the default when unset.
func (a *Agent) turnLimit() int {
	if a.MaxTurns > 0 {
		return a.MaxTurns
	}
	return defaultMaxTurns
}

// budgetStop ends a run that hit a loop budget (turns or cost). It appends a
// synthetic assistant message so history stays well-formed, surfaces the
// message as a text delta + turn_done event (so streaming callers render it
// like normal reply text), and returns a Reply carrying the budget StopReason
// — never an error, so the partial progress isn't discarded.
func (a *Agent) budgetStop(handler EventHandler, reason, msg string) (Reply, error) {
	a.History.Append(NewAssistantMessage(msg))
	reply := Reply{Content: msg, StopReason: reason}
	if handler != nil {
		handler(AgentEvent{Kind: EventTextDelta, Text: msg})
		r := reply
		handler(AgentEvent{Kind: EventTurnDone, Reply: &r})
	}
	return reply, nil
}

// emitToolStartedEvents fires one EventToolStarted per tool_use block.
// handler may be nil — the loop short-circuits cleanly.
func emitToolStartedEvents(handler EventHandler, useBlocks []ContentBlock) {
	if handler == nil {
		return
	}
	for _, b := range useBlocks {
		if b.Type != "tool_use" {
			continue
		}
		handler(AgentEvent{
			Kind:     EventToolStarted,
			ToolID:   b.ID,
			ToolName: b.Name,
			Input:    b.Input,
		})
	}
}

// emitToolResultEvents pairs each tool_result with the originating tool_use
// (matched on ID) so ToolName flows through to the EventDone / EventError
// payload. tool_result blocks don't carry the tool name themselves — this
// pairing is required to keep events fully self-describing for UI consumers.
func emitToolResultEvents(handler EventHandler, useBlocks, resultBlocks []ContentBlock) {
	if handler == nil {
		return
	}
	// Build an id→name index once.
	nameByID := make(map[string]string, len(useBlocks))
	for _, b := range useBlocks {
		if b.Type == "tool_use" {
			nameByID[b.ID] = b.Name
		}
	}
	for _, r := range resultBlocks {
		if r.Type != "tool_result" {
			continue
		}
		ev := AgentEvent{
			ToolID:   r.ToolUseID,
			ToolName: nameByID[r.ToolUseID],
			Output:   truncateOutput(r.Result),
		}
		if r.IsError {
			ev.Kind = EventToolError
			ev.Err = r.Result // full untruncated error message in Err
		} else {
			ev.Kind = EventToolDone
		}
		handler(ev)
	}
}

// dispatchTools calls executor.Execute (or ExecuteStream when both the
// executor implements StreamingToolExecutor AND handler is non-nil) for
// every tool_use block in blocks, returning the corresponding tool_result
// blocks. Errors from Execute are returned as tool_result blocks with
// IsError=true so the model can recover.
//
// handler may be nil — in that case progress events are dropped and even a
// streaming executor effectively runs in non-streaming mode (the aggregated
// output is still what lands in the tool_result block).
//
// gate may be nil — no permission gating. When non-nil, each tool call is
// vetted before execution; a denied call is turned into an IsError
// tool_result carrying the gate's reason and the executor is never invoked.
func dispatchTools(ctx context.Context, executor ToolExecutor, blocks []ContentBlock, handler EventHandler, gate PermissionGate) ([]ContentBlock, error) {
	streaming, hasStreaming := executor.(StreamingToolExecutor)

	var results []ContentBlock
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}

		// Permission gate: a denial short-circuits execution and feeds the
		// reason back to the LLM as an error result.
		if gate != nil {
			if allowed, reason := gate.Check(ctx, b.Name, b.Input); !allowed {
				if reason == "" {
					reason = "permission denied"
				}
				results = append(results, NewToolResultBlock(b.ID, reason, true))
				continue
			}
		}

		var (
			output  string
			execErr error
		)
		if hasStreaming && handler != nil {
			// Capture id/name in this iteration's closure — the next loop
			// iteration would clobber `b` otherwise.
			toolID, toolName := b.ID, b.Name
			progress := func(chunk string) {
				if chunk == "" {
					return
				}
				handler(AgentEvent{
					Kind:     EventToolProgress,
					ToolID:   toolID,
					ToolName: toolName,
					Chunk:    chunk,
				})
			}
			output, execErr = streaming.ExecuteStream(ctx, b.Name, b.Input, progress)
		} else {
			output, execErr = executor.Execute(ctx, b.Name, b.Input)
		}

		if execErr != nil {
			results = append(results, NewToolResultBlock(b.ID, execErr.Error(), true))
		} else {
			results = append(results, NewToolResultBlock(b.ID, output, false))
		}
	}
	return results, nil
}

// textFromBlocks joins text from all "text" content blocks.
func textFromBlocks(blocks []ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// SessionTokens returns the cumulative input and output token counts for all
// turns made so far in this Agent's lifetime.
func (a *Agent) SessionTokens() (inputTokens, outputTokens int) {
	return a.sessionInputTokens, a.sessionOutputTokens
}

// SessionCostUSD returns a rough USD estimate for the tokens used so far.
// Pricing is based on publicly listed rates as of May 2026 and is best-effort
// — it uses a prefix match on the model name and falls back to a conservative
// mid-tier estimate for unknown models.
func (a *Agent) SessionCostUSD() float64 {
	in, out := float64(a.sessionInputTokens), float64(a.sessionOutputTokens)
	inPrice, outPrice := modelPricePerMillion(a.Model)
	return (in/1_000_000)*inPrice + (out/1_000_000)*outPrice
}

// modelPricePerMillion returns (inputPricePerMillion, outputPricePerMillion)
// in USD for the given model name. Prices are approximate and may be stale.
func modelPricePerMillion(model string) (float64, float64) {
	switch {
	// Anthropic Claude 4.x Haiku — cheapest tier
	case hasPrefix(model, "claude-haiku"):
		return 0.80, 4.00
	// Anthropic Claude 4.x Sonnet
	case hasPrefix(model, "claude-sonnet"):
		return 3.00, 15.00
	// Anthropic Claude 4.x Opus
	case hasPrefix(model, "claude-opus"):
		return 15.00, 75.00
	// OpenAI GPT-4o mini
	case hasPrefix(model, "gpt-4o-mini"):
		return 0.15, 0.60
	// OpenAI GPT-4o
	case hasPrefix(model, "gpt-4o"):
		return 2.50, 10.00
	// Unknown — conservative mid-tier estimate
	default:
		return 3.00, 15.00
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// popLast is an internal helper used by Turn to undo the user-message append
// when the Sender call fails. Exported users should not need this.
func (h *History) popLast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.messages); n > 0 {
		h.messages = h.messages[:n-1]
	}
}
