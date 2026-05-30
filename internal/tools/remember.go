package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/memory"
)

// activeMemory, when non-nil, backs the `remember` tool and gates its
// advertisement in DefaultTools. Set once at session start via SetMemoryStore;
// nil (e.g. --no-memory) disables immediate writes. Mirrors activeSkills.
var activeMemory *memory.Store

// SetMemoryStore registers the store the `remember` tool writes to. cmd/octo
// calls this at session start; pass nil to disable.
func SetMemoryStore(s *memory.Store) { activeMemory = s }

func memoryEnabled() bool { return activeMemory != nil }

// RememberTool persists a durable fact to cross-session memory the moment the
// model recognizes one (a user preference, feedback, correction, or info worth
// recalling later). The write lands immediately; it surfaces in the *next*
// session's system prompt (the current session already has it in context).
type RememberTool struct{}

func (RememberTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "remember",
		Description: "Save a durable fact to cross-session memory when the user states a " +
			"lasting preference, gives feedback/a correction, or shares info worth recalling in " +
			"future sessions (e.g. \"run tests before committing\", \"I prefer Go\"). It persists " +
			"for the next session — don't use it for one-off task details or things already in the " +
			"repo/CLAUDE.md.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{
					"type":        "string",
					"description": "The fact to remember, stated plainly. For feedback/project facts, include why it matters.",
				},
				"type": map[string]any{
					"type":        "string",
					"enum":        []string{"user", "feedback", "project", "reference"},
					"description": "user = who the user is/preferences; feedback = how to work (corrections/confirmed approaches); project = ongoing work/constraints; reference = external resource pointers.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Optional one-line summary used in the memory index. Defaults to the first line of content.",
				},
			},
			"required": []string{"content"},
		},
	}
}

func (RememberTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	content := strings.TrimSpace(stringArg(input, "content"))
	if content == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("remember: content is required")
	}
	if !memoryEnabled() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("remember: memory is disabled for this session")
	}
	desc := strings.TrimSpace(stringArg(input, "description"))
	if desc == "" {
		desc = firstLine(content)
	}
	cwd, _ := os.Getwd()
	e := memory.Entry{
		Name:        slugify(desc),
		Description: desc,
		Type:        memory.Type(strings.TrimSpace(stringArg(input, "type"))),
		Cwd:         memory.ProjectRoot(cwd),
		Body:        content,
	}
	if e.Name == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("remember: could not derive a name from the content")
	}
	if err := activeMemory.Save(e); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("remember: %w", err)
	}
	return agent.ToolResult{Text: "Remembered (" + string(normalizeType(e.Type)) + "): " + desc}, nil
}

// normalizeType mirrors the store's defaulting so the confirmation message
// reports the type that was actually saved.
func normalizeType(t memory.Type) memory.Type {
	switch t {
	case memory.TypeUser, memory.TypeFeedback, memory.TypeProject, memory.TypeReference:
		return t
	}
	return memory.TypeReference
}

// stringArg pulls a string argument, tolerating absence.
func stringArg(input map[string]any, key string) string {
	v, _ := input[key].(string)
	return v
}

// firstLine returns the first non-empty line, capped, for use as a description.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 80 {
			line = strings.TrimSpace(line[:80])
		}
		return line
	}
	return ""
}

// slugify turns a description into a kebab-case filename stem (lowercase
// alphanumerics, runs of other chars collapsed to a single dash), capped so the
// filename stays sane.
//
// Falls back to a hash when the input contains no ASCII alphanumerics (e.g.
// pure CJK text) so the result is never empty.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 50 {
		out = strings.Trim(out[:50], "-")
	}
	if out == "" {
		return memory.Slugify(s)
	}
	return out
}
