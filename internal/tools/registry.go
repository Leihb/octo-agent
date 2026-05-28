package tools

import (
	"context"
	"fmt"

	"github.com/Leihb/octo-agent/internal/agent"
)

// tool is the internal interface every built-in tool implements — both a
// Definition (what the LLM sees) and an Execute (what the agent loop calls).
// External callers of the tools package only need agent.ToolExecutor; this
// interface is private so adding methods later doesn't break consumers.
type tool interface {
	Definition() agent.ToolDefinition
	Execute(ctx context.Context, name string, input map[string]any) (string, error)
}

// allTools is the canonical, ordered list of built-in tools shipped with
// octo-agent. Adding a tool means a single new entry here — the registry
// scan and the DefaultTools() listing both pick it up automatically.
var allTools = []tool{
	TerminalTool{},
	TerminalOutputTool{},
	ReadFileTool{},
	WriteFileTool{},
	EditFileTool{},
	GlobTool{},
	GrepTool{},
	WebFetchTool{},
	WebSearchTool{},
	SkillTool{},
	RememberTool{},
	LaunchAgentTool{},
}

// DefaultRegistry is the agent.ToolExecutor used when `octo chat --tools` is
// enabled. It dispatches each tool call by name to the matching entry in
// allTools, returning a clean error for unknown names.
//
// When tracker is non-nil it enforces read-before-write: write_file /
// edit_file calls to an existing file are refused unless the file was read
// (and is unchanged) this session. The zero value (DefaultRegistry{}) has a
// nil tracker and so enforces nothing — preserved for tests and callers that
// don't want the discipline. Use NewDefaultRegistry for the enforced variant.
type DefaultRegistry struct {
	tracker *ReadTracker
}

// NewDefaultRegistry returns a registry with read-before-write enforcement
// backed by a fresh per-session ReadTracker.
func NewDefaultRegistry() DefaultRegistry {
	return DefaultRegistry{tracker: NewReadTracker()}
}

// Execute implements agent.ToolExecutor.
func (r DefaultRegistry) Execute(ctx context.Context, name string, input map[string]any) (string, error) {
	// Read-before-write pre-check (skipped when no tracker is configured).
	if r.tracker != nil && (name == "write_file" || name == "edit_file") {
		if path, ok := input["path"].(string); ok {
			if abs, err := resolvePath(path); err == nil {
				if cerr := r.tracker.CheckWritable(abs); cerr != nil {
					return "", cerr
				}
			}
		}
	}

	for _, t := range allTools {
		if t.Definition().Name == name {
			out, err := t.Execute(ctx, name, input)
			// On a successful read OR write, (re)stamp the tracker so the
			// file is considered "read at its current mtime" — this lets a
			// write be followed by an edit without a redundant re-read.
			if err == nil && r.tracker != nil &&
				(name == "read_file" || name == "write_file" || name == "edit_file") {
				if path, ok := input["path"].(string); ok {
					if abs, rerr := resolvePath(path); rerr == nil {
						r.tracker.RecordRead(abs)
					}
				}
			}
			return out, err
		}
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// DefaultTools returns the slice of ToolDefinitions sent to the LLM when
// `--tools` is on. Order matches allTools. Each capability-gated tool is
// withheld unless the corresponding registration call has been made —
// SkillTool needs SetSkills, RememberTool needs SetMemoryStore, LaunchAgentTool
// needs SetSpawner. Advertising a tool that can only error wastes a slot and
// confuses the model.
func DefaultTools() []agent.ToolDefinition {
	skillsOn := skillsEnabled()
	memoryOn := memoryEnabled()
	spawnerOn := spawnerEnabled()
	defs := make([]agent.ToolDefinition, 0, len(allTools))
	for _, t := range allTools {
		if _, isSkill := t.(SkillTool); isSkill && !skillsOn {
			continue
		}
		if _, isRemember := t.(RememberTool); isRemember && !memoryOn {
			continue
		}
		if _, isLaunch := t.(LaunchAgentTool); isLaunch && !spawnerOn {
			continue
		}
		defs = append(defs, t.Definition())
	}
	return defs
}
