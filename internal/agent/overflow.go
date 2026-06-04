package agent

import (
	"context"
	"strings"
)

// overflowRecovery handles "context too long" errors by compressing history
// and retrying. Aligned with Ruby's perform_context_overflow_compression.
type overflowRecovery struct {
	inProgress bool // true during recovery compression (prevents recursion)
	attempted  bool // true after one attempt per turn (max one retry)
}

// contextTooLongError detects whether an error is about exceeding the model's
// context window. Aligned with Ruby's context_too_long_error?.
//
// Coverage (verified against real production error strings):
//
//	OpenAI:    "This model's maximum context length is 128000 tokens..."
//	Anthropic: "prompt is too long: 218849 tokens > 200000 maximum"
//	Qwen:      "You passed 117345 input tokens... context length is only 125536"
//	DeepSeek:  Variants of "context length" / "tokens exceeds"
//	Generic:   "The total number of tokens exceeds the model's maximum context length"
func contextTooLongError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// Strong phrases — any one is conclusive on its own.
	strongPhrases := []string{
		"context length",
		"context_length_exceeded",
		"maximum context",
		"maximum input length",
		"prompt is too long",
		"input is too long",
		"exceeds the maximum context",
		"exceeds the model's context",
		"exceeds the model's maximum",
		"reduce the length of the input",
		"reduce the length of the messages",
		"reduce the length of your",
		"reduce the length of the prompt",
		"range of input length",
	}
	for _, p := range strongPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}

	// Pattern 1: Anthropic-style "<N> tokens > <N> maximum"
	if strings.Contains(msg, "tokens") && strings.Contains(msg, "maximum") {
		return true
	}

	// Pattern 2: Qwen-style structured field "parameter=input_tokens"
	if strings.Contains(msg, "parameter=input_tokens") {
		return true
	}

	return false
}

// tryRecover attempts to recover from a context-overflow error by compressing
// history and returning true if the caller should retry.
//
// Layer 1 (standard): pull back 1 message from tail (preserves prompt cache),
// compress, retry. Handles 99% of cases.
//
// Layer 2 (aggressive): pull back ~half the history. Sacrifices cache but
// guarantees the compression call fits.
func (r *overflowRecovery) tryRecover(ctx context.Context, a *Agent, sendErr error, handler EventHandler) bool {
	if r.attempted || r.inProgress {
		return false
	}
	if !contextTooLongError(sendErr) {
		return false
	}

	r.attempted = true
	r.inProgress = true
	defer func() { r.inProgress = false }()

	// Layer 1: standard cache-preserving compression
	if a.tryOverflowCompact(ctx, pullBackStandard, handler) {
		return true
	}

	// Layer 2: aggressive fallback
	if a.tryOverflowCompact(ctx, pullBackAggressive, handler) {
		return true
	}

	return false
}

// reset clears the attempted flag so the next turn can attempt recovery again.
func (r *overflowRecovery) reset() {
	r.attempted = false
}

const (
	pullBackStandard   = 1  // preserve cache#A
	pullBackAggressive = -1 // ~half history, computed dynamically
)

// tryOverflowCompact is the Layer 1/2 entry point for 400-recovery compression.
// It pops K messages from tail, runs compression, then reattaches them.
// Returns true if compression succeeded and history was rebuilt.
func (a *Agent) tryOverflowCompact(ctx context.Context, pullBackMode int, handler EventHandler) bool {
	trigger := a.compactTriggerTokens()
	if trigger <= 0 {
		return false // compaction disabled
	}

	msgs := a.History.Snapshot()
	if len(msgs) < 4 {
		return false // not enough to safely compact
	}

	// Compute pull-back count
	k := computePullBack(len(msgs), pullBackMode)
	if k <= 0 || k >= len(msgs)-1 {
		return false // would pop too much (keep at least system + 1)
	}

	// Save pulled-back messages
	pulledBack := make([]Message, k)
	copy(pulledBack, msgs[len(msgs)-k:])

	// Build truncated history for compression
	truncated := NewHistory()
	for _, m := range msgs[:len(msgs)-k] {
		truncated.Append(m)
	}

	// Find safe split point
	split := safeSplitIndex(truncated.Snapshot(), compactKeepTurns)
	if split <= 0 {
		return false
	}

	before := estimateMessages(msgs)
	if handler != nil {
		handler(AgentEvent{Kind: EventCompactStarted, Compact: &CompactStats{
			BeforeTokens: before,
			FoldedMsgs:   split,
			KeptTurns:    compactKeepTurns,
			MaxTokens:    summarizeMaxTokens,
		}})
	}

	// Run compression side-call on truncated history
	summary, err := a.summarize(ctx, truncated.Snapshot()[:split], handler)
	if err != nil || summary == "" {
		emitCompactDone(handler, before, before, split) // no-op: clear the indicator
		return false
	}

	// Rebuild: summary + kept recent + pulled_back
	recent := truncated.Snapshot()[split:]

	a.History.Reset()
	a.History.Append(NewUserMessage("[Earlier conversation summary]\n\n" + summary))
	for _, m := range recent {
		a.History.Append(m)
	}
	for _, m := range pulledBack {
		a.History.Append(m)
	}

	a.resetContextTrigger() // reset trigger so we don't immediately re-compact
	emitCompactDone(handler, before, estimateMessages(a.History.Snapshot()), split)
	return true
}

func computePullBack(historyLen, mode int) int {
	if mode == pullBackStandard {
		return 1
	}
	// Aggressive: pop ~half, bounded
	half := historyLen / 2
	return clamp(half, 4, historyLen-2) // keep system + at least 1 message
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
