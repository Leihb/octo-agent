package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// UserPrompt is a structured request for a synchronous answer from the user,
// raised mid-turn from the agent goroutine. The two kinds — a tool-permission
// approval and an ask_user_question selection — share one path so the view
// (plain stdin today, a bubbletea modal next) renders and answers them
// uniformly. See dev-docs/tui-input-modes-design.md §6.
type UserPrompt struct {
	Kind UserPromptKind

	// Permission (KindPermission): the tool the agent wants to run.
	ToolName  string
	ToolInput map[string]any

	// Question (KindQuestion): the ask_user_question payload.
	Header      string
	Question    string
	Options     []string
	MultiSelect bool
}

// UserPromptKind tags a UserPrompt.
type UserPromptKind int

const (
	// KindPermission asks the user to approve/deny a tool call.
	KindPermission UserPromptKind = iota
	// KindQuestion asks the user to pick option(s) or supply free text.
	KindQuestion
)

// UserResponse is the structured answer to a UserPrompt.
type UserResponse struct {
	// Permission (KindPermission):
	Allow  bool // run this tool call
	Always bool // ...and remember the allow for the rest of the session

	// Question (KindQuestion):
	Choices   []string // selected option label(s)
	Custom    string   // free-text ("Other") answer
	Cancelled bool     // user declined / no usable answer
}

// userPrompter is the narrow capability the permission gate and the
// ask_user_question asker need from a view: raise a structured prompt and
// block for the answer. Both plainView and the bubbletea view satisfy it, so
// the gate/asker stay ignorant of how the answer is gathered (stdin line vs
// modal). It is exactly ViewSink's Ask method, split out so callers depend
// only on what they use.
type userPrompter interface {
	Ask(ctx context.Context, p UserPrompt) (UserResponse, error)
}

// parseSelection converts the user's typed answer into a list of 1-based
// option indices. Empty / non-numeric input returns nil with no error;
// genuinely malformed input (e.g. mixing numbers and prose) errors so the
// caller can treat it as cancellation rather than guessing.
//
// In single-select mode, only the first index is honored even if the user
// typed multiple.
func parseSelection(raw string, maxIdx int, multi bool) ([]int, error) {
	parts := strings.Split(raw, ",")
	var out []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("not a number: %q", p)
		}
		if n < 1 || n > maxIdx {
			return nil, fmt.Errorf("out of range: %d", n)
		}
		out = append(out, n)
	}
	if !multi && len(out) > 1 {
		out = out[:1]
	}
	return out, nil
}
