package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// Asker presents a structured question to the user and waits for their
// answer. Implementations live in cmd/octo (the REPL prompt reader); tests
// substitute fakes. Like Spawner, this interface stays free of the
// stdin/terminal mechanics so the tools package doesn't depend on cmd/octo.
type Asker interface {
	Ask(ctx context.Context, q AskRequest) (AskResponse, error)
}

// AskRequest is the structured question, parsed from the tool input.
type AskRequest struct {
	// Question is the prompt shown to the user, complete with punctuation.
	Question string

	// Options are the mutually-exclusive (or, when MultiSelect, jointly
	// selectable) labels. Empty means "free text answer" (the asker prompts
	// for an open-ended response with no list).
	Options []string

	// MultiSelect lets the user pick more than one option. The asker is
	// responsible for parsing a comma-separated selection and returning all
	// chosen labels.
	MultiSelect bool

	// Header is an optional short tag (≤12 chars) shown in the prompt UI.
	// Helpful when several questions share visual real estate.
	Header string
}

// AskResponse is what the user provided. Cancelled is true when the user
// dismissed the prompt (Ctrl-C, empty enter on a forced-pick, etc.); in
// that case Choices and Custom are empty.
type AskResponse struct {
	// Choices are the labels the user selected from Options. Empty when
	// the user picked "Other" (then Custom carries their free text) or
	// when the question had no Options (free text only).
	Choices []string

	// Custom is the free-text answer for "Other" picks or option-less
	// questions. Empty for plain selections.
	Custom string

	// Cancelled reports user dismissal. The tool surfaces this to the LLM
	// as a non-error result so the model can decide what to do next.
	Cancelled bool
}

// activeAsker, when non-nil, backs the ask_user_question tool and gates its
// advertisement in DefaultTools. Set by the REPL at session start; nil in
// single-turn / unattended modes, where prompting the user is impossible.
var activeAsker Asker

// SetAsker registers the asker the ask_user_question tool delegates to.
// Pass nil to disable (the tool then doesn't appear in DefaultTools).
func SetAsker(a Asker) { activeAsker = a }

func askerEnabled() bool { return activeAsker != nil }

// AskUserQuestionTool lets the model ask the user a single structured
// clarifying question. The point isn't to chat with the user — it's to
// resolve a branch where the model genuinely doesn't have enough
// information to pick a default and asking via free-form prose would
// produce a sloppy, hard-to-parse answer.
//
// Schema mirrors Claude Code's AskUserQuestion at the surface level but
// caps at ONE question per call (per the M11-prep design): simpler
// schema, simpler REPL UX, model fires multiple calls if it needs
// multiple questions.
type AskUserQuestionTool struct{}

func (AskUserQuestionTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "ask_user_question",
		Description: "Ask the user one structured clarifying question and wait for their answer. " +
			"Use this when you're at a branch where you genuinely lack the information to pick a " +
			"reasonable default — preferences (\"which library?\"), trade-offs (\"prioritize speed " +
			"or readability?\"), or scope (\"include the migration too?\"). Don't use it for " +
			"information you could find in the repo or for questions you should have an opinion " +
			"on yourself. Options must be 2-4 mutually exclusive labels; if you also pass " +
			"multi_select=true the user can pick more than one. An \"Other\" tail with a free-text " +
			"follow-up is always added. Result text is shaped like 'User chose: <label>' or, " +
			"for Other, 'User chose: Other — <free text>'.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The question to ask, complete with punctuation. Should be one sentence; if you need more context, put it in the option labels instead of the question itself.",
				},
				"options": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    2,
					"maxItems":    4,
					"description": "2-4 mutually exclusive labels. Each label should be a complete choice (\"OAuth with PKCE\" not just \"OAuth\"). The asker adds an \"Other (free text)\" tail automatically — don't include one yourself.",
				},
				"multi_select": map[string]any{
					"type":        "boolean",
					"description": "Set true when the choices are NOT mutually exclusive (e.g. \"which features should we enable?\"). The user can then pick a comma-separated subset.",
				},
				"header": map[string]any{
					"type":        "string",
					"description": "Optional short tag shown in the prompt UI (≤12 chars). Helps the user track which question is which when you ask several in sequence. Example: 'auth_method', 'scope'.",
				},
			},
			"required": []string{"question", "options"},
		},
	}
}

func (AskUserQuestionTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if !askerEnabled() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("ask_user_question: not available in this mode (REPL only)")
	}
	question := strings.TrimSpace(stringArg(input, "question"))
	if question == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("ask_user_question: question is required")
	}
	options := stringSliceArg(input, "options")
	if len(options) < 2 || len(options) > 4 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("ask_user_question: options must have 2-4 entries (got %d)", len(options))
	}
	multi, _ := input["multi_select"].(bool)
	header := strings.TrimSpace(stringArg(input, "header"))

	res, err := activeAsker.Ask(ctx, AskRequest{
		Question:    question,
		Options:     options,
		MultiSelect: multi,
		Header:      header,
	})
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("ask_user_question: %w", err)
	}
	return agent.ToolResult{Text: formatAskResponse(res)}, nil
}

// formatAskResponse turns the asker's structured reply into the text the LLM
// reads as its tool_result. Three shapes:
//
//	cancelled                  → "(user cancelled)"
//	plain selection(s)         → "User chose: A" or "User chose: A, B"
//	"Other" with free text     → "User chose: Other — <text>"
func formatAskResponse(r AskResponse) string {
	if r.Cancelled {
		return "(user cancelled)"
	}
	if r.Custom != "" && len(r.Choices) == 0 {
		return "User chose: Other — " + r.Custom
	}
	if len(r.Choices) == 0 {
		return "(user cancelled)" // defensive: no choices and no custom → nothing to report
	}
	return "User chose: " + strings.Join(r.Choices, ", ")
}
