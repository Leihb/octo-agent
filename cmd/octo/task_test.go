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
	// Exit code 2: unknown-ID is a user input error, not a runtime fault.
	// (Was 1 before UX-2; the resolver classifies "no match" as 2.)
	if code != 2 {
		t.Errorf("unknown id exit = %d, want 2; stderr=%q", code, errBuf.String())
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

// ── list / status / show / resume / cancel ──────────────────────────────

// seedTask creates one task in the fake-home store and returns it. Helper
// for the inspection-command tests.
func seedTask(t *testing.T) *taskgraph.Task {
	t.Helper()
	store, err := taskgraph.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	tk, err := store.Create("test goal", []taskgraph.Subtask{
		{ID: 1, Description: "do A", Status: taskgraph.SubtaskPending},
		{ID: 2, Description: "do B", BlockedBy: []int{1}, Status: taskgraph.SubtaskPending},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestRunTaskList_EmptyShowsNotice(t *testing.T) {
	withFakeHome(t)
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"list"}, nil, &out, &errBuf); code != 0 {
		t.Errorf("empty list exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "No tasks yet") {
		t.Errorf("empty list should print notice:\n%s", out.String())
	}
}

func TestRunTaskList_LsAlias(t *testing.T) {
	// `ls` is the alias for `list` (vim users / unix muscle memory).
	withFakeHome(t)
	var out bytes.Buffer
	if code := runTask([]string{"ls"}, nil, &out, &out); code != 0 {
		t.Errorf("ls alias exit = %d, want 0", code)
	}
}

func TestRunTaskList_ShowsAllTasks(t *testing.T) {
	withFakeHome(t)
	t1 := seedTask(t)
	t2 := seedTask(t)

	var out bytes.Buffer
	if code := runTask([]string{"list"}, nil, &out, &out); code != 0 {
		t.Fatalf("list exit = %d", code)
	}
	got := out.String()
	// Ordering is descending-by-ID (proved at the store layer in
	// internal/taskgraph; here we just confirm the CLI passes everything
	// through with status + goal columns. Display switched to 8-char short
	// IDs in UX-2 so we compare against the short form, not the full ID.)
	for _, want := range []string{t1.ShortID(), t2.ShortID(), "pending", "test goal"} {
		if !strings.Contains(got, want) {
			t.Errorf("list output missing %q:\n%s", want, got)
		}
	}
}

func TestRunTaskStatus_RequiresID(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"status"}, nil, &out, &errBuf); code != 2 {
		t.Errorf("missing id exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "octo task status <id>") {
		t.Errorf("missing-id error should print usage, got:\n%s", errBuf.String())
	}
}

func TestRunTaskStatus_UnknownIDErrors(t *testing.T) {
	withFakeHome(t)
	var out, errBuf bytes.Buffer
	// Exit code is 2: a user-typed ID that doesn't match anything is a
	// command-line error (like missing required arg), not a runtime error.
	if code := runTask([]string{"status", "missing"}, nil, &out, &errBuf); code != 2 {
		t.Errorf("unknown id exit = %d, want 2; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no task matches") {
		t.Errorf("expected 'no task matches' error, got:\n%s", errBuf.String())
	}
}

func TestRunTaskStatus_RendersDAG(t *testing.T) {
	withFakeHome(t)
	tk := seedTask(t)

	var out bytes.Buffer
	if code := runTask([]string{"status", tk.ID}, nil, &out, &out); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	for _, want := range []string{"Task " + tk.ID, "Goal: test goal", "○ #1", "○ #2", "blocked_by: #1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunTaskShow_RequiresIDAndSubID(t *testing.T) {
	withFakeHome(t)
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"show"}, nil, &out, &errBuf); code != 2 {
		t.Error("missing id should exit 2")
	}
	if code := runTask([]string{"show", "abc"}, nil, &out, &errBuf); code != 2 {
		t.Error("missing subtask-id should exit 2")
	}
}

func TestRunTaskShow_RejectsNonIntSubID(t *testing.T) {
	withFakeHome(t)
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"show", "abc", "two"}, nil, &out, &errBuf); code != 2 {
		t.Errorf("non-int subtask-id exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "invalid subtask-id") {
		t.Errorf("expected 'invalid subtask-id' note, got:\n%s", errBuf.String())
	}
}

func TestRunTaskShow_UnknownSubtaskErrors(t *testing.T) {
	withFakeHome(t)
	tk := seedTask(t)
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"show", tk.ID, "99"}, nil, &out, &errBuf); code != 1 {
		t.Errorf("unknown subtask-id exit = %d, want 1", code)
	}
}

func TestRunTaskShow_RendersFullSubtask(t *testing.T) {
	withFakeHome(t)
	tk := seedTask(t)

	// Mark subtask 1 Done with a result so the formatter has content
	// to render.
	store, _ := taskgraph.NewStore()
	_, _ = store.Update(tk.ID, func(t *taskgraph.Task) error {
		s := t.Find(1)
		s.Status = taskgraph.SubtaskDone
		s.Result = "found the cache layer"
		return nil
	})

	var out bytes.Buffer
	if code := runTask([]string{"show", tk.ID, "1"}, nil, &out, &out); code != 0 {
		t.Fatalf("show exit = %d", code)
	}
	for _, want := range []string{"subtask #1", "done", "do A", "found the cache layer"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunTaskCancel_RequiresID(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"cancel"}, nil, &out, &errBuf); code != 2 {
		t.Errorf("missing id exit = %d, want 2", code)
	}
}

func TestRunTaskCancel_MarksTaskCancelled(t *testing.T) {
	withFakeHome(t)
	tk := seedTask(t)
	var out bytes.Buffer
	if code := runTask([]string{"cancel", tk.ID}, nil, &out, &out); code != 0 {
		t.Fatalf("cancel exit = %d", code)
	}
	store, _ := taskgraph.NewStore()
	got, _ := store.Get(tk.ID)
	if got.Status != taskgraph.TaskCancelled {
		t.Errorf("task status after cancel = %q, want %q", got.Status, taskgraph.TaskCancelled)
	}
}

func TestRunTaskCancel_RefusesDoneTask(t *testing.T) {
	withFakeHome(t)
	tk := seedTask(t)
	store, _ := taskgraph.NewStore()
	_, _ = store.Update(tk.ID, func(t *taskgraph.Task) error {
		t.Status = taskgraph.TaskDone
		return nil
	})
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"cancel", tk.ID}, nil, &out, &errBuf); code != 1 {
		t.Errorf("cancel on done exit = %d, want 1", code)
	}
}

func TestRunTaskResume_RequiresID(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runTask([]string{"resume"}, nil, &out, &errBuf); code != 2 {
		t.Errorf("missing id exit = %d, want 2", code)
	}
}

func TestRunTaskResume_ResetsFailedAndSkippedToPending(t *testing.T) {
	// Resume mutates state but then tries to actually re-run via the LLM —
	// we don't want to call the real provider in unit tests. So we set up
	// the task, run resume (which will fail at the provider build step
	// because there's no API key), and verify the state reset still happened
	// before the failure.
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	tk := seedTask(t)

	store, _ := taskgraph.NewStore()
	_, _ = store.Update(tk.ID, func(t *taskgraph.Task) error {
		t.Status = taskgraph.TaskFailed
		t.Subtasks[0].Status = taskgraph.SubtaskFailed
		t.Subtasks[0].Error = "old error"
		t.Subtasks[1].Status = taskgraph.SubtaskSkipped
		return nil
	})

	var out, errBuf bytes.Buffer
	// We expect exit 1 (provider build will fail), but the state mutation
	// happens FIRST so it must persist.
	runTask([]string{"resume", tk.ID}, nil, &out, &errBuf)

	got, _ := store.Get(tk.ID)
	if got.Status != taskgraph.TaskPending {
		t.Errorf("task status after resume = %q, want %q", got.Status, taskgraph.TaskPending)
	}
	if got.Subtasks[0].Status != taskgraph.SubtaskPending {
		t.Errorf("failed subtask not reset: %q", got.Subtasks[0].Status)
	}
	if got.Subtasks[0].Error != "" {
		t.Errorf("failed subtask error not cleared: %q", got.Subtasks[0].Error)
	}
	if got.Subtasks[1].Status != taskgraph.SubtaskPending {
		t.Errorf("skipped subtask not reset: %q", got.Subtasks[1].Status)
	}
}

func TestRunTaskResume_RejectsDoneTask(t *testing.T) {
	withFakeHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	tk := seedTask(t)

	store, _ := taskgraph.NewStore()
	_, _ = store.Update(tk.ID, func(t *taskgraph.Task) error {
		t.Status = taskgraph.TaskDone
		return nil
	})

	var out, errBuf bytes.Buffer
	if code := runTask([]string{"resume", tk.ID}, nil, &out, &errBuf); code != 1 {
		t.Errorf("resume on done task exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
}

func TestSubtaskGlyph(t *testing.T) {
	cases := map[taskgraph.SubtaskStatus]string{
		taskgraph.SubtaskRunning:         "▶",
		taskgraph.SubtaskPending:         "○",
		taskgraph.SubtaskDone:            "✓",
		taskgraph.SubtaskFailed:          "✗",
		taskgraph.SubtaskSkipped:         "⊘",
		taskgraph.SubtaskStatus("bogus"): "·",
	}
	for in, want := range cases {
		if got := subtaskGlyph(in); got != want {
			t.Errorf("subtaskGlyph(%q) = %q, want %q", in, got, want)
		}
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
