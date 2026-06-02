// Package memory implements octo's cross-session memory as plain markdown
// files the agent manages with its own file tools.
package memory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Priority defines how strongly a memory entry should be injected.
type Priority string

const (
	PriorityCritical Priority = "CRITICAL" // Must be acknowledged on every session start
	PriorityHigh     Priority = "HIGH"     // Auto-recalled when related operations are triggered
	PriorityNormal   Priority = "NORMAL"   // Background context, recalled on demand
)

// Entry is a single parsed memory item from MEMORY.md.
type Entry struct {
	Priority    Priority
	Title       string
	Body        string   // raw markdown body (may contain nested lists, code blocks)
	ConfirmText string   // explicit confirmation phrase for CRITICAL entries
	Triggers    []string // keywords that trigger recall (HIGH entries)
	References  []string // linked topic files, e.g. [foo.md](foo.md)
}

// ParsedIndex is the structured representation of a MEMORY.md file.
type ParsedIndex struct {
	Raw      string   // original truncated text (for backward compat)
	Entries  []Entry  // structured entries
	Warnings []string // parse issues (unknown priority tags, etc.)
}

// headingPattern matches: ## 🔴 CRITICAL — Title  or  ## 🟡 HIGH — Title  or  ### Title
// Also accepts plain "## CRITICAL" / "## HIGH" without emoji.
var headingPattern = regexp.MustCompile(`^(#{2,3})\s*(?:🔴\s*|🟡\s*|🟢\s*)?\s*(CRITICAL|HIGH|NORMAL)?\s*(?:[-–—]\s*)?\s*(.*)$`)

// confirmPattern extracts confirmation text from lines like:
// "确认话术：..." or "Confirm: ..." or "**确认**：..."
var confirmPattern = regexp.MustCompile(`(?i)(?:确认话术|确认|confirm)[:：]\s*(.+)`)

// triggerPattern extracts trigger keywords from lines like:
// "触发场景：a, b, c" or "Triggers: a | b | c"
var triggerPattern = regexp.MustCompile(`(?i)(?:触发场景|triggers?)[:：]\s*(.+)`)

// refPattern finds markdown links like [text](file.md) or [text](./file.md)
var refPattern = regexp.MustCompile(`\[([^\]]+)\]\((?:\.\/)?([^)]+\.md)\)`)

// ParseIndex reads and parses MEMORY.md into structured entries.
// It still respects the injection budget (maxInjectLines / maxInjectBytes).
func ParseIndex(dir string) *ParsedIndex {
	b, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		return &ParsedIndex{Raw: ""}
	}
	truncated := truncateForInjection(string(b))
	return parseStructured(truncated)
}

// parseStructured turns truncated MEMORY.md markdown into structured entries.
func parseStructured(raw string) *ParsedIndex {
	p := &ParsedIndex{Raw: raw}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), maxInjectBytes+1024)

	var current *Entry
	var bodyLines []string

	flushEntry := func() {
		if current == nil {
			return
		}
		current.Body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
		// Extract references from body
		for _, m := range refPattern.FindAllStringSubmatch(current.Body, -1) {
			current.References = append(current.References, m[2])
		}
		p.Entries = append(p.Entries, *current)
		current = nil
		bodyLines = nil
	}

	for sc.Scan() {
		line := sc.Text()
		m := headingPattern.FindStringSubmatch(line)
		if m != nil {
			flushEntry()
			level, prioStr, title := m[1], m[2], m[3]
			_ = level // we treat ## and ### the same for now

			var prio Priority
			switch strings.ToUpper(prioStr) {
			case "CRITICAL":
				prio = PriorityCritical
			case "HIGH":
				prio = PriorityHigh
			case "NORMAL", "":
				prio = PriorityNormal
			default:
				p.Warnings = append(p.Warnings, fmt.Sprintf("unknown priority %q in heading: %s", prioStr, line))
				prio = PriorityNormal
			}

			current = &Entry{
				Priority: prio,
				Title:    strings.TrimSpace(title),
			}
			continue
		}

		if current == nil {
			continue // skip lines before first heading
		}

		// Extract confirm text
		if c := confirmPattern.FindStringSubmatch(line); c != nil {
			current.ConfirmText = strings.TrimSpace(c[1])
		}

		// Extract triggers
		if t := triggerPattern.FindStringSubmatch(line); t != nil {
			parts := strings.FieldsFunc(t[1], func(r rune) bool {
				return r == ',' || r == '|' || r == '、'
			})
			for _, part := range parts {
				if s := strings.TrimSpace(part); s != "" {
					current.Triggers = append(current.Triggers, s)
				}
			}
		}

		bodyLines = append(bodyLines, line)
	}
	flushEntry()
	return p
}

// EntriesByPriority returns all entries with the given priority.
func (p *ParsedIndex) EntriesByPriority(prio Priority) []Entry {
	var out []Entry
	for _, e := range p.Entries {
		if e.Priority == prio {
			out = append(out, e)
		}
	}
	return out
}

// MatchTriggers returns HIGH entries whose triggers intersect with the given keywords.
func (p *ParsedIndex) MatchTriggers(keywords []string) []Entry {
	var out []Entry
	for _, e := range p.Entries {
		if e.Priority != PriorityHigh {
			continue
		}
		for _, kw := range keywords {
			kwLower := strings.ToLower(kw)
			for _, t := range e.Triggers {
				if strings.Contains(strings.ToLower(t), kwLower) || strings.Contains(kwLower, strings.ToLower(t)) {
					out = append(out, e)
					goto nextEntry
				}
			}
		}
	nextEntry:
	}
	return out
}

// RenderAcknowledgement builds the session-start confirmation text for CRITICAL entries.
func (p *ParsedIndex) RenderAcknowledgement() string {
	critical := p.EntriesByPriority(PriorityCritical)
	if len(critical) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## 🔴 会话启动确认\n\n")
	for _, e := range critical {
		b.WriteString(fmt.Sprintf("### %s\n\n", e.Title))
		if e.ConfirmText != "" {
			b.WriteString(fmt.Sprintf("**确认**：%s\n\n", e.ConfirmText))
		}
		// Include a brief summary of the body (first non-empty line that's not a list item)
		for _, line := range strings.Split(e.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "*") {
				b.WriteString(fmt.Sprintf("_要点：%s_\n\n", trimmed))
				break
			}
		}
	}
	b.WriteString("---\n\n")
	return b.String()
}

// RenderTriggerRecall builds a recall block for matched HIGH entries.
func (p *ParsedIndex) RenderTriggerRecall(keywords []string) string {
	matched := p.MatchTriggers(keywords)
	if len(matched) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## 🟡 上下文提醒\n\n")
	for _, e := range matched {
		b.WriteString(fmt.Sprintf("**%s**：%s\n\n", e.Title, firstSentence(e.Body)))
	}
	b.WriteString("---\n\n")
	return b.String()
}

// firstSentence extracts the first meaningful sentence from a body for brief display.
// It skips metadata lines (触发场景, 确认话术, etc.) and empty lines.
func firstSentence(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip metadata lines
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "触发场景") || strings.HasPrefix(lower, "确认") ||
			strings.HasPrefix(lower, "triggers") || strings.HasPrefix(lower, "confirm") ||
			strings.HasPrefix(lower, "规范") || strings.HasPrefix(lower, "要点") {
			continue
		}
		// Skip list markers
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") {
			continue
		}
		if idx := strings.IndexAny(line, ".。"); idx > 0 {
			return strings.TrimSpace(line[:idx])
		}
		if len(line) > 100 {
			return line[:100] + "..."
		}
		return line
	}
	return ""
}
