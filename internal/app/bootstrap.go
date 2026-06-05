package app

import (
	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
)

// ToolEnv is the wired tool environment for a session: the executor the agent
// loop dispatches tool calls through, the manager that tracks async sub-agents
// (sub_agent), and a model-aware tool-list function. Call
// ToolsFor again after MCP connects so the list (or its Tool Search bridge)
// picks up the new surface.
type ToolEnv struct {
	Executor    tools.DefaultRegistry
	SubAgentMgr *tools.SubAgentManager
	ToolsFor    func() []agent.ToolDefinition
}

// WireTools sets up the tool environment every full-loop entry point shares —
// the CLI today, the HTTP server and IM bridge as they migrate. It builds a
// read-before-write executor, registers the sub-agent spawner globally (so
// sub_agent appears in the catalog, which is why SetSpawner must run before
// any DefaultTools call), creates the async sub-agent manager, and — when
// enableTasks — installs the session task store.
//
// It returns the wired environment and a cleanup that resets the process-global
// registrations (spawner, task store); defer it for the session's lifetime. The
// caller still owns the agent's Sender, System prompt, knobs, gate, asker, and
// MCP connection strategy — those legitimately differ per entry point.
func WireTools(a *agent.Agent, enableTasks bool) (ToolEnv, func()) {
	executor := tools.NewDefaultRegistry()
	toolsFor := func() []agent.ToolDefinition { return tools.DefaultToolsFor(a.Model) }

	spawner := NewSpawner(a, executor, toolsFor)
	tools.SetSpawner(spawner)
	mgr := tools.NewSubAgentManager(spawner)
	tools.SetDefaultSubAgentManager(mgr)

	cleanup := func() {
		tools.SetDefaultSubAgentManager(nil)
		tools.SetSpawner(nil)
	}
	if enableTasks {
		tools.SetTaskStore(tasks.New())
		prev := cleanup
		cleanup = func() {
			prev()
			tools.SetTaskStore(nil)
		}
	}

	return ToolEnv{Executor: executor, SubAgentMgr: mgr, ToolsFor: toolsFor}, cleanup
}
