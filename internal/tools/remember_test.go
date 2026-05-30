package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/memory"
)

func useMemory(t *testing.T) *memory.Store {
	t.Helper()
	s := memory.NewStoreAt(t.TempDir())
	SetMemoryStore(s)
	t.Cleanup(func() { SetMemoryStore(nil) })
	return s
}

func TestRememberTool_Execute(t *testing.T) {
	store := useMemory(t)

	out, err := RememberTool{}.Execute(context.Background(), "remember", map[string]any{
		"content": "Run tests before committing.",
		"type":    "feedback",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Text, "feedback") {
		t.Errorf("confirmation should report the type: %q", out.Text)
	}

	entries, _ := store.List()
	if len(entries) != 1 {
		t.Fatalf("expected 1 saved entry, got %d", len(entries))
	}
	if entries[0].Type != memory.TypeFeedback {
		t.Errorf("Type = %q", entries[0].Type)
	}
	if entries[0].Description != "Run tests before committing." {
		t.Errorf("Description (from content first line) = %q", entries[0].Description)
	}
}

func TestRememberTool_Errors(t *testing.T) {
	// Missing content.
	useMemory(t)
	if _, err := (RememberTool{}).Execute(context.Background(), "remember", map[string]any{}); err == nil {
		t.Error("expected error for missing content")
	}

	// Memory disabled.
	SetMemoryStore(nil)
	if _, err := (RememberTool{}).Execute(context.Background(), "remember", map[string]any{"content": "x"}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("expected 'disabled' error, got %v", err)
	}
}

func TestDefaultTools_RememberGatedOnMemory(t *testing.T) {
	SetMemoryStore(nil)
	t.Cleanup(func() { SetMemoryStore(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "remember" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("remember tool should be absent when memory is disabled")
	}
	useMemory(t)
	if !has() {
		t.Error("remember tool should be present when memory is enabled")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Run tests before committing": "run-tests-before-committing",
		"  I prefer Go!!!  ":          "i-prefer-go",
		"用户偏好 Go":                     "go", // non-ascii collapses to dashes, trimmed
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
