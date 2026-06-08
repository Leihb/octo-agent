package main

import (
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/tools"
)

func TestCardVerbFor(t *testing.T) {
	cases := map[string]string{
		"edit_file":       "Update",
		"terminal":        "Run",
		"terminal_output": "Check",
		"kill_shell":      "Kill",
		"terminal_input":  "Input",
		"grep":            "Grep",
		"web_search":      "Search",
		"glob":            "Glob",
		"read_file":       "Read",
		"write_file":      "Write",
		"web_fetch":       "Fetch",
		"sub_agent":       "", // not a card tool → one-liner
		"remember":        "",
	}
	for tool, want := range cases {
		if got := cardVerbFor(tool); got != want {
			t.Errorf("cardVerbFor(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestCardTargetFor(t *testing.T) {
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"terminal", map[string]any{"command": "go test ./..."}, "go test ./..."},
		{"grep", map[string]any{"pattern": "TODO"}, "TODO"},
		{"web_search", map[string]any{"query": "golang generics"}, "golang generics"},
		{"read_file", map[string]any{"path": "main.go"}, "main.go"},
		{"web_fetch", map[string]any{"url": "https://x.example"}, "https://x.example"},
	}
	for _, c := range cases {
		if got := cardTargetFor(c.tool, c.input); got != c.want {
			t.Errorf("cardTargetFor(%q, %v) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

func TestRenderToolCard_Dispatch(t *testing.T) {
	// edit_file success → diff card.
	edit := renderToolCard("edit_file", map[string]any{
		"path": "/tmp/nope.go", "old_string": "alpha", "new_string": "beta",
	}, "", false)
	if !strings.Contains(edit, "Update(") || !strings.Contains(edit, "alpha") || !strings.Contains(edit, "beta") {
		t.Errorf("edit_file should render a diff card; got:\n%s", edit)
	}

	// terminal → output card with the command + output.
	run := renderToolCard("terminal", map[string]any{"command": "ls"}, "file_a\nfile_b", false)
	if !strings.Contains(run, "Run(ls)") || !strings.Contains(run, "file_a") {
		t.Errorf("terminal should render an output card; got:\n%s", run)
	}

	// terminal_output strips the STOP POLLING notice.
	pollOut := "[status: running]\n(no new output)\n\n" + tools.TerminalOutputStopPolling
	check := renderToolCard("terminal_output", map[string]any{"id": "bg_1"}, pollOut, false)
	if strings.Contains(check, "STOP POLLING") {
		t.Errorf("terminal_output card should strip STOP POLLING; got:\n%s", check)
	}

	// non-card tool → "".
	if got := renderToolCard("sub_agent", map[string]any{}, "x", false); got != "" {
		t.Errorf("non-card tool should return \"\"; got %q", got)
	}
}

func TestRendersCard_PlainDisablesCards(t *testing.T) {
	plainOff := &tuiModel{cfg: replConfig{plain: false}}
	if !plainOff.rendersCard("edit_file") {
		t.Error("with --plain off, edit_file should render as a card")
	}
	plainOn := &tuiModel{cfg: replConfig{plain: true}}
	if plainOn.rendersCard("edit_file") {
		t.Error("with --plain on, no tool should render as a card")
	}
	// terminal_output is now a card tool.
	if !plainOff.rendersCard("terminal_output") {
		t.Error("with --plain off, terminal_output should render as a card")
	}
	// A non-card tool is never a card regardless of --plain.
	if plainOff.rendersCard("sub_agent") {
		t.Error("sub_agent is not a card tool")
	}
}
