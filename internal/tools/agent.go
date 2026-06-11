package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// AgentTool is the unified sub-agent tool. It replaces the previous
// explore_agent / plan_agent / general_agent /
// code_review_agent split with a single tool controlled by parameters.
//
// Parameters:
//   - description: short label for UI/logging
//   - prompt:      the task (self-contained — the child can't see this conversation)
//   - subagent_type: optional agent type (explore, plan, general, code-review).
//     Omit to fork yourself — the child inherits your full conversation context.
//   - run_in_background: when true the agent runs async and you are notified
//     on completion. When false (default) it blocks and returns the result.
//   - model: optional model override
//   - tools: optional tool-name allowlist for the child
//
// The tool is advertised only when a SubAgentManager is registered.
type AgentTool struct{}

func (AgentTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "sub_agent",
		Description: "Launch an autonomous sub-agent to handle a focused sub-task. " +
			"The sub-agent runs with its own context window and tool budget. " +
			"Use when you need parallel investigation, a fresh context for an isolated " +
			"sub-problem, or when the task is well-defined enough to delegate.\n\n" +
			"Two modes:\n" +
			"- **Fork** (omit subagent_type): the child inherits your system prompt — the " +
			"shared harness identity (base rules, env, skills, memory) — and shares its " +
			"prompt cache, so it's cheap. It does NOT see this conversation's messages, so " +
			"still put everything the task needs in `prompt`. Use when the intermediate " +
			"tool output isn't worth keeping in your context.\n" +
			"- **Fresh agent** (set subagent_type): the child starts with zero context and a " +
			"specialized persona. Provide a complete task description. Use when you want an " +
			"independent read (e.g. code review).\n\n" +
			"Set run_in_background=true to run asynchronously — you will be notified when " +
			"it completes. Leave it false (default) to block and receive the result directly. " +
			"(Some transports run every sub-agent synchronously; the result says so when it does.)\n\n" +
			"Follow up with sub_agent_send (message a child with its context intact), " +
			"sub_agent_status (check progress / latest result), and sub_agent_kill.",
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
				"subagent_type": map[string]any{
					"type":        "string",
					"description": "Optional agent type. 'explore' (read-only research), 'plan' (read-only planning), 'general' (full toolbelt), 'code-review' (read-only review). Omit to fork yourself — the child inherits your system prompt (not this conversation's messages).",
				},
				"run_in_background": map[string]any{
					"type":        "boolean",
					"description": "When true, run asynchronously and receive a notification on completion. When false (default), block until the agent finishes and return its result directly.",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override. Defaults to the parent's model.",
				},
				"tools": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional tool-name allowlist for the sub-agent. Omit to inherit your tools (minus sub_agent itself — no recursion).",
				},
			},
			"required": []string{"description", "prompt"},
		},
	}
}

func (AgentTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		return agent.ToolResult{Text: ""}, fmt.Errorf("sub_agent: a sub-agent cannot spawn another sub-agent")
	}

	desc := strings.TrimSpace(stringArg(input, "description"))
	prompt := strings.TrimSpace(stringArg(input, "prompt"))
	if prompt == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("sub_agent: prompt is required")
	}
	if desc == "" {
		desc = firstLine(prompt)
	}

	mgr := resolveSubAgentManager(ctx, nil)
	if mgr == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("sub_agent: sub-agent dispatch is not configured for this session")
	}

	// Resolve subagent_type → preset or fork
	subagentType := strings.TrimSpace(stringArg(input, "subagent_type"))
	var preset *agentPreset
	if subagentType != "" {
		p, ok := lookupAgentPreset(subagentType)
		if !ok {
			return agent.ToolResult{Text: ""}, fmt.Errorf("sub_agent: unknown subagent_type %q. Available: %s", subagentType, listPresetNames())
		}
		preset = &p
	}

	// Build the spawn request
	req := SpawnRequest{
		Description: desc,
		Prompt:      prompt,
		Tools:       stringSliceArg(input, "tools"),
		Model:       strings.TrimSpace(stringArg(input, "model")),
	}
	if preset != nil {
		req.SystemSuffix = preset.persona
		req.ReadOnly = preset.readOnly
	}

	// Determine sync vs async
	runInBackground := boolArg(input, "run_in_background")
	// Synchronous transports (server / IM) have no follow-up-turn channel, so
	// force sync even if the model asked for background — and tell the model,
	// rather than silently downgrading its choice.
	forcedSync := false
	if runInBackground && mgr.Synchronous() {
		runInBackground = false
		forcedSync = true
	}

	if runInBackground {
		id, err := mgr.Start(req)
		if err != nil {
			return agent.ToolResult{Text: ""}, fmt.Errorf("sub_agent: %w", err)
		}
		return agent.ToolResult{
			Text: fmt.Sprintf("Started sub-agent %s. You will be notified when it completes.", id),
		}, nil
	}

	// Synchronous path — block and return the result.
	res, err := mgr.RunSync(ctx, req)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("sub_agent: %w", err)
	}
	text := withAgentTag(res.AgentID, res.Reply)
	// Surface a truncated result rather than passing a partial reply off as
	// complete: a sub-agent that hit its turn limit returns partial work.
	if res.StopReason == "max_turns" {
		text += "\n\n[INCOMPLETE: this sub-agent hit its turn limit — the result above is partial. Re-launch with a narrower task, or treat it as unfinished.]"
	}
	if forcedSync {
		text += "\n\n[note: ran synchronously and returned its full result here — this transport doesn't support background sub-agents, so run_in_background was ignored.]"
	}
	return agent.ToolResult{Text: text}, nil
}

// boolArg pulls a boolean argument, defaulting to false.
func boolArg(input map[string]any, key string) bool {
	raw, ok := input[key]
	if !ok {
		return false
	}
	if v, ok := raw.(bool); ok {
		return v
	}
	return false
}
