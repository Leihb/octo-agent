package main

import (
	"context"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// agentSpawner implements tools.Spawner by building a child agent on each
// launch_agent call. The child shares the parent's Sender (one provider
// connection) and System (same harness identity), but runs in isolation:
// fresh History, no visibility into the parent's conversation, its own
// loop budget. The final child reply text is returned to the parent as the
// launch_agent tool_result; the child's token usage is rolled into the
// parent's session totals so /cost reports one consolidated number.
//
// toolsFn is a deferred lookup of the LLM-facing tool catalog (DefaultTools).
// Resolving it on each Spawn — rather than capturing a slice at construction
// — lets cmd/octo set up the spawner before computing the tool list, since
// SetSpawner has to run first for launch_agent to appear in DefaultTools().
type agentSpawner struct {
	parent   *agent.Agent
	executor agent.ToolExecutor
	toolsFn  func() []agent.ToolDefinition
}

func newAgentSpawner(parent *agent.Agent, executor agent.ToolExecutor, toolsFn func() []agent.ToolDefinition) *agentSpawner {
	return &agentSpawner{parent: parent, executor: executor, toolsFn: toolsFn}
}

// childMaxTurns caps the sub-agent's tool loop. Smaller than the parent's
// default — sub-agents are meant for focused sub-tasks, not free-form work,
// and a runaway child can otherwise burn through budget unnoticed.
const childMaxTurns = 12

// Spawn implements tools.Spawner.
func (s *agentSpawner) Spawn(ctx context.Context, req tools.SpawnRequest) (tools.SpawnResult, error) {
	childTools := filterChildTools(s.toolsFn(), req.Tools)

	model := req.Model
	if model == "" {
		model = s.parent.Model
	}

	child := agent.New(s.parent.Sender, model)
	child.System = s.parent.System // share harness identity (base + soul + env + skills + memory + …)
	child.MaxTokens = s.parent.MaxTokens
	child.Gate = s.parent.Gate
	child.MaxTurns = childMaxTurns
	// Inherit the parent's hard cost ceiling — the child's spend is rolled
	// back into the parent below, but the per-loop check inside the child
	// uses its own counter, so a budget the user already set on the parent
	// should still constrain the child.
	child.MaxCostUSD = s.parent.MaxCostUSD

	// Mark the context so the launch_agent tool refuses recursive calls
	// even if a hallucinating model bypasses the empty-tools filter below.
	childCtx := tools.WithSubAgentMarker(ctx)

	reply, err := child.Run(childCtx, req.Prompt, childTools, s.executor)
	if err != nil {
		return tools.SpawnResult{}, err
	}

	in, out := child.SessionTokens()
	s.parent.AccrueChildUsage(in, out)

	return tools.SpawnResult{
		Reply:        reply.Content,
		InputTokens:  in,
		OutputTokens: out,
	}, nil
}

// filterChildTools drops launch_agent (no recursion) and, when allowed is
// non-empty, intersects with that allowlist so the parent can hand the child
// a restricted toolbelt (e.g. read-only research).
func filterChildTools(parent []agent.ToolDefinition, allowed []string) []agent.ToolDefinition {
	var allowSet map[string]bool
	if len(allowed) > 0 {
		allowSet = make(map[string]bool, len(allowed))
		for _, a := range allowed {
			allowSet[a] = true
		}
	}
	out := make([]agent.ToolDefinition, 0, len(parent))
	for _, td := range parent {
		if td.Name == "launch_agent" {
			continue
		}
		if allowSet != nil && !allowSet[td.Name] {
			continue
		}
		out = append(out, td)
	}
	return out
}
