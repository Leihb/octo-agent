// Package memory implements octo's cross-session memory as plain markdown
// files the agent manages with its own file tools.
package memory

import (
	"strings"
)

// Injector handles structured memory injection into the system prompt.
type Injector struct {
	parsed *ParsedIndex
	// acknowledged tracks which CRITICAL entries have been acknowledged
	// in the current session. Map key is entry title.
	acknowledged map[string]bool
	// recalled tracks which HIGH entries have been triggered this session
	// to avoid duplicate recalls.
	recalled map[string]bool
}

// NewInjector creates an injector from a parsed memory index.
func NewInjector(parsed *ParsedIndex) *Injector {
	return &Injector{
		parsed:       parsed,
		acknowledged: make(map[string]bool),
		recalled:     make(map[string]bool),
	}
}

// HasCritical returns true if there are unacknowledged CRITICAL entries.
func (i *Injector) HasCritical() bool {
	for _, e := range i.parsed.EntriesByPriority(PriorityCritical) {
		if !i.acknowledged[e.Title] {
			return true
		}
	}
	return false
}

// Acknowledge marks a CRITICAL entry as acknowledged by title.
func (i *Injector) Acknowledge(title string) {
	i.acknowledged[title] = true
}

// AcknowledgeAll marks all CRITICAL entries as acknowledged.
func (i *Injector) AcknowledgeAll() {
	for _, e := range i.parsed.EntriesByPriority(PriorityCritical) {
		i.acknowledged[e.Title] = true
	}
}

// StartupInjection returns the text to inject at session start.
// It includes unacknowledged CRITICAL entries with their confirmation text.
func (i *Injector) StartupInjection() string {
	var b strings.Builder
	for _, e := range i.parsed.EntriesByPriority(PriorityCritical) {
		if i.acknowledged[e.Title] {
			continue
		}
		b.WriteString("🔴 **")
		b.WriteString(e.Title)
		b.WriteString("**\n\n")
		if e.ConfirmText != "" {
			b.WriteString("确认话术：")
			b.WriteString(e.ConfirmText)
			b.WriteString("\n\n")
		}
		// Add a brief summary
		for _, line := range strings.Split(e.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "-") &&
				!strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "确认") {
				b.WriteString("_")
				b.WriteString(firstSentence(trimmed))
				b.WriteString("_\n\n")
				break
			}
		}
		b.WriteString("---\n\n")
	}
	return b.String()
}

// TriggerRecall checks user input for HIGH entry triggers and returns
// recall text for newly matched entries.
func (i *Injector) TriggerRecall(userInput string) string {
	keywords := extractKeywords(userInput)
	matched := i.parsed.MatchTriggers(keywords)
	var b strings.Builder
	for _, e := range matched {
		if i.recalled[e.Title] {
			continue
		}
		i.recalled[e.Title] = true
		b.WriteString("🟡 **")
		b.WriteString(e.Title)
		b.WriteString("**：")
		b.WriteString(firstSentence(e.Body))
		b.WriteString("\n\n")
	}
	if b.Len() > 0 {
		return "## 上下文提醒\n\n" + b.String() + "---\n\n"
	}
	return ""
}

// extractKeywords pulls potential trigger words from user input.
// It splits on delimiters and also extracts substrings for Chinese text
// so that "帮我跑一下测试" yields "跑测试" which can match trigger "跑测试".
func extractKeywords(input string) []string {
	var keywords []string
	seen := make(map[string]bool)

	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		keywords = append(keywords, s)
	}

	// Split on common delimiters first
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ' ' || r == '，' || r == ',' || r == '。' || r == '.' ||
			r == '、' || r == '|' || r == '\n' || r == '\t' || r == '！' ||
			r == '?' || r == '？' || r == ';' || r == '；'
	})

	for _, f := range fields {
		add(f)
		add(strings.ToLower(f))

		// For Chinese text, also extract overlapping substrings of length 2-4
		// This helps match triggers like "跑测试" inside "帮我跑一下测试"
		runes := []rune(f)
		if len(runes) >= 2 {
			for length := 2; length <= 4 && length <= len(runes); length++ {
				for i := 0; i+length <= len(runes); i++ {
					add(string(runes[i : i+length]))
				}
			}
		}
	}
	return keywords
}
