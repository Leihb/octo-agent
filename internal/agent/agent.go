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

// ToolStreamingSender extends ToolSender and StreamingSender with a streaming
// tool-aware variant. Implementations stream text deltas via onChunk and
// accumulate tool_use blocks; the final Reply carries Blocks for dispatch.
type ToolStreamingSender interface {
	ToolSender
	StreamMessagesWithTools(
		ctx context.Context,
		model, system string,
		messages []Message,
		maxTokens int,
		tools []ToolDefinition,
		onChunk func(textDelta string),
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

	// Cumulative token counts for this session (all turns combined).
	sessionInputTokens  int
	sessionOutputTokens int
}

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
	return reply, nil
}

// maxToolIterations is the safety cap on the agentic loop. If the model
// requests more tool calls than this in a single Run, we bail out with an
// error rather than looping forever.
const maxToolIterations = 20

// Run is the agentic loop. It appends the user message to history then
// repeatedly calls the provider until the model reaches end_turn (no more
// tool calls) or the iteration cap is hit.
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

	// No tools → plain Turn.
	if len(tools) == 0 || executor == nil {
		return a.Turn(ctx, userInput)
	}

	ts, ok := a.Sender.(ToolSender)
	if !ok {
		// Sender doesn't support tools; fall back to Turn.
		return a.Turn(ctx, userInput)
	}

	a.History.Append(NewUserMessage(userInput))

	for i := 0; i < maxToolIterations; i++ {
		reply, err := ts.SendMessagesWithTools(ctx, a.Model, a.System, a.History.Snapshot(), a.MaxTokens, tools)
		if err != nil {
			if i == 0 {
				a.History.popLast()
			}
			return Reply{}, fmt.Errorf("agent: run[%d]: %w", i, err)
		}
		a.sessionInputTokens += reply.InputTokens
		a.sessionOutputTokens += reply.OutputTokens

		if reply.StopReason == "tool_use" {
			// Append assistant message with tool_use blocks, then dispatch.
			a.History.Append(NewToolUseMessage(reply.Blocks))

			// Run.runner doesn't carry a handler — progress events would
			// have nowhere to go. Pass nil; dispatchTools degrades to the
			// non-streaming path automatically.
			resultBlocks, err := dispatchTools(ctx, executor, reply.Blocks, nil)
			if err != nil {
				return Reply{}, fmt.Errorf("agent: dispatch tools[%d]: %w", i, err)
			}
			a.History.Append(NewToolResultMessage(resultBlocks))
			continue
		}

		// end_turn (or other stop reason): done.
		// Use text content from reply; if Content is empty but Blocks has text, reconstruct.
		content := reply.Content
		if content == "" {
			content = textFromBlocks(reply.Blocks)
		}
		a.History.Append(NewAssistantMessage(content))
		reply.Content = content
		return reply, nil
	}

	return Reply{}, fmt.Errorf("agent: exceeded max tool iterations (%d)", maxToolIterations)
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

	// Try ToolStreamingSender first, then fall back to ToolSender (buffered).
	if tss, ok := a.Sender.(ToolStreamingSender); ok {
		return a.runStreamLoop(ctx, userInput, tools, executor, handler,
			func(ctx context.Context, msgs []Message) (Reply, error) {
				return tss.StreamMessagesWithTools(ctx, a.Model, a.System, msgs, a.MaxTokens, tools, onChunk)
			})
	}
	if ts, ok := a.Sender.(ToolSender); ok {
		return a.runStreamLoop(ctx, userInput, tools, executor, handler,
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

// runStreamLoop is the shared inner loop for RunStream. The send function
// encapsulates the provider call (streaming or buffered) and is responsible
// for surfacing text deltas itself; this loop is only responsible for tool
// dispatch and tool-level events.
func (a *Agent) runStreamLoop(
	ctx context.Context,
	userInput string,
	tools []ToolDefinition,
	executor ToolExecutor,
	handler EventHandler,
	send func(ctx context.Context, msgs []Message) (Reply, error),
) (Reply, error) {
	a.History.Append(NewUserMessage(userInput))

	for i := 0; i < maxToolIterations; i++ {
		reply, err := send(ctx, a.History.Snapshot())
		if err != nil {
			if i == 0 {
				a.History.popLast()
			}
			return Reply{}, fmt.Errorf("agent: run-stream[%d]: %w", i, err)
		}
		a.sessionInputTokens += reply.InputTokens
		a.sessionOutputTokens += reply.OutputTokens

		if reply.StopReason == "tool_use" {
			a.History.Append(NewToolUseMessage(reply.Blocks))

			// Emit EventToolStarted before dispatch so observers see the
			// "thinking → tool call" boundary even if the tool blocks.
			emitToolStartedEvents(handler, reply.Blocks)

			// handler is threaded through to dispatchTools so streaming
			// tools (StreamingToolExecutor) can fire EventToolProgress as
			// output arrives mid-execution.
			resultBlocks, err := dispatchTools(ctx, executor, reply.Blocks, handler)
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

	return Reply{}, fmt.Errorf("agent: exceeded max tool iterations (%d)", maxToolIterations)
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
func dispatchTools(ctx context.Context, executor ToolExecutor, blocks []ContentBlock, handler EventHandler) ([]ContentBlock, error) {
	streaming, hasStreaming := executor.(StreamingToolExecutor)

	var results []ContentBlock
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
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
