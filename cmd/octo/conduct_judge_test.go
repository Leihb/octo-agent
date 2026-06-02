package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/conductor"
)

func TestParseJudge(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantGreen bool
		wantInSum string
	}{
		{"pass", `{"pass": true, "reason": "all good"}`, true, "all good"},
		{"fail", `{"pass": false, "reason": "missing tests"}`, false, "missing tests"},
		{"pass with prose+fence", "Sure:\n```json\n{\"pass\": true, \"reason\": \"ok\"}\n```", true, "ok"},
		{"no json → fail", "looks fine to me", false, "no verdict"},
		{"unparseable → fail", `{"pass": tru}`, false, "unparseable"},
		{"pass empty reason", `{"pass": true}`, true, "meets the goal"},
		{"fail empty reason", `{"pass": false}`, false, "does not meet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := parseJudge(tt.in)
			if err != nil {
				t.Fatalf("parseJudge err = %v", err)
			}
			if v.Green != tt.wantGreen {
				t.Errorf("green = %v, want %v (in=%q)", v.Green, tt.wantGreen, tt.in)
			}
			if tt.wantInSum != "" && !strings.Contains(v.Summary, tt.wantInSum) {
				t.Errorf("summary = %q, want to contain %q", v.Summary, tt.wantInSum)
			}
		})
	}
}

// TestConductVerifierSelection checks the default-no-verify / --verify=judge /
// --verify-cmd=shell precedence (verify-cmd wins over verify).
func TestConductVerifierSelection(t *testing.T) {
	if _, ok := (conductFlags{}).verifier(nil).(conductor.NopVerifier); !ok {
		t.Errorf("default verifier should be NopVerifier")
	}
	if _, ok := (conductFlags{verify: true}).verifier(nil).(*judgeVerifier); !ok {
		t.Errorf("--verify should select the LLM judge")
	}
	if _, ok := (conductFlags{verifyCmd: "make ci"}).verifier(nil).(*conductor.CmdVerifier); !ok {
		t.Errorf("--verify-cmd should select the shell gate")
	}
	// verify-cmd wins when both are set.
	if _, ok := (conductFlags{verify: true, verifyCmd: "make ci"}).verifier(nil).(*conductor.CmdVerifier); !ok {
		t.Errorf("--verify-cmd should override --verify")
	}
}

// The judge's whole-tree Global check is a no-op pass (per-unit judging suffices).
func TestJudgeGlobalIsGreen(t *testing.T) {
	j := newJudgeVerifier(nil) // nil agent is fine; Global short-circuits before any call
	v, err := j.Verify(context.Background(), conductor.VerifyTarget{Global: true})
	if err != nil || !v.Green {
		t.Fatalf("global judge verdict = %+v err=%v, want green", v, err)
	}
}
