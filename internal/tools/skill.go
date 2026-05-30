package tools

import (
	"context"
	"fmt"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/skills"
)

// activeSkills, when non-nil and non-empty, backs the `skill` tool: it serves
// SKILL.md bodies on demand (progressive-disclosure L2). Set once at session
// start via SetSkills; mirrors the package-level activeSandbox/defaultBg.
var activeSkills *skills.Registry

// SetSkills registers the skills the `skill` tool serves and that DefaultTools
// uses to decide whether to advertise the tool. cmd/octo calls this at session
// start. Pass nil (or an empty registry) to disable.
func SetSkills(r *skills.Registry) { activeSkills = r }

// skillsEnabled reports whether any skill was discovered — the gate for both
// advertising and dispatching the skill tool.
func skillsEnabled() bool { return activeSkills != nil && activeSkills.Len() > 0 }

// SkillTool loads a skill's full SKILL.md body on demand. The model calls it
// after spotting a matching skill in the system-prompt "Available skills"
// manifest; the body returns as a tool_result, landing in history rather than
// the frozen system prefix. The zero value reads from the package-level
// registry set by SetSkills.
type SkillTool struct{}

func (SkillTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "skill",
		Description: "Load the full instructions for an available skill by name. Skills are " +
			"listed in the system prompt under \"Available skills\". Call this with a skill's " +
			"name to get its step-by-step instructions, then follow them using the other tools.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The skill name exactly as shown in the Available skills list.",
				},
			},
			"required": []string{"name"},
		},
	}
}

func (SkillTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	name, _ := input["name"].(string)
	if name == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("skill: name is required")
	}
	if !skillsEnabled() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("skill: no skills are available")
	}
	s, ok := activeSkills.Get(name)
	if !ok {
		return agent.ToolResult{Text: ""}, fmt.Errorf("skill: unknown skill %q", name)
	}
	return agent.ToolResult{Text: s.Body}, nil
}
