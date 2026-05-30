package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tasks"
)

func useTaskStore(t *testing.T) *tasks.Store {
	t.Helper()
	s := tasks.New()
	SetTaskStore(s)
	t.Cleanup(func() { SetTaskStore(nil) })
	return s
}

// ── task_create ──────────────────────────────────────────────────────────

func TestTaskCreateTool_Schema(t *testing.T) {
	def := TaskCreateTool{}.Definition()
	if def.Name != "task_create" {
		t.Errorf("Name = %q", def.Name)
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, want := range []string{"subject", "description", "active_form"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema missing property %q", want)
		}
	}
	required, _ := def.Parameters["required"].([]string)
	if !sliceContains(required, "subject") {
		t.Errorf("subject should be required, got %v", required)
	}
}

func TestTaskCreateTool_Execute(t *testing.T) {
	store := useTaskStore(t)
	out, err := TaskCreateTool{}.Execute(context.Background(), "task_create", map[string]any{
		"subject":     "Migrate auth middleware",
		"description": "Replace legacy session check",
		"active_form": "Migrating auth middleware",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Created task #1") || !strings.Contains(out.Text, "Migrate auth middleware") {
		t.Errorf("Execute = %q", out.Text)
	}
	got := store.List()
	if len(got) != 1 || got[0].Subject != "Migrate auth middleware" {
		t.Errorf("store state after Create = %+v", got)
	}
}

func TestTaskCreateTool_Execute_RequiresSubject(t *testing.T) {
	useTaskStore(t)
	if _, err := (TaskCreateTool{}).Execute(context.Background(), "task_create", map[string]any{
		"description": "Something",
	}); err == nil || !strings.Contains(err.Error(), "subject is required") {
		t.Errorf("missing subject should error, got %v", err)
	}
}

func TestTaskCreateTool_Execute_NoStore(t *testing.T) {
	SetTaskStore(nil)
	if _, err := (TaskCreateTool{}).Execute(context.Background(), "task_create", map[string]any{
		"subject": "x",
	}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("no store should error 'not configured', got %v", err)
	}
}

// ── task_update ──────────────────────────────────────────────────────────

func TestTaskUpdateTool_Schema(t *testing.T) {
	def := TaskUpdateTool{}.Definition()
	if def.Name != "task_update" {
		t.Errorf("Name = %q", def.Name)
	}
	required, _ := def.Parameters["required"].([]string)
	if !sliceContains(required, "task_id") {
		t.Errorf("task_id should be required, got %v", required)
	}
}

func TestTaskUpdateTool_Execute_StatusChange(t *testing.T) {
	store := useTaskStore(t)
	id, _ := store.Create("task", "", "")

	out, err := TaskUpdateTool{}.Execute(context.Background(), "task_update", map[string]any{
		"task_id": float64(id), // JSON shape
		"status":  "in_progress",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "in_progress") {
		t.Errorf("Execute = %q", out.Text)
	}
	got, _ := store.Get(id)
	if got.Status != tasks.InProgress {
		t.Errorf("status not updated: %q", got.Status)
	}
}

func TestTaskUpdateTool_Execute_PartialUpdate(t *testing.T) {
	store := useTaskStore(t)
	id, _ := store.Create("orig", "orig desc", "")

	_, err := TaskUpdateTool{}.Execute(context.Background(), "task_update", map[string]any{
		"task_id": float64(id),
		"subject": "new subject",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(id)
	if got.Subject != "new subject" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.Description != "orig desc" {
		t.Errorf("Description should be untouched: %q", got.Description)
	}
}

func TestTaskUpdateTool_Execute_RejectsNoFields(t *testing.T) {
	store := useTaskStore(t)
	id, _ := store.Create("task", "", "")
	if _, err := (TaskUpdateTool{}).Execute(context.Background(), "task_update", map[string]any{
		"task_id": float64(id),
	}); err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Errorf("expected 'nothing to update' error, got %v", err)
	}
}

func TestTaskUpdateTool_Execute_InvalidStatus(t *testing.T) {
	store := useTaskStore(t)
	id, _ := store.Create("task", "", "")
	_, err := TaskUpdateTool{}.Execute(context.Background(), "task_update", map[string]any{
		"task_id": float64(id),
		"status":  "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Errorf("invalid status should error, got %v", err)
	}
}

func TestTaskUpdateTool_Execute_UnknownTaskID(t *testing.T) {
	useTaskStore(t)
	_, err := TaskUpdateTool{}.Execute(context.Background(), "task_update", map[string]any{
		"task_id": float64(999),
		"status":  "completed",
	})
	if err == nil || !strings.Contains(err.Error(), "no task with id") {
		t.Errorf("unknown ID should error, got %v", err)
	}
}

func TestTaskUpdateTool_Execute_RequiresPositiveID(t *testing.T) {
	useTaskStore(t)
	for _, in := range []map[string]any{
		{},                       // missing
		{"task_id": float64(0)},  // zero
		{"task_id": float64(-3)}, // negative
	} {
		in["status"] = "completed"
		if _, err := (TaskUpdateTool{}).Execute(context.Background(), "task_update", in); err == nil {
			t.Errorf("input %+v should error", in)
		}
	}
}

// ── task_list ────────────────────────────────────────────────────────────

func TestTaskListTool_Execute_Empty(t *testing.T) {
	useTaskStore(t)
	out, err := TaskListTool{}.Execute(context.Background(), "task_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "No tasks yet." {
		t.Errorf("empty list = %q", out.Text)
	}
}

func TestTaskListTool_Execute_GroupsByStatus(t *testing.T) {
	store := useTaskStore(t)
	id1, _ := store.Create("first", "", "")
	id2, _ := store.Create("second", "", "")
	store.Create("third", "", "")

	inProg := tasks.InProgress
	done := tasks.Completed
	_, _ = store.Update(id1, tasks.UpdateField{Status: &inProg})
	_, _ = store.Update(id2, tasks.UpdateField{Status: &done})

	out, err := TaskListTool{}.Execute(context.Background(), "task_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"▶", "○", "✓", "first", "second", "third"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("output missing %q:\n%s", want, out.Text)
		}
	}
	// In-progress (id1) should appear before pending (id3) which should
	// appear before completed (id2) in the rendered text.
	inProgPos := strings.Index(out.Text, "first")
	pendPos := strings.Index(out.Text, "third")
	donePos := strings.Index(out.Text, "second")
	if !(inProgPos < pendPos && pendPos < donePos) {
		t.Errorf("status order wrong (in_prog<pending<done): %d<%d<%d", inProgPos, pendPos, donePos)
	}
}

func TestTaskListTool_Execute_UsesActiveFormForInProgress(t *testing.T) {
	store := useTaskStore(t)
	id, _ := store.Create("Migrate auth middleware", "", "Migrating auth middleware")
	inProg := tasks.InProgress
	_, _ = store.Update(id, tasks.UpdateField{Status: &inProg})

	out, _ := TaskListTool{}.Execute(context.Background(), "task_list", nil)
	if !strings.Contains(out.Text, "Migrating auth middleware") {
		t.Errorf("in-progress should use ActiveForm, got:\n%s", out.Text)
	}
}

// ── gating ───────────────────────────────────────────────────────────────

func TestDefaultTools_TaskToolsGatedOnStore(t *testing.T) {
	SetTaskStore(nil)
	t.Cleanup(func() { SetTaskStore(nil) })

	taskNames := func() []string {
		var got []string
		for _, d := range DefaultTools() {
			if strings.HasPrefix(d.Name, "task_") {
				got = append(got, d.Name)
			}
		}
		return got
	}

	if got := taskNames(); len(got) != 0 {
		t.Errorf("task_* tools should be absent without a store, got %v", got)
	}
	useTaskStore(t)
	got := taskNames()
	for _, want := range []string{"task_create", "task_update", "task_list"} {
		if !sliceContains(got, want) {
			t.Errorf("expected %q in DefaultTools, got %v", want, got)
		}
	}
}

func TestActiveTaskStore_Roundtrip(t *testing.T) {
	if ActiveTaskStore() != nil {
		// Defensive: tests in this package set/clear the store; another test
		// might have left it set. Reset.
		SetTaskStore(nil)
	}
	if ActiveTaskStore() != nil {
		t.Fatal("ActiveTaskStore should be nil when none is set")
	}
	s := tasks.New()
	SetTaskStore(s)
	defer SetTaskStore(nil)
	if got := ActiveTaskStore(); got == nil {
		t.Error("ActiveTaskStore should return the registered store")
	}
}

// ── formatter ────────────────────────────────────────────────────────────

func TestFormatTaskList_DescriptionIndented(t *testing.T) {
	id := 7
	out := FormatTaskList([]tasks.Task{{
		ID:          id,
		Subject:     "Refactor X",
		Description: "First line\nSecond line",
		Status:      tasks.Pending,
	}})
	if !strings.Contains(out, "Refactor X") || !strings.Contains(out, "#7") {
		t.Errorf("subject line missing:\n%s", out)
	}
	// Description lines should be indented further than the row.
	if !strings.Contains(out, "       First line") || !strings.Contains(out, "       Second line") {
		t.Errorf("description not indented:\n%s", out)
	}
}
