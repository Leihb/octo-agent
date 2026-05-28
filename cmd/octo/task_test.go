package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Leihb/octo-agent/internal/taskgraph"
)

// withFakeHome redirects $HOME (and Windows USERPROFILE) to a fresh
// tempdir so `octo task` writes to a sandbox, never the developer's
// ~/.octo/tasks. Returns the tempdir for assertions.
func withFakeHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	return tmp
}

func TestRunTask_NoArgsShowsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runTask(nil, nil, &out, &errBuf)
	if code != 2 {
		t.Errorf("no args exit = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "octo task") {
		t.Errorf("usage should be printed:\n%s", out.String())
	}
}

func TestRunTask_UnknownSubcommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runTask([]string{"bogus"}, nil, &out, &errBuf)
	if code != 2 {
		t.Errorf("unknown subcommand exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' notice, got:\n%s", errBuf.String())
	}
}

func TestRunTask_HelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		var out, errBuf bytes.Buffer
		if code := runTask([]string{arg}, nil, &out, &errBuf); code != 0 {
			t.Errorf("%s exit = %d, want 0", arg, code)
		}
	}
}

func TestRunTaskStart_RequiresGoal(t *testing.T) {
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var out, errBuf bytes.Buffer
	code := runTask([]string{"start"}, nil, &out, &errBuf)
	if code != 2 {
		t.Errorf("missing goal exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "goal is required") {
		t.Errorf("expected 'goal is required' note, got:\n%s", errBuf.String())
	}
}

func TestRunTaskStart_MissingAPIKey(t *testing.T) {
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	var out, errBuf bytes.Buffer
	code := runTask([]string{"start", "ship the daemon"}, nil, &out, &errBuf)
	if code != 1 {
		t.Errorf("missing key exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("stderr should name the missing env var: %q", errBuf.String())
	}
}

// printPlannedDAG is a pure formatter — exercise it directly so we don't
// have to stand up the whole provider chain just to verify rendering.
func TestPrintPlannedDAG_ShowsGoalAndSubtasks(t *testing.T) {
	tk := &taskgraph.Task{
		Goal: "Ship the daemon",
		Subtasks: []taskgraph.Subtask{
			{ID: 1, Description: "Investigate sessions mtime watching"},
			{ID: 2, Description: "Sketch daemon lifecycle", BlockedBy: []int{1}},
			{ID: 3, Description: "Write the unit file", BlockedBy: []int{2}},
		},
	}
	var out bytes.Buffer
	printPlannedDAG(&out, tk)
	for _, want := range []string{
		"Goal: Ship the daemon",
		"#1",
		"#2",
		"#3",
		"depends on: #1",
		"depends on: #2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPrintPlannedDAG_OmitsBlockedByLineWhenEmpty(t *testing.T) {
	tk := &taskgraph.Task{
		Goal: "Solo task",
		Subtasks: []taskgraph.Subtask{
			{ID: 1, Description: "Do the thing"},
		},
	}
	var out bytes.Buffer
	printPlannedDAG(&out, tk)
	if strings.Contains(out.String(), "depends on") {
		t.Errorf("subtask with no deps should not print 'depends on':\n%s", out.String())
	}
}

func TestOneLine_CollapsesAndTruncates(t *testing.T) {
	in := "first\nsecond\n  third"
	if got := oneLine(in); got != "first second third" {
		t.Errorf("oneLine = %q", got)
	}
	long := strings.Repeat("x", 200)
	out := oneLine(long)
	// 77 ASCII x's + ellipsis (1 rune, 3 bytes in UTF-8) → 78 runes total.
	if runes := utf8.RuneCountInString(out); runes != 78 {
		t.Errorf("oneLine truncation should cap at 78 runes (77 + ellipsis), got %d", runes)
	}
	if !strings.HasSuffix(out, "…") {
		t.Errorf("truncated output should end with ellipsis, got %q", out)
	}
}

func TestJoinInts(t *testing.T) {
	cases := map[string][]int{
		"":           nil,
		"#1":         {1},
		"#1, #3":     {1, 3},
		"#1, #2, #3": {1, 2, 3},
	}
	for want, in := range cases {
		if got := joinInts(in); got != want {
			t.Errorf("joinInts(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestRunTaskRun_RequiresTaskID(t *testing.T) {
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var out, errBuf bytes.Buffer
	code := runTask([]string{"run"}, nil, &out, &errBuf)
	if code != 2 {
		t.Errorf("missing id exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "task id is required") {
		t.Errorf("expected 'task id is required' note, got:\n%s", errBuf.String())
	}
}

func TestRunTaskRun_UnknownTaskID(t *testing.T) {
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var out, errBuf bytes.Buffer
	code := runTask([]string{"run", "20990101-000000-deadbeef"}, nil, &out, &errBuf)
	if code != 1 {
		t.Errorf("unknown id exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "octo task run") {
		t.Errorf("error should be tagged with 'octo task run':\n%s", errBuf.String())
	}
}

func TestRunTaskRun_MissingAPIKey(t *testing.T) {
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	var out, errBuf bytes.Buffer
	code := runTask([]string{"run", "any-id"}, nil, &out, &errBuf)
	if code != 1 {
		t.Errorf("missing key exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("stderr should name the missing env var: %q", errBuf.String())
	}
}

// Smoke test that the planner-to-store conversion preserves IDs and the
// taskgraph layer accepts what the planner emits. Doesn't hit the LLM —
// it constructs the equivalent subtasks directly and checks they round-
// trip through Store.Create.
func TestTaskGraph_AcceptsPlannerOutput(t *testing.T) {
	tmp := t.TempDir()
	store := taskgraph.NewStoreAt(tmp)

	subs := []taskgraph.Subtask{
		{ID: 1, Description: "A", Status: taskgraph.SubtaskPending},
		{ID: 2, Description: "B", BlockedBy: []int{1}, Status: taskgraph.SubtaskPending},
	}
	tk, err := store.Create("test goal", subs)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID == "" {
		t.Error("Create should assign an ID")
	}
	if got := len(tk.Subtasks); got != 2 {
		t.Errorf("Subtasks len = %d, want 2", got)
	}
	if _, err := store.Get(tk.ID); err != nil {
		t.Errorf("created task should be readable, got %v", err)
	}
	if _, err := store.Get(filepath.Base("nonexistent")); err == nil {
		t.Error("missing task should error")
	}
}
