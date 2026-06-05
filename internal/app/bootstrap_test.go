package app

import (
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

func TestWireTools_SetsUpEnvAndCleansUp(t *testing.T) {
	// Reset the process globals afterwards regardless of what the test asserts.
	t.Cleanup(func() { tools.SetSpawner(nil); tools.SetTaskStore(nil) })

	a := agent.New(sender{p: &mockProvider{}}, "claude-haiku-4-5")
	env, cleanup := WireTools(a, true)

	if tools.ActiveSpawner() == nil {
		t.Error("WireTools should register a spawner so sub_agent appears in the catalog")
	}
	if tools.ActiveTaskStore() == nil {
		t.Error("WireTools(enableTasks=true) should install a task store")
	}
	if env.SubAgentMgr == nil {
		t.Error("WireTools should return a sub-agent manager")
	}
	// The sub-agent manager must be registered globally so that DefaultToolsFor
	// advertises the sub_agent tool to the LLM.
	hasSubAgent := false
	for _, d := range env.ToolsFor() {
		if d.Name == "sub_agent" {
			hasSubAgent = true
			break
		}
	}
	if !hasSubAgent {
		t.Error("WireTools should advertise the sub_agent tool in the catalog")
	}
	if env.ToolsFor == nil {
		t.Fatal("WireTools should return a tool-list function")
	}
	// ToolsFor is model-aware and must at least return the core built-ins.
	if len(env.ToolsFor()) == 0 {
		t.Error("ToolsFor returned an empty tool list")
	}

	cleanup()
	if tools.ActiveSpawner() != nil {
		t.Error("cleanup should reset the spawner")
	}
	if tools.ActiveTaskStore() != nil {
		t.Error("cleanup should reset the task store")
	}
	// After cleanup, the sub_agent tool should no longer be advertised.
	for _, d := range env.ToolsFor() {
		if d.Name == "sub_agent" {
			t.Error("cleanup should reset the sub-agent manager so sub_agent is no longer advertised")
			break
		}
	}
}

func TestWireTools_TasksDisabled(t *testing.T) {
	t.Cleanup(func() { tools.SetSpawner(nil); tools.SetTaskStore(nil) })

	a := agent.New(sender{p: &mockProvider{}}, "claude-haiku-4-5")
	_, cleanup := WireTools(a, false)
	defer cleanup()

	if tools.ActiveTaskStore() != nil {
		t.Error("WireTools(enableTasks=false) must not install a task store")
	}
	if tools.ActiveSpawner() == nil {
		t.Error("spawner should still be registered when tasks are disabled")
	}
}
