package agent

import (
	"context"
	"fmt"
	"strings"
)

// consolidateMaxTokens caps the consolidation side-call output.
const consolidateMaxTokens = 4096

// consolidateSystem instructs the consolidation side-call to maintain (rather
// than rebuild) a summary: it gets the current summary plus any new notes since
// the last pass and emits the updated summary. This keeps the input bounded as
// the memory store grows.
const consolidateSystem = `You maintain a coding agent's cross-session memory summary. You will receive
the current consolidated summary (which may be empty on the first pass) and
any new memory notes added since the last consolidation. Produce the UPDATED
summary: fold the new notes into the existing summary, dedupe, drop anything
stale or trivial, and keep the load-bearing facts. Be terse — bullet points,
grouped loosely by kind (who the user is, how they like to work, ongoing
project context, useful references). Output only the updated summary text.`

// ConsolidateMemory runs the (incremental) consolidation side-call: it folds
// newNotes into priorSummary and returns the updated summary. Either argument
// may be empty — empty priorSummary means "first pass"; empty newNotes means
// "no new material" and the call short-circuits.
func (a *Agent) ConsolidateMemory(ctx context.Context, priorSummary, newNotes string) (string, error) {
	if a.Sender == nil {
		return "", fmt.Errorf("agent: no Sender configured")
	}
	priorSummary = strings.TrimSpace(priorSummary)
	newNotes = strings.TrimSpace(newNotes)
	if priorSummary == "" && newNotes == "" {
		return "", nil
	}

	var prompt strings.Builder
	if priorSummary != "" {
		prompt.WriteString("Current consolidated summary:\n\n")
		prompt.WriteString(priorSummary)
		prompt.WriteString("\n\n")
	}
	if newNotes != "" {
		prompt.WriteString("New memory notes since last consolidation:\n\n")
		prompt.WriteString(newNotes)
		prompt.WriteString("\n\n")
	}
	prompt.WriteString("Produce the updated consolidated summary per your instructions.")

	req := []Message{NewUserMessage(prompt.String())}
	reply, err := a.Sender.SendMessages(ctx, a.Model, consolidateSystem, req, consolidateMaxTokens)
	if err != nil {
		return "", err
	}
	a.addUsage(reply.InputTokens, reply.OutputTokens)
	return strings.TrimSpace(reply.Content), nil
}
