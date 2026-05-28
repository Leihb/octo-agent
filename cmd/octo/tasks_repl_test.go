package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
)

func TestPrintTasks_Disabled(t *testing.T) {
	tools.SetTaskStore(nil)
	t.Cleanup(func() { tools.SetTaskStore(nil) })

	var buf bytes.Buffer
	printTasks(&buf)
	if !strings.Contains(buf.String(), "disabled") {
		t.Errorf("expected 'disabled' notice when no store, got:\n%s", buf.String())
	}
}

func TestPrintTasks_Empty(t *testing.T) {
	tools.SetTaskStore(tasks.New())
	t.Cleanup(func() { tools.SetTaskStore(nil) })

	var buf bytes.Buffer
	printTasks(&buf)
	if !strings.Contains(buf.String(), "No tasks yet") {
		t.Errorf("expected empty-list message, got:\n%s", buf.String())
	}
}

func TestPrintTasks_RendersList(t *testing.T) {
	store := tasks.New()
	id, _ := store.Create("Refactor cache", "", "")
	inProg := tasks.InProgress
	_, _ = store.Update(id, tasks.UpdateField{Status: &inProg})
	store.Create("Add tests", "", "")

	tools.SetTaskStore(store)
	t.Cleanup(func() { tools.SetTaskStore(nil) })

	var buf bytes.Buffer
	printTasks(&buf)
	out := buf.String()
	for _, want := range []string{"Refactor cache", "Add tests", "▶", "○"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestREPL_TasksCommand(t *testing.T) {
	cfg, stdout, _, stub := makeREPLFixture(t, "/tasks\n/exit\n")
	store := tasks.New()
	store.Create("Test task", "", "")
	tools.SetTaskStore(store)
	t.Cleanup(func() { tools.SetTaskStore(nil) })

	runREPL(cfg)
	if stub.called != 0 {
		t.Errorf("/tasks should not call the sender, got %d", stub.called)
	}
	if !strings.Contains(stdout.String(), "Test task") {
		t.Errorf("/tasks output missing the task:\n%s", stdout.String())
	}
}
