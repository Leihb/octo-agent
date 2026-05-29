package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// Spawner runs one sub-agent task to completion and returns the final reply.
// Implementations live outside the tools package (cmd/octo wires the real one,
// tests substitute fakes) so this package stays free of agent-construction
// concerns. Multiple Spawner calls may run concurrently from one parent
// tool_use batch (see parallel dispatch in agent.dispatchTools), so the
// implementation MUST be safe for concurrent invocation on distinct requests.
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) (SpawnResult, error)
	// Continue re-runs a still-alive sub-agent (one a prior Spawn kept in the
	// implementation's live registry) with a new message and returns its next
	// reply. The sub-agent's prior history carries over. An unknown or
	// already-evicted agentID returns an error whose text tells the model to
	// launch a fresh sub-agent instead. Concurrent Continue calls on the SAME
	// agentID must be serialized by the implementation — a sub-agent's history
	// can't take two interleaved turns.
	Continue(ctx context.Context, agentID, message string) (SpawnResult, error)
}

// SpawnRequest is the LLM-supplied launch_agent payload, parsed.
type SpawnRequest struct {
	// Description is a short human-readable label for logging / progress UI.
	// Not load-bearing for the LLM — the actual task is in Prompt.
	Description string

	// Prompt is the sub-agent's user message. It carries the task.
	Prompt string

	// Tools, when non-empty, restricts the child to this subset of the
	// parent's tool list. nil/empty means "inherit all of parent's tools
	// except launch_agent itself" — the spawn implementation handles the
	// recursion filter, not the tool.
	Tools []string

	// Model, when non-empty, overrides the parent's model for this child.
	// Empty means "use the parent's default" — typically the same model.
	Model string
}

// SpawnResult is the sub-agent's final output, plus its token usage so the
// parent can roll it into the session total (the same way /cost displays one
// number even when sub-agents fired side calls).
type SpawnResult struct {
	// AgentID addresses the sub-agent for a later send_message. Spawn returns
	// a non-empty id when the implementation keeps the child alive; the
	// launch_agent tool surfaces it to the model in an "[agent <id>]" tag.
	// Empty when the implementation doesn't support continuation (e.g. test
	// stubs) — the tag is then omitted.
	AgentID      string
	Reply        string
	InputTokens  int
	OutputTokens int
}

// activeSpawner, when non-nil, backs the launch_agent tool and gates its
// advertisement in DefaultTools. Set once at session start via SetSpawner.
// Nil disables sub-agent dispatch — the tool stays out of the LLM-facing
// schema and Execute errors. Mirrors activeMemory / activeSkills.
var activeSpawner Spawner

// SetSpawner registers the function the launch_agent tool delegates to. Pass
// nil to disable (the tool then doesn't appear in DefaultTools).
func SetSpawner(s Spawner) { activeSpawner = s }

// ActiveSpawner returns the currently registered Spawner, or nil if none.
// Used by callers outside the tools package (cmd/octo's memory consolidator)
// to spawn a sub-agent without going through a LLM-driven tool call. May
// return nil — caller is responsible for the fallback path.
func ActiveSpawner() Spawner { return activeSpawner }

func spawnerEnabled() bool { return activeSpawner != nil }

// subAgentCtxKey marks a context as belonging to a sub-agent's run. The
// launch_agent tool checks for it to refuse recursive nesting (a sub-agent
// trying to spawn another sub-agent), defense-in-depth on top of the
// "drop launch_agent from the child's tool list" filter the Spawner applies.
type subAgentCtxKeyType struct{}

var subAgentCtxKey = subAgentCtxKeyType{}

// WithSubAgentMarker stamps ctx so descendants are detectable as sub-agent
// work. The Spawner implementation calls this when invoking the child loop.
func WithSubAgentMarker(ctx context.Context) context.Context {
	return context.WithValue(ctx, subAgentCtxKey, true)
}

// IsSubAgent reports whether ctx is currently inside a sub-agent's run. Used
// by the launch_agent tool to refuse recursion.
func IsSubAgent(ctx context.Context) bool {
	v, _ := ctx.Value(subAgentCtxKey).(bool)
	return v
}

// LaunchAgentTool spawns a child agent that handles a single prompt in
// isolation and returns its final reply. Mapped to the LLM as "launch_agent"
// — name borrowed from Claude Code's `Agent` tool but the surface is closer
// to Codex's sub-agent (no event stream surfaced to the parent, just a
// final string).
type LaunchAgentTool struct{}

func (LaunchAgentTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "launch_agent",
		Description: "Spawn an autonomous sub-agent to handle a focused sub-task and " +
			"return its final answer. Use when you need parallel investigation (research two " +
			"unrelated areas in one tool_use), when you want a fresh context window for an " +
			"isolated sub-problem (the sub-agent doesn't see this conversation), or when the " +
			"sub-task is well-defined enough to delegate without back-and-forth. The sub-agent " +
			"has the same tools as you (minus launch_agent itself — no recursion) and reports " +
			"a single text reply when done. Multiple launch_agent calls in one tool_use batch " +
			"run in parallel. Don't use for tasks that need ongoing user steering or that " +
			"depend on what's in this conversation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "Short human-readable label for this sub-agent (3-7 words). Shown in progress UI; doesn't shape behavior. Example: 'Investigate auth middleware'.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task for the sub-agent. Self-contained: include all context the sub-agent needs (file paths, constraints, deliverable) since it can't see this conversation. State the expected output shape (a summary, a list, a YES/NO).",
				},
				"tools": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional tool-name allowlist for the sub-agent. Omit or pass empty to inherit your tools (minus launch_agent). Useful to harden a research sub-agent to read-only (e.g. ['read_file','grep','glob']).",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override. Defaults to a cheaper model than the parent when supported, otherwise reuses the parent's model.",
				},
			},
			"required": []string{"description", "prompt"},
		},
	}
}

func (LaunchAgentTool) Execute(ctx context.Context, _ string, input map[string]any) (string, error) {
	if !spawnerEnabled() {
		return "", fmt.Errorf("launch_agent: sub-agent dispatch is not configured for this session")
	}
	if IsSubAgent(ctx) {
		// Defense in depth — the Spawner is supposed to filter launch_agent
		// out of a child's tool list, but a hallucinated tool call would
		// otherwise still execute through the shared registry.
		return "", fmt.Errorf("launch_agent: a sub-agent cannot spawn another sub-agent")
	}

	desc := strings.TrimSpace(stringArg(input, "description"))
	prompt := strings.TrimSpace(stringArg(input, "prompt"))
	if prompt == "" {
		return "", fmt.Errorf("launch_agent: prompt is required")
	}
	if desc == "" {
		// Falling back to a truncated prompt is friendlier than an error —
		// the LLM may forget the optional-feeling label sometimes.
		desc = firstLine(prompt)
	}

	req := SpawnRequest{
		Description: desc,
		Prompt:      prompt,
		Tools:       stringSliceArg(input, "tools"),
		Model:       strings.TrimSpace(stringArg(input, "model")),
	}
	res, err := activeSpawner.Spawn(ctx, req)
	if err != nil {
		return "", fmt.Errorf("launch_agent: %w", err)
	}
	reply := strings.TrimSpace(res.Reply)
	if reply == "" {
		// A sub-agent that ended with no final text isn't fatal — it just
		// has nothing to say. Surface that explicitly so the parent doesn't
		// guess.
		return "(sub-agent " + desc + " produced no reply)", nil
	}
	return withAgentTag(res.AgentID, reply), nil
}

// stringSliceArg pulls an []string argument tolerating absence and the JSON
// pattern where everything comes through as []any.
func stringSliceArg(input map[string]any, key string) []string {
	raw, ok := input[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
