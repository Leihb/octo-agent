package tools

import "strings"

// Helpers for building ToolResult.UI payloads — the structured result
// summaries the web frontend renders as rich cards (sessions.js
// _renderRichResult). A payload's "type" value must match a frontend
// renderer. Payloads persist in session JSON and ride every tool_result
// broadcast, so previews are hard-capped to keep transcripts small.

// uiHead returns the first maxLines lines of s, additionally capped at
// maxBytes bytes (cut at a line boundary where possible).
func uiHead(s string, maxLines, maxBytes int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes]
		if i := strings.LastIndexByte(out, '\n'); i > 0 {
			out = out[:i]
		}
	}
	return out
}

// uiTail returns the last maxLines lines of s, additionally capped at
// maxBytes bytes. Used where the end of the output matters most (shell
// commands: the error/summary is at the bottom).
func uiTail(s string, maxLines, maxBytes int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
		if i := strings.IndexByte(out, '\n'); i >= 0 && i < len(out)-1 {
			out = out[i+1:]
		}
	}
	return out
}
