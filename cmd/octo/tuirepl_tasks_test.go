package main

import (
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tasks"
)

func TestRenderTaskLines_HidesWhenNothingOutstanding(t *testing.T) {
	if got := renderTaskLines(nil, 80); got != "" {
		t.Errorf("empty list: want \"\", got %q", got)
	}
	allDone := []tasks.Task{
		{ID: 1, Subject: "a", Status: tasks.Completed},
		{ID: 2, Subject: "b", Status: tasks.Completed},
	}
	if got := renderTaskLines(allDone, 80); got != "" {
		t.Errorf("all-completed list should be hidden, got %q", got)
	}
}

func TestRenderTaskLines_Layout(t *testing.T) {
	items := []tasks.Task{
		{ID: 1, Subject: "Migrate auth", ActiveForm: "Migrating auth", Status: tasks.InProgress},
		{ID: 2, Subject: "Add test", Status: tasks.Pending},
		{ID: 3, Subject: "Update docs", Status: tasks.Completed},
	}
	out := renderTaskLines(items, 80)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 rows, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "└") {
		t.Errorf("first row should carry the tree elbow, got %q", lines[0])
	}
	// In-progress uses the present-continuous ActiveForm, not the Subject.
	if !strings.Contains(lines[0], "Migrating auth") {
		t.Errorf("in-progress row should use ActiveForm, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "□") {
		t.Errorf("in-progress row should use the box glyph, got %q", lines[0])
	}
	if !strings.Contains(lines[2], "✓") || !strings.Contains(lines[2], "Update docs") {
		t.Errorf("completed row should use the check glyph + subject, got %q", lines[2])
	}
}

func TestRenderTaskLines_CollapsesLongList(t *testing.T) {
	var items []tasks.Task
	for i := 1; i <= 12; i++ {
		items = append(items, tasks.Task{ID: i, Subject: "task", Status: tasks.Pending})
	}
	out := renderTaskLines(items, 80)
	lines := strings.Split(out, "\n")
	if len(lines) != maxTaskRows {
		t.Fatalf("want %d rows (capped), got %d:\n%s", maxTaskRows, len(lines), out)
	}
	last := lines[len(lines)-1]
	// 12 total, maxTaskRows-1 = 7 shown, so 5 collapsed.
	if !strings.Contains(last, "5 more") {
		t.Errorf("last row should report the collapsed count, got %q", last)
	}
}

func TestRenderTaskRow_TruncatesToWidth(t *testing.T) {
	long := tasks.Task{ID: 1, Subject: strings.Repeat("x", 200), Status: tasks.Pending}
	row := renderTaskRow(long, 40)
	if !strings.Contains(row, "…") {
		t.Errorf("over-wide row should be truncated with an ellipsis, got %q", row)
	}
}
