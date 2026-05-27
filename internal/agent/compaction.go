package agent

import (
	"context"
	"fmt"
)

// compactKeepTurns is how many of the most recent user turns runLoop keeps
// verbatim when it compacts; everything older is folded into one summary.
const compactKeepTurns = 4

// summarizeMaxTokens caps the summary length. A summary is meant to be a
// compact carry-forward of decisions/paths/open tasks, not a transcript.
const summarizeMaxTokens = 1024

// summarizeSystem is the system prompt for the summarization side-call. It is
// deliberately small and distinct from the main session prompt — the call
// trades a cache miss (it's infrequent) for a focused instruction.
const summarizeSystem = "You are a conversation summarizer for a coding agent. " +
	"Produce a compact summary of the conversation so the agent can continue " +
	"without the full transcript. Preserve: the user's goal, decisions made, " +
	"files/paths touched, commands run and their outcomes, and any open tasks " +
	"or unresolved errors. Omit chit-chat. Write in terse bullet points."

// maybeCompact summarizes the older portion of history when the most recent
// context size (lastInputTokens) crossed CompactThreshold. It runs only at a
// safe boundary — between turns, splitting on a plain user message — so
// tool_use/tool_result pairs are never severed. A nil return means either
// "compaction disabled / not needed" or "compacted successfully"; an error
// means the summarization side-call failed (the caller logs and proceeds with
// uncompacted history rather than aborting the turn).
func (a *Agent) maybeCompact(ctx context.Context) error {
	if a.CompactThreshold <= 0 || a.lastInputTokens < a.CompactThreshold {
		return nil
	}

	msgs := a.History.Snapshot()
	split := safeSplitIndex(msgs, compactKeepTurns)
	if split <= 0 {
		return nil // not enough complete turns to safely compact yet
	}

	summary, err := a.summarize(ctx, msgs[:split])
	if err != nil {
		return fmt.Errorf("agent: compact: %w", err)
	}
	if summary == "" {
		return nil // nothing usable came back; leave history alone
	}

	// Rebuild: one summary user-message, then the kept recent turns verbatim.
	a.History.Reset()
	a.History.Append(NewUserMessage("[Earlier conversation summary]\n\n" + summary))
	for _, m := range msgs[split:] {
		a.History.Append(m)
	}
	// Reset the trigger so we don't re-compact until the context grows again.
	a.lastInputTokens = 0
	return nil
}

// summarize asks the Sender to condense the given messages into a summary.
// It appends a single user instruction after the slice (which ends on an
// assistant message, so alternation holds) and returns the reply text.
func (a *Agent) summarize(ctx context.Context, msgs []Message) (string, error) {
	req := make([]Message, 0, len(msgs)+1)
	req = append(req, msgs...)
	req = append(req, NewUserMessage(
		"Summarize the conversation above per your instructions. Output only the summary."))

	reply, err := a.Sender.SendMessages(ctx, a.Model, summarizeSystem, req, summarizeMaxTokens)
	if err != nil {
		return "", err
	}
	// Summary tokens count toward the session budget like any other call.
	a.sessionInputTokens += reply.InputTokens
	a.sessionOutputTokens += reply.OutputTokens
	return reply.Content, nil
}

// safeSplitIndex returns the history index at which compaction can split:
// messages before it are summarized, messages from it onward are kept. The
// split always lands on a plain user turn (RoleUser with no tool_result
// blocks), so a tool_use and its tool_result are never separated. It keeps
// the last keepTurns user turns; if there aren't more than that, it returns 0
// (nothing safe to compact).
func safeSplitIndex(msgs []Message, keepTurns int) int {
	var userTurns []int
	for i, m := range msgs {
		if m.Role == RoleUser && !hasToolResult(m) {
			userTurns = append(userTurns, i)
		}
	}
	if len(userTurns) <= keepTurns {
		return 0
	}
	return userTurns[len(userTurns)-keepTurns]
}

// hasToolResult reports whether m carries any tool_result block (i.e. it's a
// synthetic user message returning tool output, not a real user turn).
func hasToolResult(m Message) bool {
	for _, b := range m.Blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}
