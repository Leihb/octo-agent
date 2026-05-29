package main

import (
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tools"
)

func TestFormatBgNote_WrapsAsSystemReminder(t *testing.T) {
	got := formatBgNote(tools.BgExit{
		ID:        "bg_1",
		Command:   "go test ./...",
		Status:    "exited: 0",
		NewOutput: "ok  github.com/x/y  1.2s\n",
	})
	for _, want := range []string{
		"<system-reminder>", "</system-reminder>",
		"bg_1", "go test ./...", "exited: 0", "ok  github.com/x/y  1.2s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatBgNote missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatBgNote_NoOutput(t *testing.T) {
	got := formatBgNote(tools.BgExit{ID: "bg_2", Command: "true", Status: "exited: 0"})
	if !strings.Contains(got, "(no new output)") {
		t.Errorf("want '(no new output)' marker; got:\n%s", got)
	}
}

func TestFormatBgNote_CarriesNonZeroExit(t *testing.T) {
	got := formatBgNote(tools.BgExit{ID: "bg_3", Command: "false", Status: "exited: exit status 1"})
	if !strings.Contains(got, "exit status 1") {
		t.Errorf("want the non-zero exit surfaced; got:\n%s", got)
	}
}
