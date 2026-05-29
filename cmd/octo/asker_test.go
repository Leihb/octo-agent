package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tools"
)

// askInput wires a replAsker around the canned stdin string + a buffer for
// the rendered prompt. Returns the asker and the output buffer so tests
// can assert both the user answer and the displayed UI.
func askInput(input string) (*replAsker, *bytes.Buffer) {
	out := &bytes.Buffer{}
	reader := newScannerLineReader(strings.NewReader(input), out)
	view := newPlainView(reader, out, out, verbosityNormal, false)
	return newREPLAsker(view), out
}

func TestReplAsker_PrintsQuestionAndOptions(t *testing.T) {
	a, out := askInput("1\n")
	_, err := a.Ask(context.Background(), tools.AskRequest{
		Question: "Which library should we use?",
		Options:  []string{"date-fns", "Day.js", "Luxon"},
		Header:   "lib_choice",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"[ask_user_question · lib_choice]",
		"Which library should we use?",
		"1) date-fns",
		"2) Day.js",
		"3) Luxon",
		"4) Other (free text)", // tail is always added
		"Select [1-4]:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestReplAsker_SinglePickFirstOption(t *testing.T) {
	a, _ := askInput("1\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"alpha", "beta"},
	})
	if len(r.Choices) != 1 || r.Choices[0] != "alpha" {
		t.Errorf("single pick = %+v, want [alpha]", r.Choices)
	}
}

func TestReplAsker_MultiSelect(t *testing.T) {
	a, out := askInput("1,3\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question:    "?",
		Options:     []string{"alpha", "beta", "gamma"},
		MultiSelect: true,
	})
	if len(r.Choices) != 2 || r.Choices[0] != "alpha" || r.Choices[1] != "gamma" {
		t.Errorf("multi-pick = %+v, want [alpha gamma]", r.Choices)
	}
	if !strings.Contains(out.String(), "comma-separated") {
		t.Errorf("multi-select prompt should hint at comma-separated input")
	}
}

func TestReplAsker_SingleSelectIgnoresExtraPicks(t *testing.T) {
	// Even when the user types "1,2", single-select keeps only the first.
	a, _ := askInput("1,2\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"alpha", "beta"},
	})
	if len(r.Choices) != 1 || r.Choices[0] != "alpha" {
		t.Errorf("single-select must keep only the first pick, got %+v", r.Choices)
	}
}

func TestReplAsker_OtherFreeText(t *testing.T) {
	// "Other" is the (len(opts)+1)-th slot; after picking it, the asker
	// prompts again for the free-text follow-up.
	a, out := askInput("3\nKerberos with constrained delegation\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"OAuth", "API key"},
	})
	if r.Custom != "Kerberos with constrained delegation" {
		t.Errorf("custom = %q", r.Custom)
	}
	if len(r.Choices) != 0 {
		t.Errorf("custom answer should leave Choices empty, got %+v", r.Choices)
	}
	if !strings.Contains(out.String(), "Other (free text):") {
		t.Errorf("Other follow-up prompt missing in:\n%s", out)
	}
}

func TestReplAsker_OtherEmptyFreeText_Cancels(t *testing.T) {
	a, _ := askInput("3\n\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"a", "b"},
	})
	if !r.Cancelled {
		t.Errorf("empty Other text should cancel, got %+v", r)
	}
}

func TestReplAsker_EmptyInputCancels(t *testing.T) {
	a, _ := askInput("\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"a", "b"},
	})
	if !r.Cancelled {
		t.Errorf("empty selection should cancel, got %+v", r)
	}
}

func TestReplAsker_EOFCancels(t *testing.T) {
	// Empty reader → Scan returns false immediately → cancel.
	a, _ := askInput("")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"a", "b"},
	})
	if !r.Cancelled {
		t.Errorf("EOF should cancel, got %+v", r)
	}
}

func TestReplAsker_NonNumericCancels(t *testing.T) {
	// Garbage input → can't parse → cancel (with a friendly inline note).
	a, out := askInput("yes please\n")
	r, _ := a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"a", "b"},
	})
	if !r.Cancelled {
		t.Errorf("non-numeric input should cancel, got %+v", r)
	}
	if !strings.Contains(out.String(), "couldn't parse") {
		t.Errorf("expected user-facing parse note in:\n%s", out)
	}
}

func TestReplAsker_HeaderDefault(t *testing.T) {
	a, out := askInput("1\n")
	_, _ = a.Ask(context.Background(), tools.AskRequest{
		Question: "?",
		Options:  []string{"a", "b"},
		// no Header
	})
	if !strings.Contains(out.String(), "[ask_user_question · question]") {
		t.Errorf("missing header should default to 'question':\n%s", out)
	}
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		max     int
		multi   bool
		want    []int
		wantErr bool
	}{
		{"single", "2", 4, false, []int{2}, false},
		{"multi keeps all", "1,3", 4, true, []int{1, 3}, false},
		{"single drops extras", "1,3", 4, false, []int{1}, false},
		{"whitespace tolerated", " 2 , 4 ", 4, true, []int{2, 4}, false},
		{"out of range errors", "9", 4, false, nil, true},
		{"non-numeric errors", "abc", 4, false, nil, true},
		{"empty returns empty", "", 4, false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSelection(tc.in, tc.max, tc.multi)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !sliceEqualsInt(got, tc.want) {
				t.Errorf("parseSelection = %v, want %v", got, tc.want)
			}
		})
	}
}

func sliceEqualsInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
