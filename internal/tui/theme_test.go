package tui

import (
	"strings"
	"testing"
)

func TestPanel_BordersTitleAndBody(t *testing.T) {
	out := Panel("queue (2)", "1. lint\n2. test")
	for _, want := range []string{"╭", "╯", "queue (2)", "1. lint", "2. test"} {
		if !strings.Contains(out, want) {
			t.Errorf("Panel missing %q in:\n%s", want, out)
		}
	}
}

func TestPanel_TitleOnly(t *testing.T) {
	out := Panel("background (1)", "")
	if !strings.Contains(out, "background (1)") || !strings.Contains(out, "╭") {
		t.Errorf("title-only panel should still draw a bordered box; got:\n%s", out)
	}
}

func TestBox_BordersContent(t *testing.T) {
	out := Box("⚠ permission\nterminal wants to run")
	for _, want := range []string{"╭", "│", "⚠ permission", "terminal wants to run"} {
		if !strings.Contains(out, want) {
			t.Errorf("Box missing %q in:\n%s", want, out)
		}
	}
}
