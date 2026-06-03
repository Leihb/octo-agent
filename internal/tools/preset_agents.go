package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// presetAgent describes a named sub-agent type. Each preset becomes its own
// top-level tool (explore_agent, plan_agent, …) rather than a parameter on
// launch_agent, so the model picks a role by name and the schema spells out
// what each one is for. A preset bundles three things launch_agent leaves to
// the caller: a curated persona (appended to the child's system prompt), a
// read-only toolbelt switch, and an optional model override.
type presetAgent struct {
	// name is the LLM-facing tool name.
	name string
	// label is the default progress-UI description when the model omits one.
	label string
	// description is the tool description shown to the model.
	description string
	// persona is appended to the child's system prompt (SpawnRequest.SystemSuffix).
	persona string
	// readOnly drops write_file / edit_file from the child's toolbelt.
	readOnly bool
}

// presetAgents is the canonical set of built-in sub-agent types. Adding a
// preset here registers its tool automatically via the init() below.
var presetAgents = []presetAgent{
	{
		name:  "explore_agent",
		label: "Explore the codebase",
		description: "Spawn a read-only sub-agent to investigate and report on the codebase. " +
			"It can read files, grep, glob, run read-only shell commands, and use any " +
			"code-intelligence tools available, but cannot modify files. Use to locate code, " +
			"understand how subsystems connect, or answer a broad 'where/how does X work' " +
			"question without polluting this context. Runs asynchronously — you get a " +
			"notification with its findings when it completes. Returns a report, not a code change.",
		readOnly: true,
		persona: "You are a read-only exploration sub-agent. Your job is to locate and understand " +
			"code, then report findings — not to modify anything. Use read_file, grep, glob, " +
			"read-only terminal commands (git, find, ls), and any code-intelligence tools available. " +
			"Do NOT write or edit files. Deliverable: a concise report answering the task directly — " +
			"the relevant file paths with line numbers, how the pieces connect, and the minimal code " +
			"quoted to make the point. Don't dump whole files.",
	},
	{
		name:  "plan_agent",
		label: "Draft an implementation plan",
		description: "Spawn a read-only sub-agent that investigates the codebase and returns a " +
			"concrete, step-by-step implementation plan. It can read and search but cannot modify " +
			"files. Use when a task is well-defined enough to plan in isolation and you want a " +
			"dependency-ordered breakdown grounded in the real code. Runs asynchronously — you get " +
			"a notification with the plan when it completes.",
		readOnly: true,
		persona: "You are a planning sub-agent. Investigate the codebase read-only, then produce a " +
			"concrete, step-by-step implementation plan. Do NOT modify files. Deliverable: an ordered " +
			"plan — the files to change and what changes in each, in dependency order; the key design " +
			"decisions and trade-offs; risks and a test strategy. Ground every step in code you have " +
			"actually inspected (cite file:line). Do not write speculative steps you couldn't verify.",
	},
	{
		name:  "general_agent",
		label: "Handle a delegated task",
		description: "Spawn a general-purpose sub-agent with the full toolbelt (it can read, write, " +
			"edit, and run commands) to handle a focused, self-contained task end-to-end. Use when " +
			"the sub-task is well-defined enough to delegate without back-and-forth and you want it " +
			"done in a fresh context window. Runs asynchronously — you get a notification with the " +
			"result when it completes. For a read-only investigation prefer explore_agent; to restrict " +
			"the toolbelt explicitly, use launch_agent.",
		readOnly: false,
		persona: "You are an autonomous general-purpose sub-agent handling a delegated task end-to-end. " +
			"You have the full toolbelt. Complete the task, verify your work, and return a clear, " +
			"self-contained result the caller can act on without seeing your intermediate steps.",
	},
	{
		name:  "code_review_agent",
		label: "Review the changes",
		description: "Spawn a read-only sub-agent to review code changes for correctness bugs, " +
			"convention violations, performance issues, missing tests, and security problems. It can " +
			"run `git diff`/`git status`, read the touched files, and search, but cannot modify files. " +
			"Use after making changes or to review a diff. Runs asynchronously — you get a notification " +
			"with the findings when it completes.",
		readOnly: true,
		persona: "You are a code-review sub-agent. Review the changes — use `git diff`, `git status`, " +
			"and read the touched files — for correctness bugs, convention violations, performance " +
			"issues, missing tests, and security problems. Do NOT modify files. Deliverable: a " +
			"prioritized list of findings, each with file:line, a severity, what is wrong, and a " +
			"suggested fix. If you find nothing material, say so explicitly rather than inventing nits.",
	},
}

// init registers one PresetAgentTool per preset alongside the hand-written
// built-ins. allTools is already initialised when init runs (package-level var
// literals evaluate before init functions), so the append lands after the
// literal entries — the presets show up at the end of DefaultTools().
func init() {
	for _, p := range presetAgents {
		allTools = append(allTools, PresetAgentTool{spec: p})
	}
}

// PresetAgentTool is one named sub-agent type. It reuses the same async
// SubAgentManager.Start path as LaunchAgentTool — so the result arrives via a
// notification, agent_status/kill_agent/send_message all work on it — but fixes
// the persona and read-only toolbelt from its preset instead of taking them as
// LLM arguments. Only description and prompt remain caller-supplied.
type PresetAgentTool struct {
	spec presetAgent
	mgr  *SubAgentManager
}

func (t PresetAgentTool) manager() *SubAgentManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultSubAgentMgr
}

func (t PresetAgentTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        t.spec.name,
		Description: t.spec.description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "Short human-readable label for this sub-agent (3-7 words). Shown in progress UI; doesn't shape behavior.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task for the sub-agent. Self-contained: include all context it needs (file paths, constraints, the exact question) since it can't see this conversation.",
				},
			},
			"required": []string{"prompt"},
		},
	}
}

func (t PresetAgentTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if IsSubAgent(ctx) {
		// Same recursion guard as launch_agent — a sub-agent cannot spawn
		// another sub-agent, even via a preset tool.
		return agent.ToolResult{Text: ""}, fmt.Errorf("%s: a sub-agent cannot spawn another sub-agent", t.spec.name)
	}

	prompt := strings.TrimSpace(stringArg(input, "prompt"))
	if prompt == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("%s: prompt is required", t.spec.name)
	}
	desc := strings.TrimSpace(stringArg(input, "description"))
	if desc == "" {
		desc = t.spec.label
	}

	mgr := t.manager()
	if mgr == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("%s: sub-agent dispatch is not configured for this session", t.spec.name)
	}

	id, err := mgr.Start(SpawnRequest{
		Description:  desc,
		Prompt:       prompt,
		SystemSuffix: t.spec.persona,
		ReadOnly:     t.spec.readOnly,
	})
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("%s: %w", t.spec.name, err)
	}
	return agent.ToolResult{
		Text: fmt.Sprintf("Started %s %s. You will be notified when it completes.", t.spec.name, id),
	}, nil
}
