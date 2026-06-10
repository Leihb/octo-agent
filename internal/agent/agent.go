package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
		onThinking func(thinkingDelta string),
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

// ThinkingDeltaFunc receives fragments of a reasoning model's thinking trace
// as they stream in, before the visible reply. May be nil; implementations
// treat nil as "don't surface reasoning" and skip the callback.
type ThinkingDeltaFunc func(thinkingDelta string)

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
		onThinking ThinkingDeltaFunc,
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
	// Cache accounting for this call (0 when the backend reports none).
	// CacheReadTokens is input served from cache; CacheWriteTokens is input
	// written into the cache this turn (Anthropic only).
	CacheReadTokens  int
	CacheWriteTokens int
}

// Agent owns one conversation: the system prompt, the history of turns, the
// model name, and the LLM transport (Sender).
type Agent struct {
	System    string
	Model     string
	MaxTokens int
	History   *History
	Sender    Sender

	// CWD is the working directory used to resolve project context (e.g.
	// .octorules) for the planner. Callers should set this to the repo root
	// before invoking PlanTask.
	CWD string

	// Gate, when non-nil, vets every tool call before execution. A nil
	// Gate means no gating — all tool calls run (the pre-M6.5 behaviour).
	Gate PermissionGate

	// MaxTurns caps the number of provider round-trips in a single Run/
	// RunStream. <= 0 uses defaultMaxTurns. Hitting the cap ends the run
	// with a friendly budget reply (StopReason "max_turns"), not an error.
	MaxTurns int

	// MaxTokensEscalate is the per-response cap retried once, from unchanged
	// history, when a round is truncated by the output cap (StopReasonMaxTokens).
	// It only ever raises the cap: escalation fires only when this exceeds the
	// round's current cap. <= 0 disables escalation. See
	// dev-docs/truncation-recovery.md.
	MaxTokensEscalate int

	// CompactThreshold controls history compaction: when the most recent
	// context sent (lastInputTokens) crosses the effective trigger, the next
	// Run/RunStream summarizes the older turns before continuing. Semantics:
	// <0 disables; ==0 auto (a fraction of the model's context window, the
	// default); >0 is an explicit token count. See compactTriggerTokens.
	CompactThreshold int

	// CompactAutoFraction is the share (0.0–1.0) of the model's context window
	// at which auto-compaction triggers when CompactThreshold == 0. Zero uses
	// the built-in default (0.75). Values outside 0–1 are clamped.
	CompactAutoFraction float64

	// CompactBatchThreshold controls compaction after a tool batch, before the
	// next LLM call. Semantics mirror CompactThreshold:
	//   < 0  → disabled (never compact between batches)
	//   == 0 → follow compactTriggerTokens (the default)
	//   > 0  → that explicit token count
	CompactBatchThreshold int

	// overflow handles "context too long" 400 errors by compressing history
	// and retrying. Aligned with Ruby's perform_context_overflow_compression.
	overflow overflowRecovery

	// usageMu guards the cumulative counters below. They are written by the
	// turn loop (accrueUsage) AND by sub-agent goroutines (AccrueChildUsage,
	// which run concurrently when a sub_agent batch fans out), and read from
	// the TUI goroutine (SessionTokens / ContextUsage / SessionCacheTokens).
	usageMu sync.Mutex
	// Cumulative token counts for this session (all turns combined).
	sessionInputTokens  int
	sessionOutputTokens int
	// Cumulative cache accounting for this session.
	sessionCacheReadTokens  int
	sessionCacheWriteTokens int
	// lastInputTokens is the size of the most recently sent context, used as
	// the compaction trigger.
	lastInputTokens int

	// Inbox holds user messages that arrived while a turn was running.
	// The run loop drains it at the start of each iteration, before the LLM
	// call, so messages enter history in chronological order. This mirrors
	// Ruby octo's @inbox and keeps mid-turn input handling simple.
	Inbox Inbox

	// UserInputHook, when set, is called with each user input just before it is
	// appended to history. A non-empty return is prepended to the user message
	// (separated by a blank line). It exists to re-surface memory rules at the
	// point of action without touching the cached system prompt; the reminder
	// rides the message stream instead. The hook must not mutate state the
	// caller relies on elsewhere — it's invoked once per appended user turn.
	UserInputHook func(userInput string) string

	// pendingUserBlocks holds content blocks (e.g. images pasted in the TUI)
	// to merge into the next user message. Set via AttachUserBlocks and
	// consumed exactly once by the next appendUserInput, alongside the text.
	pendingUserBlocks []ContentBlock

	// turnIterations is the number of provider round-trips (loop iterations)
	// executed during the most recent Run/RunStream call. It is set when the
	// runLoop returns so callers can surface it in UI (e.g. "3 iterations").
	turnIterations int
}

// AttachUserBlocks queues content blocks — typically image blocks — to be
// folded into the next user message appended by a Turn/Run/RunStream call.
// The blocks are consumed exactly once (by the next appendUserInput) and then
// cleared. Call it immediately before the run so the text and the attachments
// land on the same user turn. Passing nil clears any queued blocks.
func (a *Agent) AttachUserBlocks(blocks []ContentBlock) {
	a.pendingUserBlocks = blocks
}

// appendUserInput appends userInput to history, first prepending any
// UserInputHook output. It stays a single appended message so the error-path
// popLast contract in Turn/TurnStream/runLoop still removes exactly one turn.
func (a *Agent) appendUserInput(userInput string) {
	text := userInput
	if a.UserInputHook != nil {
		if reminder := a.UserInputHook(userInput); reminder != "" {
			text = reminder + "\n\n" + userInput
		}
	}
	// Attachments (e.g. a pasted image) ride on the same user turn as the
	// text. Consume them exactly once: build a multi-part message with an
	// optional leading text block followed by the attachment blocks.
	if len(a.pendingUserBlocks) > 0 {
		blocks := make([]ContentBlock, 0, 1+len(a.pendingUserBlocks))
		if text != "" {
			blocks = append(blocks, NewTextBlock(text))
		}
		blocks = append(blocks, a.pendingUserBlocks...)
		a.pendingUserBlocks = nil
		a.History.Append(Message{Role: RoleUser, Blocks: blocks})
		return
	}
	a.History.Append(NewUserMessage(text))
}

// StopReason sentinels set on the Reply when a loop budget is exhausted.
// They are NOT provider stop reasons — the agent synthesises them so callers
// can distinguish "the model finished" from "we cut it off".
const (
	StopReasonMaxTurns    = "max_turns"
	StopReasonInterrupted = "interrupted"
	// StopReasonMaxTokens is the canonical output-truncation sentinel. Provider
	// adapters normalise their wire value to it (Anthropic "max_tokens", OpenAI
	// "length") so the loop checks one thing. The loop also reuses it as the
	// synthetic StopReason when a turn is ended because the response stayed
	// truncated even after escalation. See dev-docs/truncation-recovery.md.
	StopReasonMaxTokens = "max_tokens"
)

// interruptNote caps an interrupted turn as an assistant message so history
// keeps the user/assistant alternation the next turn depends on.
const interruptNote = "[Interrupted by user.]"

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
	if userInput == "" && len(a.pendingUserBlocks) == 0 {
		return Reply{}, fmt.Errorf("agent: userInput must be non-empty")
	}

	// Append user message first so the snapshot the Sender sees includes it.
	a.appendUserInput(userInput)

	reply, err := a.Sender.SendMessages(ctx, a.Model, a.System, a.History.Snapshot(), a.MaxTokens)
	if err != nil {
		// Pop the user message we just appended so retrying with the same
		// History doesn't duplicate it. Cheaper than transactional locking
		// since History is only mutated from this goroutine in M1.2.
		a.History.popLast()
		return Reply{}, fmt.Errorf("agent: send: %w", err)
	}

	a.History.Append(NewAssistantMessage(reply.Content))
	a.accrueUsage(reply)
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
	onThinking func(thinkingDelta string),
) (Reply, error) {
	if a.Sender == nil {
		return Reply{}, fmt.Errorf("agent: no Sender configured")
	}
	if a.Model == "" {
		return Reply{}, fmt.Errorf("agent: Model is required")
	}
	if userInput == "" && len(a.pendingUserBlocks) == 0 {
		return Reply{}, fmt.Errorf("agent: userInput must be non-empty")
	}

	a.appendUserInput(userInput)

	var (
		reply Reply
		err   error
	)
	if ss, ok := a.Sender.(StreamingSender); ok {
		reply, err = ss.StreamMessages(ctx, a.Model, a.System, a.History.Snapshot(), a.MaxTokens, onChunk, onThinking)
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
	a.accrueUsage(reply)
	return reply, nil
}

// defaultMaxTurns is the fallback per-Run loop cap when Agent.MaxTurns is
// unset (0). A "turn" here is one provider round-trip inside the agentic
// loop; the cap stops a misbehaving model from looping on tools forever.
const defaultMaxTurns = 100

// unlimitedTurns signals no cap (used for unattended runs).
const unlimitedTurns = -1

// maxTruncationResumes caps how many times layer-2 resume-and-chunk fires per
// run. Each resume appends the partial reply + a recovery prompt, consuming
// another iteration of the loop budget. Three resumes is enough for most
// large prose without risking an infinite loop.
const maxTruncationResumes = 3

// truncationResumePrompt is injected as a user message when a text reply is
// cut off by the output-token cap and escalation (layer 1) didn't help.
// The model sees its own partial output in history and is asked to continue
// from exactly where it left off.
const truncationResumePrompt = "You were cut off mid-thought. Continue exactly where you left off and complete your response. Do not repeat what you've already written."

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
	if userInput == "" && len(a.pendingUserBlocks) == 0 {
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
		func(ctx context.Context, msgs []Message, maxTokens int) (Reply, error) {
			return ts.SendMessagesWithTools(ctx, a.Model, a.System, msgs, maxTokens, tools)
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
	if userInput == "" && len(a.pendingUserBlocks) == 0 {
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

	// onThinking adapts reasoning-trace fragments into EventThinkingDelta
	// events. Nil-safe; empty deltas are dropped. Fires only when the Sender
	// surfaces reasoning at all.
	onThinking := func(delta string) {
		if handler == nil || delta == "" {
			return
		}
		handler(AgentEvent{Kind: EventThinkingDelta, Text: delta})
	}

	// No tools → plain TurnStream with the event-adapting onChunk. The
	// terminal EventTurnDone is fired here so the caller's contract is
	// identical regardless of whether tools were used.
	if len(tools) == 0 || executor == nil {
		reply, err := a.TurnStream(ctx, userInput, onChunk, onThinking)
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
			func(ctx context.Context, msgs []Message, maxTokens int) (Reply, error) {
				return tss.StreamMessagesWithTools(ctx, a.Model, a.System, msgs, maxTokens, tools, onChunk, onToolDelta, onThinking)
			})
	}
	if ts, ok := a.Sender.(ToolSender); ok {
		return a.runLoop(ctx, userInput, tools, executor, handler,
			func(ctx context.Context, msgs []Message, maxTokens int) (Reply, error) {
				reply, err := ts.SendMessagesWithTools(ctx, a.Model, a.System, msgs, maxTokens, tools)
				if err == nil && reply.Content != "" {
					onChunk(reply.Content)
				}
				return reply, err
			})
	}

	// Neither tool-aware interface available → plain TurnStream with the
	// event-adapting onChunk. EventTurnDone fires on success.
	reply, err := a.TurnStream(ctx, userInput, onChunk, onThinking)
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
	send func(ctx context.Context, msgs []Message, maxTokens int) (Reply, error),
) (Reply, error) {
	// Compact older history before starting a new turn, if the last context
	// crossed the threshold. Done here (a safe between-turns boundary, history
	// ends on a complete assistant message) rather than mid-loop, where a
	// tool_use/tool_result pair could be split. A summarization failure is
	// non-fatal — we log nothing and proceed with the full history.
	_ = a.maybeCompact(ctx, handler)

	a.appendUserInput(userInput)

	limit := a.turnLimit()
	streamStalls := 0      // transient mid-stream stalls re-issued for the current round
	truncationResumes := 0 // layer-2 resume-and-chunk budget
	escalateExhausted := false
	a.turnIterations = 0
	for i := 0; limit == unlimitedTurns || i < limit; i++ {
		a.turnIterations = i + 1
		// Interrupt (Ctrl-C) between iterations — e.g. right after a tool batch.
		if ctx.Err() != nil {
			return a.finishInterrupted(handler)
		}

		// Drain inbox messages that arrived since the last iteration.
		// Done before the LLM call so the model sees mid-turn user input
		// as a first-class message boundary, not folded into tool output.
		if steerItems := a.Inbox.Drain(); len(steerItems) > 0 {
			for _, it := range steerItems {
				if len(it.Blocks) > 0 {
					blocks := make([]ContentBlock, 0, 1+len(it.Blocks))
					if it.Text != "" {
						blocks = append(blocks, NewTextBlock(it.Text))
					}
					blocks = append(blocks, it.Blocks...)
					a.History.Append(Message{Role: RoleUser, Blocks: blocks})
				} else {
					a.History.Append(NewUserMessage(it.Text))
				}
			}
			if handler != nil {
				msgs := make([]string, len(steerItems))
				for i, it := range steerItems {
					msgs[i] = it.Text
				}
				handler(AgentEvent{Kind: EventSteerInjected, Messages: msgs, Steer: steerItems})
			}
		}

		// Defensive check: ensure every tool_use block has a matching tool_result.
		// This handles edge cases where compaction, overflow recovery, or an
		// interrupted dispatchTools left orphaned tool_use blocks. Synthesizing
		// error tool_results prevents Anthropic HTTP 400 errors.
		a.ensureToolPairing()

		reply, err := send(ctx, a.History.Snapshot(), a.MaxTokens)
		if err != nil {
			// Interrupt during the provider call: finalize cleanly rather than
			// surfacing context.Canceled as a turn error.
			if ctx.Err() != nil {
				return a.finishInterrupted(handler)
			}

			// ── Context overflow recovery (aligned with Ruby) ──
			if a.overflow.tryRecover(ctx, a, err, handler) {
				// Compression succeeded — retry the same iteration
				// without incrementing i or popping user message
				continue
			}

			// ── Transient mid-stream stall recovery ──
			// The streaming idle-timeout watchdog (or similar) fired: the server
			// went silent mid-response. The partial reply was never appended, so
			// re-issuing this round from the unchanged history is safe — only the
			// already-streamed text is re-emitted. Bounded so a persistently dead
			// stream still ends the turn.
			if isTransientStreamErr(err) && streamStalls < maxStreamStalls {
				streamStalls++
				if handler != nil {
					handler(AgentEvent{Kind: EventTextDelta, Text: fmt.Sprintf(
						"\n[octo] stream stalled; retrying (%d/%d)…\n", streamStalls, maxStreamStalls)})
				}
				continue
			}

			if i == 0 {
				a.History.popLast()
			}
			return Reply{}, fmt.Errorf("agent: loop[%d]: %w", i, err)
		}

		// Success — reset the overflow and stream-stall budgets for the next round.
		a.overflow.reset()
		streamStalls = 0

		a.accrueUsage(reply)

		// ── Output-truncation recovery (layer 1) ──
		// The response was cut off by the output-token cap. Retry this same
		// round once at the escalated cap, from the UNCHANGED history (the
		// truncated reply is never appended), so a half-written tool call is
		// regenerated with more room — no provider-specific partial-tool_use
		// handling needed. Fires only when escalation raises the cap.
		// See dev-docs/truncation-recovery.md.
		if isTruncated(reply.StopReason) && !escalateExhausted && a.MaxTokensEscalate > a.MaxTokens {
			escalated, eerr := send(ctx, a.History.Snapshot(), a.MaxTokensEscalate)
			switch {
			case eerr == nil:
				reply = escalated
				a.accrueUsage(reply)
				truncationResumes = 0 // escalation solved it — reset resume budget
				escalateExhausted = false
			case ctx.Err() != nil:
				return a.finishInterrupted(handler)
			case isMaxTokensTooLargeErr(eerr):
				// The model's ceiling is below the escalation target (e.g.
				// Claude 3 caps at 4096). Keep the truncated reply and fall
				// through to the graceful stop below.
			default:
				if i == 0 {
					a.History.popLast()
				}
				return Reply{}, fmt.Errorf("agent: loop[%d] escalate: %w", i, eerr)
			}
		}

		// ── Output-truncation recovery (layer 2) ──
		// When escalation was attempted (cap raised) but the reply is still
		// truncated, keep the partial text in history and prompt the model to
		// continue. This covers long prose replies that exceed even the escalated
		// cap. Limited to maxTruncationResumes to prevent infinite loops. Skipped
		// for truncated tool_use blocks (partial tool calls are unsafe in
		// OpenAI-protocol history) and when escalation is disabled.
		if isTruncated(reply.StopReason) && a.MaxTokensEscalate > a.MaxTokens && reply.Content != "" && truncationResumes < maxTruncationResumes {
			a.History.Append(NewAssistantMessage(reply.Content))
			a.History.Append(NewUserMessage(truncationResumePrompt))
			truncationResumes++
			escalateExhausted = true
			if handler != nil {
				handler(AgentEvent{Kind: EventTextDelta, Text: "\n[octo] response truncated — resuming…\n"})
			}
			continue
		}

		// Still truncated and ineligible for layer-2 resume (tool_use truncation
		// or resume budget exhausted): end the turn cleanly rather than dispatching
		// a half-formed tool call or returning an empty reply.
		if isTruncated(reply.StopReason) {
			return a.budgetStop(handler, StopReasonMaxTokens,
				"[octo] Stopped: the response was truncated at the output-token cap. "+
					"Raise --max-tokens / --max-tokens-escalate, or ask me to continue in smaller steps.")
		}

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

			// Turn-in compaction check: after a tool batch, history may have
			// grown significantly. If the estimated size is near the window,
			// compact before the next LLM call to avoid a 400.
			if a.shouldCompactBetweenBatches() {
				_ = a.maybeCompact(ctx, handler)
			}
			continue
		}

		content := reply.Content
		if content == "" {
			content = textFromBlocks(reply.Blocks)
		}
		a.History.Append(NewAssistantMessage(content))
		reply.Content = content

		// A mid-turn steer (text and/or pasted images) arrived while the model
		// was producing this answer. Don't end the turn: loop so the next
		// iteration drains it into history and the model responds in-turn,
		// rather than stranding it for the front-end to re-queue as a fresh
		// turn (which would drop any image blocks). EventTurnDone stays
		// once-only — it fires only when we actually return below.
		if a.Inbox.HasPending() {
			continue
		}

		if handler != nil {
			r := reply
			handler(AgentEvent{Kind: EventTurnDone, Reply: &r})
		}
		a.turnIterations = i + 1
		return reply, nil
	}

	// Loop cap reached while the model still wanted to keep going. End the
	// run gracefully rather than erroring — the history holds the partial
	// progress and the caller gets a clear, non-fatal explanation.
	// turnIterations already holds the last loop index (set at the top of each
	// iteration), so the UI reports the actual count before the cap.
	return a.budgetStop(handler, StopReasonMaxTurns, fmt.Sprintf(
		"[octo] Stopped: reached the max-turns limit (%d). The task may be incomplete — "+
			"raise --max-turns or send another message to continue.", limit))
}

// finishInterrupted finalizes a turn cut short by context cancellation
// (Ctrl-C) so the history stays well-formed for the next turn, then returns
// context.Canceled so the caller can recognise the interrupt. It restores the
// user/assistant alternation invariant:
//
//   - last message is the unanswered user input → drop it (turn never happened)
//   - last message is a tool_result (user role) → cap with an assistant note,
//     so the next user turn doesn't produce two user messages in a row
//   - last message is an assistant(tool_use) without tool_result → synthesize
//     error tool_results and cap with an assistant note
//   - last message is already a plain assistant turn → nothing to do
//
// The synthesized tool_result blocks for interrupted tool calls are normally
// produced by dispatchTools itself (cancelled executions become is_error
// results). This function handles the edge case where dispatchTools didn't
// complete before the interrupt was detected.
func (a *Agent) finishInterrupted(handler EventHandler) (Reply, error) {
	msgs := a.History.Snapshot()
	if n := len(msgs); n > 0 {
		last := msgs[n-1]
		switch {
		case last.Role == RoleAssistant && hasToolUse(last):
			// Orphaned assistant(tool_use) — dispatchTools didn't produce results.
			// Synthesize error tool_results so the next send() doesn't 400.
			results := synthesizeInterruptedToolResults(last.Blocks)
			if len(results) > 0 {
				a.History.Append(NewToolResultMessage(results))
			}
			a.History.Append(NewAssistantMessage(interruptNote))
		case last.Role == RoleUser && hasToolResult(last):
			a.History.Append(NewAssistantMessage(interruptNote))
		case last.Role == RoleUser:
			a.History.popLast()
		}
	}
	reply := Reply{Content: interruptNote, StopReason: StopReasonInterrupted}
	if handler != nil {
		r := reply
		handler(AgentEvent{Kind: EventTurnDone, Reply: &r})
	}
	// turnIterations stays at its current value (set by the caller in runLoop
	// before finishInterrupted is reached) so the UI shows how many iterations
	// completed before the interrupt.
	return reply, context.Canceled
}

// TurnIterations returns the number of provider round-trips executed during
// the most recent Run/RunStream call. It is 0 before the first run.
func (a *Agent) TurnIterations() int {
	return a.turnIterations
}

// turnLimit resolves the per-Run loop cap.
//
//	> 0  → explicit cap
//	0    → defaultMaxTurns (interactive default)
//	< 0  → unlimited (unattended runs)
func (a *Agent) turnLimit() int {
	if a.MaxTurns > 0 {
		return a.MaxTurns
	}
	if a.MaxTurns < 0 {
		return unlimitedTurns
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

// isTruncated reports whether a reply was cut off by the output-token cap.
// Adapters normalise their wire value to StopReasonMaxTokens, so the loop only
// checks this one sentinel.
func isTruncated(stopReason string) bool {
	return stopReason == StopReasonMaxTokens
}

// maxStreamStalls bounds how many times a single round may be re-issued after a
// transient mid-stream stall before the turn is failed. Reset after any healthy
// round, so a long turn that stalls at different points each gets a fresh budget.
const maxStreamStalls = 3

// transientStreamError is implemented by provider errors that represent a
// recoverable mid-stream stall (e.g. the streaming idle-timeout watchdog
// firing). Declaring it as an interface here lets the loop classify such errors
// without the agent package importing any provider package — Go interface
// satisfaction is structural, so the provider error just needs the method.
type transientStreamError interface{ TransientStream() bool }

// isTransientStreamErr reports whether err (or anything it wraps) is a
// recoverable mid-stream stall. The round can be safely re-issued because the
// partial reply was never appended to history.
func isTransientStreamErr(err error) bool {
	var t transientStreamError
	return errors.As(err, &t) && t.TransientStream()
}

// suggestMaxTokens caps the follow-up suggestion response — it's one short line.
const suggestMaxTokens = 256

const suggestInstruction = "Suggest ONE concise, specific next message I (the user) might send to continue this work. " +
	"Do not call any tools — reply with the message text only, phrased as I would type it, a single line, " +
	"no preamble, no quotes, no numbering."

// Suggest produces a single follow-up message the user might want to send next,
// based on the conversation so far. It is a throwaway provider call: the
// instruction is appended to a snapshot of history (never to the live History),
// so it doesn't pollute the conversation, and its token usage is not accrued
// into the session. Returns "" (no error) when there's nothing to suggest.
//
// tools should be the SAME toolbelt the agentic loop uses. Anthropic's cache
// prefix is ordered tools → system → messages, so sending the identical tools
// makes this call reuse the main conversation's prompt cache (the whole history
// is billed at the cheap cache-read rate) instead of re-billing it in full.
// Without tools the prefix diverges at block 0 and nothing is cached. The model
// is told not to call tools; if it returns a tool_use anyway, Content is empty
// and we simply produce no suggestion that turn.
func (a *Agent) Suggest(ctx context.Context, tools []ToolDefinition) (string, error) {
	if a.Sender == nil || a.Model == "" {
		return "", fmt.Errorf("agent: suggest: not configured")
	}
	snap := a.History.Snapshot()
	if len(snap) == 0 {
		return "", nil
	}
	msgs := make([]Message, 0, len(snap)+1)
	msgs = append(msgs, snap...)
	msgs = append(msgs, NewUserMessage(suggestInstruction))

	var reply Reply
	var err error
	if ts, ok := a.Sender.(ToolSender); ok && len(tools) > 0 {
		reply, err = ts.SendMessagesWithTools(ctx, a.Model, a.System, msgs, suggestMaxTokens, tools)
	} else {
		reply, err = a.Sender.SendMessages(ctx, a.Model, a.System, msgs, suggestMaxTokens)
	}
	if err != nil {
		return "", err
	}
	return cleanSuggestion(reply.Content), nil
}

// titleMaxTokens caps the title response — it's a handful of words.
const titleMaxTokens = 32

const titleInstruction = "Summarize this conversation as a short title of at most 6 words. " +
	"Reply with the title text only — no preamble, no quotes, no trailing punctuation, no markdown."

// GenerateTitle produces a short title summarizing the conversation so far, for
// display in the session list. Like Suggest it is a throwaway provider call: the
// instruction is appended to a snapshot of history (never the live History) and
// its token usage is not accrued into the session. Returns "" (no error) when
// there's nothing to title.
//
// tools should be the SAME toolbelt the agentic loop uses so the request reuses
// the main conversation's prompt cache (see Suggest for the cache-prefix
// rationale). The model is told not to call tools; a stray tool_use yields empty
// Content and we simply produce no title.
func (a *Agent) GenerateTitle(ctx context.Context, tools []ToolDefinition) (string, error) {
	if a.Sender == nil || a.Model == "" {
		return "", fmt.Errorf("agent: title: not configured")
	}
	snap := a.History.Snapshot()
	if len(snap) == 0 {
		return "", nil
	}
	msgs := make([]Message, 0, len(snap)+1)
	msgs = append(msgs, snap...)
	msgs = append(msgs, NewUserMessage(titleInstruction))

	var reply Reply
	var err error
	if ts, ok := a.Sender.(ToolSender); ok && len(tools) > 0 {
		reply, err = ts.SendMessagesWithTools(ctx, a.Model, a.System, msgs, titleMaxTokens, tools)
	} else {
		reply, err = a.Sender.SendMessages(ctx, a.Model, a.System, msgs, titleMaxTokens)
	}
	if err != nil {
		return "", err
	}
	return cleanTitle(reply.Content), nil
}

// cleanTitle reduces the model's reply to a single tidy line: first non-empty
// line, stripped of surrounding quotes/markdown and trailing punctuation, capped
// to a list-friendly length.
func cleanTitle(s string) string {
	const maxLen = 60
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "# ")
		// The model sometimes wraps the title in quotes AND adds trailing
		// punctuation (e.g. `"Fix the bug".`), so trim both sets repeatedly
		// until the string stops shrinking — one pass leaves the outer quote
		// stranded behind the period.
		for {
			trimmed := strings.TrimSpace(line)
			trimmed = strings.Trim(trimmed, "\"'`*")
			trimmed = strings.TrimRight(trimmed, ".。!！?？")
			if trimmed == line {
				break
			}
			line = trimmed
		}
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxLen {
			return strings.TrimSpace(string(r[:maxLen-1])) + "…"
		}
		return line
	}
	return ""
}

// cleanSuggestion picks the first non-empty line and strips list/quote
// decoration the model sometimes adds despite the instruction.
func cleanSuggestion(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimSpace(strings.Trim(line, "\"'`"))
		if line != "" {
			return line
		}
	}
	return ""
}

// isMaxTokensTooLargeErr best-effort detects a provider rejecting an escalated
// max_tokens because it exceeds the model's ceiling (e.g. Claude 3 caps at
// 4096). Both Anthropic and OpenAI-protocol backends name max_tokens in the
// message; the surrounding wording varies, so this matches loosely. On a false
// negative the escalation error simply surfaces as a normal turn error.
func isMaxTokensTooLargeErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "max_tokens") && !strings.Contains(s, "max tokens") {
		return false
	}
	for _, marker := range []string{"exceed", "greater than", "too large", "maximum", "at most", "less than or equal"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
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

// readOnlyTools are the built-in tools safe for concurrent dispatch. A batch
// composed entirely of these can be executed concurrently (see dispatchTools).
// Conservative by design: anything that writes, edits, or shells out is absent,
// so adding a new mutating tool can never accidentally be parallelised.
var readOnlyTools = map[string]bool{
	"read_file":  true,
	"glob":       true,
	"grep":       true,
	"web_fetch":  true,
	"web_search": true,
}

// toolCall pairs a tool_use block with the gate's verdict.
type toolCall struct {
	block      ContentBlock
	denyReason string // non-empty → blocked by the gate; don't execute
}

// dispatchTools runs every tool_use block in blocks and returns the matching
// tool_result blocks (order preserved). Errors become IsError results so the
// model can recover.
//
// The permission gate runs first, serially, for every call — an interactive
// "ask" prompt must never race another on stdin. Execution then happens in one
// of two modes:
//   - Parallel, when the batch has >1 executable call and every executable
//     call is a read-only tool (readOnlyTools). The model frequently fires
//     several read_file/grep calls at once; running them concurrently cuts
//     latency. Read-only tools don't stream, so no progress events are lost.
//   - Serial otherwise (a single call, or any mutating/streaming tool present),
//     preserving EventToolProgress for StreamingToolExecutor tools.
//
// handler may be nil (no events); gate may be nil (no gating). Parallel mode
// requires executor.Execute to be safe for concurrent calls on distinct
// inputs — DefaultRegistry is (its only shared state, the read tracker, is
// mutex-guarded).
func dispatchTools(ctx context.Context, executor ToolExecutor, blocks []ContentBlock, handler EventHandler, gate PermissionGate) ([]ContentBlock, error) {
	// Pass 1 — collect calls and run the gate serially.
	var calls []toolCall
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		c := toolCall{block: b}
		if gate != nil {
			if allowed, reason := gate.Check(ctx, b.Name, b.Input); !allowed {
				if reason == "" {
					reason = "permission denied"
				}
				c.denyReason = reason
			}
		}
		calls = append(calls, c)
	}

	// Pass 2 — execute. Each tool may return multiple blocks (e.g. tool_result +
	// image), so we collect per-tool slices and flatten at the end.
	var resultSlices [][]ContentBlock

	if canParallelize(calls) {
		var wg sync.WaitGroup
		resultSlices = make([][]ContentBlock, len(calls))
		for i := range calls {
			if calls[i].denyReason != "" {
				resultSlices[i] = []ContentBlock{NewToolResultBlock(calls[i].block.ID, calls[i].denyReason, true)}
				continue
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := executor.Execute(ctx, calls[i].block.Name, calls[i].block.Input)
				resultSlices[i] = toolResultBlocks(calls[i].block.ID, res, err)
			}(i)
		}
		wg.Wait()
		return flattenResults(resultSlices), nil
	}

	streaming, hasStreaming := executor.(StreamingToolExecutor)
	resultSlices = make([][]ContentBlock, len(calls))
	for i := range calls {
		b := calls[i].block
		if calls[i].denyReason != "" {
			resultSlices[i] = []ContentBlock{NewToolResultBlock(b.ID, calls[i].denyReason, true)}
			continue
		}
		var (
			res     ToolResult
			execErr error
		)
		if hasStreaming && handler != nil {
			toolID, toolName := b.ID, b.Name
			progress := func(chunk string) {
				if chunk == "" {
					return
				}
				handler(AgentEvent{Kind: EventToolProgress, ToolID: toolID, ToolName: toolName, Chunk: chunk})
			}
			res, execErr = streaming.ExecuteStream(ctx, b.Name, b.Input, progress)
		} else {
			res, execErr = executor.Execute(ctx, b.Name, b.Input)
		}
		resultSlices[i] = toolResultBlocks(b.ID, res, execErr)
	}
	return flattenResults(resultSlices), nil
}

// canParallelize reports whether a batch can run concurrently: more than one
// executable (non-denied) call, every one a known read-only tool.
func canParallelize(calls []toolCall) bool {
	executable := 0
	for _, c := range calls {
		if c.denyReason != "" {
			continue
		}
		if !readOnlyTools[c.block.Name] {
			return false
		}
		executable++
	}
	return executable > 1
}

// toolResultBlocks builds a slice of content blocks from a ToolResult.
// The first element is always the tool_result block; any additional blocks
// (e.g. images) from the result are appended after it.
func toolResultBlocks(id string, result ToolResult, err error) []ContentBlock {
	if err != nil {
		return []ContentBlock{NewToolResultBlock(id, microCompact(err.Error()), true)}
	}
	blocks := make([]ContentBlock, 0, 1+len(result.Blocks))
	blocks = append(blocks, NewToolResultBlock(id, microCompact(result.Text), false))
	blocks = append(blocks, result.Blocks...)
	return blocks
}

// flattenResults collapses a per-tool slice-of-slices into a single flat slice.
func flattenResults(slices [][]ContentBlock) []ContentBlock {
	var total int
	for _, s := range slices {
		total += len(s)
	}
	out := make([]ContentBlock, 0, total)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
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

// accrueUsage folds one reply's token/cache counts into the session totals
// and records the context size used by the compaction trigger.
func (a *Agent) accrueUsage(reply Reply) {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	a.sessionInputTokens += reply.InputTokens
	a.sessionOutputTokens += reply.OutputTokens
	a.sessionCacheReadTokens += reply.CacheReadTokens
	a.sessionCacheWriteTokens += reply.CacheWriteTokens
	// Context occupancy is the WHOLE prompt sent, not just the uncached part.
	// On a cache hit the provider reports InputTokens as only the non-cached
	// remainder and moves the bulk into CacheReadTokens (cache_read_input_tokens)
	// / CacheWriteTokens (cache_creation_input_tokens) — e.g. Kimi streaming
	// returns input=114, cache_read=2304 for a 2418-token prompt. Summing all
	// three keeps the ctx-usage gauge honest; using InputTokens alone makes it
	// read far too low once the cached prefix dominates.
	a.lastInputTokens = reply.InputTokens + reply.CacheReadTokens + reply.CacheWriteTokens
}

// SessionTokens returns the cumulative input and output token counts for all
// turns made so far in this Agent's lifetime.
func (a *Agent) SessionTokens() (inputTokens, outputTokens int) {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	return a.sessionInputTokens, a.sessionOutputTokens
}

// AccrueChildUsage folds tokens spent by a spawned sub-agent into this
// agent's session totals, so SessionTokens still reports one
// consolidated number even when the LLM used sub_agent. cache totals are
// left untouched — the child runs against the same provider but reports its
// own cache hits internally; the parent only sees the bottom-line counts here.
func (a *Agent) AccrueChildUsage(inputTokens, outputTokens int) {
	a.addUsage(inputTokens, outputTokens)
}

// addUsage folds input/output token counts into the session totals under the
// usage lock. Shared by AccrueChildUsage (concurrent sub-agent goroutines) and
// the internal sub-operation accruals (compaction / consolidation / planning),
// all of which can run while a sub-agent goroutine is still accruing.
func (a *Agent) addUsage(inputTokens, outputTokens int) {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	a.sessionInputTokens += inputTokens
	a.sessionOutputTokens += outputTokens
}

// resetContextTrigger zeroes the compaction-trigger context size under the
// usage lock (it's read from the TUI goroutine via ContextUsage).
func (a *Agent) resetContextTrigger() {
	a.usageMu.Lock()
	a.lastInputTokens = 0
	a.usageMu.Unlock()
}

// ContextUsage reports how full the model's context window is: used is the
// most recently sent context size in tokens (reported by the provider), and
// window is the model's approximate context-window size. Lets the TUI status
// bar render a "ctx N%" gauge. window is always > 0.
func (a *Agent) ContextUsage() (used, window int) {
	a.usageMu.Lock()
	used = a.lastInputTokens
	a.usageMu.Unlock()
	return used, contextWindow(a.Model)
}

// SessionCacheTokens returns the cumulative cache read/write token counts.
// Read is input served from cache (cheap); write is input written into the
// cache (Anthropic only). Both zero when the backend reports no cache info.
func (a *Agent) SessionCacheTokens() (readTokens, writeTokens int) {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	return a.sessionCacheReadTokens, a.sessionCacheWriteTokens
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
