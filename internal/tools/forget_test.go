package tools

import (
	"context"
	"strings"
	"testing"
)

func TestForgetTool_Execute(t *testing.T) {
	store := useMemory(t)
	// Seed a real on-disk entry via remember.
	if _, err := (RememberTool{}).Execute(context.Background(), "remember", map[string]any{
		"content": "Run tests before committing.", "type": "feedback",
	}); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.List()
	if len(entries) != 1 {
		t.Fatalf("setup: want 1 entry, got %d", len(entries))
	}
	name := entries[0].Name

	out, err := ForgetTool{}.Execute(context.Background(), "forget", map[string]any{"name": name})
	if err != nil {
		t.Fatalf("forget error: %v", err)
	}
	if !strings.Contains(out.Text, name) {
		t.Errorf("confirmation should name the forgotten entry: %q", out.Text)
	}
	if remaining, _ := store.List(); len(remaining) != 0 {
		t.Errorf("entry should be gone; %d remain", len(remaining))
	}
}

func TestForgetTool_Errors(t *testing.T) {
	useMemory(t)
	if _, err := (ForgetTool{}).Execute(context.Background(), "forget", map[string]any{}); err == nil {
		t.Error("expected error for missing name")
	}
	if _, err := (ForgetTool{}).Execute(context.Background(), "forget", map[string]any{"name": "ghost"}); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Errorf("expected 'no entry' error for unknown name, got %v", err)
	}

	SetMemoryStore(nil)
	if _, err := (ForgetTool{}).Execute(context.Background(), "forget", map[string]any{"name": "x"}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("expected 'disabled' error, got %v", err)
	}
}

func TestDefaultTools_ForgetGatedOnMemory(t *testing.T) {
	hasForget := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "forget" {
				return true
			}
		}
		return false
	}

	SetMemoryStore(nil)
	if hasForget() {
		t.Error("forget should be absent when memory is disabled")
	}

	useMemory(t)
	if !hasForget() {
		t.Error("forget should be advertised when memory is enabled")
	}
}
