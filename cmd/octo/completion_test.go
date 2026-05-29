package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

// containsAll is a tiny assertion helper for "every want is present in got".
func containsAll(got, want []string) bool {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestCompletionCandidates_TopLevel(t *testing.T) {
	cases := [][]string{
		{},             // no args
		{"octo"},       // just the program
		{"octo", ""},   // typing the subcommand (empty partial)
		{"octo", "ch"}, // typing the subcommand (partial)
	}
	for _, words := range cases {
		got := completionCandidates(words)
		if !containsAll(got, []string{"chat", "goal", "memory", "init", "help", "completion"}) {
			t.Errorf("words=%v missing top-level commands; got %v", words, got)
		}
	}
}

func TestCompletionCandidates_ChatFlags(t *testing.T) {
	got := completionCandidates([]string{"octo", "chat", ""})
	for _, want := range []string{"-c", "--continue", "--tools", "--provider", "--quiet", "--verbose"} {
		if !sliceContains(got, want) {
			t.Errorf("chat flag completion missing %q; got %v", want, got)
		}
	}
}

func TestCompletionCandidates_ProviderValueAfterFlag(t *testing.T) {
	got := completionCandidates([]string{"octo", "chat", "--provider", ""})
	if !sliceEq(got, []string{"anthropic", "openai"}) {
		t.Errorf("--provider value completion = %v, want [anthropic openai]", got)
	}
}

func TestCompletionCandidates_PermissionModeAfterFlag(t *testing.T) {
	got := completionCandidates([]string{"octo", "chat", "--permission-mode", ""})
	if !sliceEq(got, []string{"interactive", "strict"}) {
		t.Errorf("--permission-mode value completion = %v", got)
	}
}

func TestCompletionCandidates_SessionIDsAfterDashC(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	// Seed two sessions so the candidate list is meaningful.
	s1 := agent.NewSession("test-model", "")
	if err := s1.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	s2 := agent.NewSession("test-model", "")
	if err := s2.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := completionCandidates([]string{"octo", "chat", "-c", ""})
	// "last" is always first, then short + full for each session.
	if len(got) < 1 || got[0] != "last" {
		t.Errorf("expected 'last' as first candidate; got %v", got)
	}
	for _, want := range []string{s1.ShortID(), s1.ID, s2.ShortID(), s2.ID} {
		if !sliceContains(got, want) {
			t.Errorf("expected %q in candidates; got %v", want, got)
		}
	}
}

func TestCompletionCandidates_TaskSubcommands(t *testing.T) {
	got := completionCandidates([]string{"octo", "goal", ""})
	for _, want := range []string{"start", "run", "list", "status", "show", "resume", "cancel"} {
		if !sliceContains(got, want) {
			t.Errorf("task subcommand completion missing %q; got %v", want, got)
		}
	}
}

func TestCompletionCandidates_TaskIDsAfterVerbs(t *testing.T) {
	withFakeHome(t)
	// No tasks seeded — at minimum we expect "last" (the empty store is fine).
	for _, verb := range []string{"status", "show", "resume", "cancel", "run"} {
		got := completionCandidates([]string{"octo", "goal", verb, ""})
		if len(got) < 1 || got[0] != "last" {
			t.Errorf("task %s ID completion missing 'last'; got %v", verb, got)
		}
	}
}

func TestCompletionCandidates_HelpTargets(t *testing.T) {
	got := completionCandidates([]string{"octo", "help", ""})
	want := []string{"chat", "config", "goal", "memory", "init", "completion", "mcp"}
	if !sliceEq(got, want) {
		t.Errorf("help target completion = %v, want %v", got, want)
	}
}

func TestCompletionCandidates_CompletionShells(t *testing.T) {
	got := completionCandidates([]string{"octo", "completion", ""})
	if !sliceEq(got, []string{"bash", "zsh", "fish"}) {
		t.Errorf("completion shell list = %v", got)
	}
}

func TestRunCompletion_BashScript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCompletion([]string{"bash"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"_octo_completions()",
		"complete -F _octo_completions octo",
		"octo __complete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bash script missing %q:\n%s", want, out)
		}
	}
}

func TestRunCompletion_ZshScript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCompletion([]string{"zsh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"#compdef octo", "compdef _octo octo", "octo __complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("zsh script missing %q:\n%s", want, out)
		}
	}
}

func TestRunCompletion_FishScript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCompletion([]string{"fish"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"complete -c octo", "octo __complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("fish script missing %q:\n%s", want, out)
		}
	}
}

func TestRunCompletion_UnknownShell(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCompletion([]string{"powershell"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unsupported shell") {
		t.Errorf("stderr should explain; got: %q", stderr.String())
	}
}

func TestRunCompletion_NoShellArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCompletion(nil, &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr should print usage; got: %q", stderr.String())
	}
}

func TestRunComplete_PrintsCandidatesOnePerLine(t *testing.T) {
	var out bytes.Buffer
	if code := runComplete([]string{"octo", "chat", "--provider", ""}, &out); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if !sliceEq(lines, []string{"anthropic", "openai"}) {
		t.Errorf("output = %v", lines)
	}
}

func TestRun_CompletionViaMain(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"completion", "bash"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "_octo_completions") {
		t.Errorf("main() routing for completion broken; got:\n%s", stdout.String())
	}
}

func TestRun_CompleteViaMain(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__complete", "octo", "help", ""}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"chat", "goal", "memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// sliceContains / sliceEq are tiny test helpers to keep assertions tight.
func sliceContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func sliceEq(a, b []string) bool {
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
