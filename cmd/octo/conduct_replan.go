package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/conductor"
)

const replanMaxTokens = 2048

// replanSystem instructs the model to repair a stuck plan. It is deliberately
// conservative: the default is to change nothing (an empty result), because an
// over-eager re-planner that keeps inventing units defeats convergence.
const replanSystem = `You are the re-planner for an autonomous engineering effort. The work is
organised as a ledger of units (a DAG). The loop has gotten STUCK: some units
are blocked (their attempts were exhausted) or depend on blocked/abandoned
units, so no unit is currently runnable, yet the goal isn't done.

Your job: propose the MINIMAL ledger edit that could unblock progress. Common
fixes: split a too-large blocked unit into smaller units; insert a missing
prerequisite unit that a blocked unit actually needed; or abandon a unit that
turned out to be unnecessary or impossible.

Output ONE JSON object:

  {
    "add":     [ { "description": "<what a worker should do>", "blocked_by": [<ids>] }, ... ],
    "abandon": [ <unit ids to drop> ],
    "note":    "<one line explaining the change>"
  }

Rules:
- blocked_by entries reference EXISTING unit ids by their positive id. To
  reference another unit you are adding in THIS response, use a negative index:
  -1 = the 1st added unit, -2 = the 2nd, etc.
- Prefer the smallest change that helps. If you genuinely cannot improve the
  situation, return {"add":[],"abandon":[],"note":"no change"} and the loop
  will stop and ask a human.
- Do not restate work that is already Done. Do not re-add blocked units
  verbatim — split or re-scope them.
- Output only the JSON object, no prose, no code fences.`

// agentReplanner is the LLM-backed conductor.Replanner.
type agentReplanner struct{ a *agent.Agent }

func newAgentReplanner(a *agent.Agent) *agentReplanner { return &agentReplanner{a: a} }

func (r *agentReplanner) Replan(ctx context.Context, l *conductor.Ledger, trigger conductor.ReplanTrigger) (conductor.ReplanResult, error) {
	if r.a == nil || r.a.Sender == nil {
		return conductor.ReplanResult{}, nil
	}
	user := buildReplanUserMessage(l, trigger)
	reply, err := r.a.Sender.SendMessages(ctx, r.a.Model, replanSystem,
		[]agent.Message{agent.NewUserMessage(user)}, replanMaxTokens)
	if err != nil {
		return conductor.ReplanResult{}, err
	}
	return parseReplan(reply.Content)
}

// buildReplanUserMessage renders the stuck ledger compactly for the model.
func buildReplanUserMessage(l *conductor.Ledger, trigger conductor.ReplanTrigger) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal:\n%s\n\n", l.Goal)
	fmt.Fprintf(&b, "Why you were called: the loop is stuck (%s).\n\n", trigger.Reason)
	b.WriteString("Current units:\n")
	for _, u := range l.Units {
		fmt.Fprintf(&b, "  #%d [%s]", u.ID, u.Status)
		if len(u.BlockedBy) > 0 {
			fmt.Fprintf(&b, " (blocked_by %v)", u.BlockedBy)
		}
		fmt.Fprintf(&b, " %s\n", oneLineN(u.Description, 100))
		if u.Status == conductor.UnitBlocked && u.LastError != "" {
			fmt.Fprintf(&b, "       last failure: %s\n", oneLineN(u.LastError, 160))
		}
	}
	b.WriteString("\nPropose the minimal ledger edit per your instructions. Output only the JSON object.")
	return b.String()
}

// parseReplan extracts the JSON object and maps it onto a conductor.ReplanResult.
func parseReplan(s string) (conductor.ReplanResult, error) {
	obj := extractJSONObject(s)
	if obj == "" {
		return conductor.ReplanResult{}, nil // nothing usable → no-op re-plan
	}
	var raw struct {
		Add []struct {
			Description string `json:"description"`
			BlockedBy   []int  `json:"blocked_by"`
		} `json:"add"`
		Abandon []int  `json:"abandon"`
		Note    string `json:"note"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return conductor.ReplanResult{}, fmt.Errorf("parse replan output: %w", err)
	}
	res := conductor.ReplanResult{Abandon: raw.Abandon, Note: raw.Note}
	for _, a := range raw.Add {
		if strings.TrimSpace(a.Description) == "" {
			continue
		}
		res.Add = append(res.Add, conductor.Unit{Description: a.Description, BlockedBy: a.BlockedBy})
	}
	return res, nil
}

// extractJSONObject returns the substring from the first '{' to the last '}',
// tolerating surrounding prose or code fences. "" if no object is present.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return ""
	}
	return s[i : j+1]
}

// oneLineN collapses whitespace and truncates to n runes (cmd-side helper so
// the conductor package's oneLine stays internal).
func oneLineN(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n > 0 && len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
