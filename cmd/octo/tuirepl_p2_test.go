package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The status bar is a compact cwd / context% / perm strip: no key hints (those
// live in the startup banner) and no running-turn duration.
func TestRenderStatusBar_NoHintNoDuration(t *testing.T) {
	m := newTestModel()
	if !strings.Contains(m.renderStatusBar(), m.cwd) {
		t.Errorf("status bar should include the cwd; got:\n%s", m.renderStatusBar())
	}

	m.turnRunning = true
	m.turnStart = time.Now().Add(-90 * time.Second)
	out := m.renderStatusBar()
	for _, hint := range []string{"Enter", "interrupt", "newline", "queue"} {
		if strings.Contains(out, hint) {
			t.Errorf("status bar must not show key hint %q (banner owns hints); got:\n%s", hint, out)
		}
	}
	if strings.Contains(out, "1m30s") || strings.Contains(out, "elapsed") {
		t.Errorf("status bar must not show the turn duration; got:\n%s", out)
	}
}

func TestRenderInputBox_FlatStyle(t *testing.T) {
	m := newTestModel()
	setInput(m, "hi")

	// Flat style: no border chars at any width, just prompt + input.
	m.width = 0
	if got := m.renderInputBox(); strings.ContainsAny(got, "╭╮╰╯") {
		t.Errorf("no border expected at width 0; got:\n%s", got)
	}
	if !strings.Contains(m.renderInputBox(), "> ") {
		t.Error("input box should always show the prompt")
	}

	m.width = 60
	if got := m.renderInputBox(); strings.ContainsAny(got, "╭╮╰╯") {
		t.Errorf("flat style should never draw a border; got:\n%s", got)
	}
	if !strings.Contains(m.renderInputBox(), "> ") {
		t.Error("input box should always show the prompt")
	}
}

func TestAbbreviateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := abbreviateHome(home); got != "~" {
		t.Errorf("abbreviateHome(home) = %q, want ~", got)
	}
	sub := filepath.Join(home, "proj", "octo")
	if got := abbreviateHome(sub); got != "~"+string(filepath.Separator)+filepath.Join("proj", "octo") {
		t.Errorf("abbreviateHome(sub) = %q, want ~/proj/octo", got)
	}
	if got := abbreviateHome("/etc/hosts"); got != "/etc/hosts" {
		t.Errorf("abbreviateHome outside home should be unchanged; got %q", got)
	}
	if got := abbreviateHome(""); got != "" {
		t.Errorf("abbreviateHome(\"\") = %q, want empty", got)
	}
}
