package main

import (
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
)

// outputCardMaxLines caps how many output lines a tool-result card shows
// before collapsing the rest into a "N more lines" marker.
const outputCardMaxLines = 12

// cardVerbFor maps a tool name to the verb shown in its result card, or ""
// for tools that render as a terse one-line status instead of a card. This is
// the set of tools the TUI renders as rich cards (the plain/headless path
// always uses one-liners — see dev-docs/tui-ux-upgrade-design.md decision #8).
func cardVerbFor(toolName string) string {
	switch toolName {
	case "edit_file":
		return "Update"
	case "terminal":
		return "Run"
	case "terminal_output":
		return "Check"
	case "kill_shell":
		return "Kill"
	case "terminal_input":
		return "Input"
	case "grep":
		return "Grep"
	case "web_search":
		return "Search"
	case "glob":
		return "Glob"
	case "read_file":
		return "Read"
	case "write_file":
		return "Write"
	case "web_fetch":
		return "Fetch"
	}
	return ""
}

// cardTargetFor extracts the card header's target (command / pattern / path /
// url) from a tool's input, falling back to a one-line input summary.
func cardTargetFor(toolName string, input map[string]any) string {
	var key string
	switch toolName {
	case "terminal":
		key = "command"
	case "terminal_output", "kill_shell", "terminal_input":
		if id, ok := input["id"].(string); ok && id != "" {
			if cmd, found := tools.BgCommand(id); found && cmd != "" {
				return fmt.Sprintf("%s (%s)", id, truncate1Line(cmd))
			}
			return id
		}
	case "grep", "glob":
		key = "pattern"
	case "web_search":
		key = "query"
	case "read_file", "edit_file", "write_file":
		key = "path"
	case "web_fetch":
		key = "url"
	}
	if key != "" {
		if s, ok := input[key].(string); ok && s != "" {
			return truncate1Line(s)
		}
	}
	return summariseInput(input)
}

// renderToolCard returns the rich card for a finished tool call, or "" if the
// tool isn't a card tool (caller falls back to a one-line status). edit_file
// success renders a diff card; everything else (and edit_file errors) renders
// an output-preview card. TUI-only — the plain path never calls this.
func renderToolCard(toolName string, input map[string]any, output string, isErr bool) string {
	verb := cardVerbFor(toolName)
	if verb == "" {
		return ""
	}
	if toolName == "edit_file" && !isErr {
		path, _ := input["path"].(string)
		oldS, _ := input["old_string"].(string)
		newS, _ := input["new_string"].(string)
		return tui.RenderEditCard(path, oldS, newS)
	}
	lang := ""
	if toolName == "read_file" || toolName == "write_file" {
		if p, _ := input["path"].(string); p != "" {
			lang = tui.GuessLanguage(p)
		}
	}
	if toolName == "terminal" {
		// The background-launch result carries a model-only "don't poll"
		// instruction appended after a blank line. It's noise for the human, so
		// drop it from the card — the "Started background process N" line stays.
		output = strings.TrimRight(strings.TrimSuffix(output, tools.BgPollNotice), "\n")
	}
	if toolName == "write_file" {
		// write_file's own output is already a human-readable summary
		// ("Wrote N bytes (M lines) to /path"); show that instead of a
		// redundant content preview.
		return tui.RenderOutputCard(verb, cardTargetFor(toolName, input), output, 0, isErr, "")
	}
	return tui.RenderOutputCard(verb, cardTargetFor(toolName, input), output, outputCardMaxLines, isErr, lang)
}
