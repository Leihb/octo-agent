package tools

import "strings"

// agentPreset describes a named sub-agent type loaded from frontmatter or
// hard-coded as a built-in fallback.
type agentPreset struct {
	name        string
	description string
	persona     string
	readOnly    bool
}

// builtInPresets is the canonical set of built-in agent types. These are
// always available even when no user-defined agents are loaded.
var builtInPresets = []agentPreset{
	{
		name:        "explore",
		description: "Read-only exploration agent",
		readOnly:    true,
		persona: "You are a read-only exploration sub-agent. Your job is to locate and understand " +
			"code, then report findings — not to modify anything. Use read_file, grep, glob, " +
			"read-only terminal commands (git, find, ls), and any code-intelligence tools available. " +
			"Do NOT write or edit files. Deliverable: a concise report answering the task directly — " +
			"the relevant file paths with line numbers, how the pieces connect, and the minimal code " +
			"quoted to make the point. Don't dump whole files.",
	},
	{
		name:        "plan",
		description: "Read-only planning agent",
		readOnly:    true,
		persona: "You are a planning sub-agent. Investigate the codebase read-only, then produce a " +
			"concrete, step-by-step implementation plan. Do NOT modify files. Deliverable: an ordered " +
			"plan — the files to change and what changes in each, in dependency order; the key design " +
			"decisions and trade-offs; risks and a test strategy. Ground every step in code you have " +
			"actually inspected (cite file:line). Do not write speculative steps you couldn't verify.",
	},
	{
		name:        "general",
		description: "General-purpose agent with full toolbelt",
		readOnly:    false,
		persona: "You are an autonomous general-purpose sub-agent handling a delegated task end-to-end. " +
			"You have the full toolbelt. Complete the task, verify your work, and return a clear, " +
			"self-contained result the caller can act on without seeing your intermediate steps.",
	},
	{
		name:        "code-review",
		description: "Read-only code review agent",
		readOnly:    true,
		persona: "You are a code-review sub-agent. Review the changes — use `git diff`, `git status`, " +
			"and read the touched files — for correctness bugs, convention violations, performance " +
			"issues, missing tests, and security problems. Do NOT modify files. Deliverable: a " +
			"prioritized list of findings, each with file:line, a severity, what is wrong, and a " +
			"suggested fix. If you find nothing material, say so explicitly rather than inventing nits.",
	},
}

// lookupAgentPreset resolves a subagent_type name to its preset.
// User-defined agents (loaded from ~/.octo/agents/*.md) are checked first so
// they override built-ins when names collide.
func lookupAgentPreset(name string) (agentPreset, bool) {
	discoverAgents() // cheap: one directory scan, populated into cache

	discoveredAgentsMu.RLock()
	if p, ok := discoveredAgents[name]; ok {
		discoveredAgentsMu.RUnlock()
		return p, true
	}
	discoveredAgentsMu.RUnlock()

	for _, p := range builtInPresets {
		if p.name == name {
			return p, true
		}
	}
	return agentPreset{}, false
}

// listPresetNames returns a comma-separated list of available preset names.
// User-defined names are included alongside built-ins.
func listPresetNames() string {
	discoverAgents()

	discoveredAgentsMu.RLock()
	names := make([]string, 0, len(discoveredAgents)+len(builtInPresets))
	for n := range discoveredAgents {
		names = append(names, n)
	}
	discoveredAgentsMu.RUnlock()

	// Add built-ins that aren't already overridden by a user agent.
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	for _, p := range builtInPresets {
		if !seen[p.name] {
			names = append(names, p.name)
		}
	}
	return strings.Join(names, ", ")
}
