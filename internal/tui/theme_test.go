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

func TestBgWash_LightDark(t *testing.T) {
	if bgWash('+', true) != bgAddedDark || bgWash('-', true) != bgRemovedDark {
		t.Error("dark background should use the deep washes")
	}
	if bgWash('+', false) != bgAddedLight || bgWash('-', false) != bgRemovedLight {
		t.Error("light background should use the pale washes")
	}
}

func TestChromaStyle_LightDark(t *testing.T) {
	if chromaStyle(true) != "github-dark" || chromaStyle(false) != "github" {
		t.Errorf("chromaStyle: dark=%q light=%q", chromaStyle(true), chromaStyle(false))
	}
}

func TestBanner_ContainsTitleAndInfo(t *testing.T) {
	out := Banner("v1.0", "claude", "~/proj", 40)
	for _, want := range []string{"◆ octo", "claude", "~/proj", "──"} {
		if !strings.Contains(out, want) {
			t.Errorf("Banner missing %q in:\n%s", want, out)
		}
	}
}

// Key hints moved from the status bar into the startup banner.
func TestBanner_ContainsKeyHints(t *testing.T) {
	out := Banner("", "claude", "~/proj", 60)
	for _, want := range []string{"Enter send", "newline", "Ctrl+Q queue", "Esc interrupt", "Ctrl+C quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("Banner missing key hint %q in:\n%s", want, out)
		}
	}
}

func TestBanner_MinWidth(t *testing.T) {
	out := Banner("", "", "", 5)
	if !strings.Contains(out, "◆ octo") {
		t.Errorf("Banner should still render at tiny width; got:\n%s", out)
	}
}

func TestStatusBar_SegmentsAndHint(t *testing.T) {
	segs := [][2]string{{"model", "gpt-4"}, {"cost", "$0.01"}}
	out := StatusBar(segs, "Enter send", 30)
	for _, want := range []string{"gpt-4", "$0.01", "Enter send", "──"} {
		if !strings.Contains(out, want) {
			t.Errorf("StatusBar missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusBar_ZeroWidth(t *testing.T) {
	out := StatusBar(nil, "hint", 0)
	if !strings.Contains(out, "hint") {
		t.Errorf("StatusBar should still render hint at width 0; got:\n%s", out)
	}
}
