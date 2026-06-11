package main

import (
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

func TestSessionSelectItems(t *testing.T) {
	s := agent.NewSession("test-model", "")
	s.Messages = []agent.Message{
		{Role: agent.RoleUser, Content: "fix the bug"},
		{Role: agent.RoleAssistant, Content: "done"},
	}

	items := sessionSelectItems([]*agent.Session{s})
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	it := items[0]
	if it.value != s.ID {
		t.Errorf("value = %q, want the full session ID %q", it.value, s.ID)
	}
	if !strings.Contains(it.label, s.ShortID()) {
		t.Errorf("label should carry the short ID; got %q", it.label)
	}
	if !strings.Contains(it.desc, "test-model") {
		t.Errorf("desc should carry the model; got %q", it.desc)
	}
	if !strings.Contains(it.desc, "1 turn") {
		t.Errorf("desc should carry the turn count; got %q", it.desc)
	}
}
