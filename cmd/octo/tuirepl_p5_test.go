package main

import (
	"strings"
	"testing"
)

func TestTUI_QueuePanelIsBordered(t *testing.T) {
	m := newTestModel()
	m.queue = []pendingItem{{text: "run lint"}, {text: "run tests"}}
	out := m.View()
	if !strings.Contains(out, "queue (2)") {
		t.Errorf("view should show the queue title; got:\n%s", out)
	}
	if !strings.ContainsAny(out, "╭╮╰╯") {
		t.Errorf("queue should render as a bordered panel; got:\n%s", out)
	}
	if !strings.Contains(out, "run lint") || !strings.Contains(out, "run tests") {
		t.Errorf("queue items should be listed; got:\n%s", out)
	}
}

func TestTUI_PermissionModalIsBordered(t *testing.T) {
	m := newTestModel()
	m.modal = &modalState{prompt: UserPrompt{
		Kind:      KindPermission,
		ToolName:  "terminal",
		ToolInput: map[string]any{"command": "rm -rf /"},
	}}
	out := m.View()
	if !strings.ContainsAny(out, "╭╮╰╯") {
		t.Errorf("permission modal should be a bordered box; got:\n%s", out)
	}
	if !strings.Contains(out, "permission") {
		t.Errorf("modal should show its heading; got:\n%s", out)
	}
}
