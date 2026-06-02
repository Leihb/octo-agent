package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/conductor"
)

const judgeMaxTokens = 1024

// judgeSystem makes the model a strict reviewer. It deliberately defaults to
// FAIL when the evidence is thin: a lenient judge is barely better than the
// worker's own self-report, which the gate exists to distrust.
const judgeSystem = `You are a strict reviewer in an autonomous engineering effort. A worker just
finished a unit of work and reported what it did. Decide whether that work
actually satisfies the unit's goal.

You are given the overall goal, the unit's description, and the worker's
reported result. You do NOT see the files directly — judge from the report,
and be skeptical of vague or self-congratulatory claims that don't show
concrete evidence of the work.

Pass only if the reported result clearly and specifically demonstrates the
unit's goal is met. If it's vague, partial, off-target, or just asserts
success without substance, FAIL it and say what's missing — that reason is
fed back to the worker as its next instruction, so make it actionable.

Output ONE JSON object, no prose, no code fences:

  { "pass": <true|false>, "reason": "<one or two sentences>" }`

// judgeVerifier is the LLM-judge gate: a side-call asks a model whether a
// unit's reported output meets its goal. General-purpose — works for docs,
// design, research, and code alike (though for code an objective shell gate via
// --verify-cmd is strictly stronger). The whole-tree Global check is a no-op:
// per-unit judging already vetted each piece and there's no aggregate artifact
// for the judge to re-read.
type judgeVerifier struct{ a *agent.Agent }

func newJudgeVerifier(a *agent.Agent) *judgeVerifier { return &judgeVerifier{a: a} }

func (j *judgeVerifier) Verify(ctx context.Context, target conductor.VerifyTarget) (conductor.Verdict, error) {
	if target.Global {
		return conductor.Verdict{Green: true}, nil
	}
	if j.a == nil || j.a.Sender == nil {
		return conductor.Verdict{}, fmt.Errorf("conduct: judge has no model configured")
	}
	user := fmt.Sprintf("Overall goal:\n%s\n\nUnit:\n%s\n\nWorker's reported result:\n%s\n\n"+
		"Judge per your instructions. Output only the JSON object.",
		target.Goal, strings.TrimSpace(target.UnitDesc), strings.TrimSpace(target.Result))

	reply, err := j.a.Sender.SendMessages(ctx, j.a.Model, judgeSystem,
		[]agent.Message{agent.NewUserMessage(user)}, judgeMaxTokens)
	if err != nil {
		return conductor.Verdict{}, err
	}
	return parseJudge(reply.Content)
}

// parseJudge extracts {pass, reason}. A reply with no usable JSON is treated as
// a fail (the judge couldn't render a verdict — don't accept the unit on a
// malformed pass), with the raw reply as the reason for debugging.
func parseJudge(s string) (conductor.Verdict, error) {
	obj := extractJSONObject(s)
	if obj == "" {
		return conductor.Verdict{Green: false, Summary: "judge returned no verdict: " + oneLine(s)}, nil
	}
	var raw struct {
		Pass   bool   `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return conductor.Verdict{Green: false, Summary: "judge verdict unparseable: " + oneLine(s)}, nil
	}
	reason := strings.TrimSpace(raw.Reason)
	if reason == "" {
		if raw.Pass {
			reason = "meets the goal"
		} else {
			reason = "does not meet the goal"
		}
	}
	return conductor.Verdict{Green: raw.Pass, Summary: reason}, nil
}
