package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// tool is the internal interface every built-in tool implements — both a
// Definition (what the LLM sees) and an Execute (what the agent loop calls).
// External callers of the tools package only need agent.ToolExecutor; this
// interface is private so adding methods later doesn't break consumers.
type tool interface {
	Definition() agent.ToolDefinition
	Execute(ctx context.Context, name string, input map[string]any) (agent.ToolResult, error)
}

// allTools is the canonical, ordered list of built-in tools shipped with
// octo-agent. Adding a tool means a single new entry here — the registry
// scan and the DefaultTools() listing both pick it up automatically.
var allTools = []tool{
	TerminalTool{},
	TerminalOutputTool{},
	KillShellTool{},
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
	SendMessageTool{},
	AskUserQuestionTool{},
	TaskCreateTool{},
	TaskUpdateTool{},
	TaskListTool{},
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
func (r DefaultRegistry) Execute(ctx context.Context, name string, input map[string]any) (agent.ToolResult, error) {
	// MCP tools land here too — route them first so an "mcp__…" name
	// never falls through to the unknown-tool path. executeMCP returns
	// ok=false when the name isn't ours, then dispatch continues below.
	if out, ok, err := executeMCP(ctx, name, input); ok {
		return agent.ToolResult{Text: out}, err
	}

	// Read-before-write pre-check (skipped when no tracker is configured).
	if r.tracker != nil && (name == "write_file" || name == "edit_file") {
		if path, ok := input["path"].(string); ok {
			if abs, err := resolvePath(path); err == nil {
				if cerr := r.tracker.CheckWritable(abs); cerr != nil {
					return agent.ToolResult{}, cerr
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
			if err == nil && r.tracker != nil {
				switch name {
				case "read_file", "write_file", "edit_file":
					if path, ok := input["path"].(string); ok {
						if abs, rerr := resolvePath(path); rerr == nil {
							r.tracker.RecordRead(abs)
						}
					}
				case "terminal":
					if cmd, ok := input["command"].(string); ok {
						r.recordTerminalReads(cmd)
					}
				}
			}
			return out, err
		}
	}
	return agent.ToolResult{Text: ""}, fmt.Errorf("unknown tool %q", name)
}

// recordTerminalReads parses a successful terminal command for common file-
// reading operations (cat, head, tail, grep, etc.) and records the referenced
// paths in the read tracker. This closes the gap where `cat file` followed by
// `write_file file` would wrongly fail with "File has not been read yet".
//
// The parser is intentionally lightweight: it tokenises the command string,
// skips flags and shell metacharacters, and treats any remaining token that
// resolves to an existing file as a read. It does NOT understand subshells,
// variable expansion, or complex pipelines — those still require an explicit
// read_file if the model wants to edit afterwards.
func (r DefaultRegistry) recordTerminalReads(command string) {
	// Commands that are known to read files as positional arguments.
	readCmds := map[string]bool{
		"cat": true, "head": true, "tail": true, "less": true, "more": true,
		"grep": true, "rg": true, "awk": true, "sed": true, "wc": true,
		"sort": true, "uniq": true, "diff": true, "comm": true, "cmp": true,
		"md5sum": true, "sha256sum": true, "shasum": true, "file": true,
		"stat": true, "ls": true, "find": true, "readlink": true,
	}

	tokens := tokenizeCommand(command)
	if len(tokens) == 0 {
		return
	}
	if !readCmds[tokens[0]] {
		return
	}

	for _, tok := range tokens[1:] {
		if strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, ">") || strings.HasPrefix(tok, "<") {
			continue // skip flags and redirection operators
		}
		if strings.ContainsAny(tok, "|;&$()`") {
			continue // skip tokens with shell metacharacters
		}
		// Try to resolve as a path.  If the file exists, record it.
		abs, err := resolvePath(tok)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			r.tracker.RecordRead(abs)
		}
	}
}

// tokenizeCommand splits a shell command line into whitespace-separated tokens,
// respecting single and double quotes (no nesting, no escape sequences).
func tokenizeCommand(s string) []string {
	var tokens []string
	var cur strings.Builder
	var inSingle, inDouble bool

	for _, r := range s {
		switch r {
		case '\'':
			if inDouble {
				cur.WriteRune(r)
			} else {
				inSingle = !inSingle
			}
		case '"':
			if inSingle {
				cur.WriteRune(r)
			} else {
				inDouble = !inDouble
			}
		case ' ', '\t':
			if inSingle || inDouble {
				cur.WriteRune(r)
			} else {
				if cur.Len() > 0 {
					tokens = append(tokens, cur.String())
					cur.Reset()
				}
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
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
	askerOn := askerEnabled()
	tasksOn := tasksEnabled()
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
		if _, isSend := t.(SendMessageTool); isSend && !spawnerOn {
			continue
		}
		if _, isAsk := t.(AskUserQuestionTool); isAsk && !askerOn {
			continue
		}
		if _, isTaskCreate := t.(TaskCreateTool); isTaskCreate && !tasksOn {
			continue
		}
		if _, isTaskUpdate := t.(TaskUpdateTool); isTaskUpdate && !tasksOn {
			continue
		}
		if _, isTaskList := t.(TaskListTool); isTaskList && !tasksOn {
			continue
		}
		defs = append(defs, t.Definition())
	}
	// MCP-advertised surfaces ride alongside built-ins, gated on a non-nil
	// registry registered via SetMCPRegistry. Synthesised per-connection so
	// each server's tools/resources/prompts surface together.
	defs = append(defs, mcpToolDefs()...)
	return defs
}
