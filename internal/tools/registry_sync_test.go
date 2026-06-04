package tools

import "testing"

func advertisedNames() map[string]bool {
	m := map[string]bool{}
	for _, d := range DefaultToolsFor("") {
		m[d.Name] = true
	}
	return m
}

// TestSyncModeWithholdsStatusKill verifies a synchronous manager (server / IM)
// advertises launch_agent + send_message but withholds agent_status /
// kill_agent — those have nothing to act on once sub-agents run inline.
func TestSyncModeWithholdsStatusKill(t *testing.T) {
	mgr := NewSubAgentManager(&mockSpawner{})
	mgr.SetSynchronous(true)
	SetDefaultSubAgentManager(mgr)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })

	names := advertisedNames()
	if !names["launch_agent"] || !names["send_message"] {
		t.Error("launch_agent and send_message should be advertised in sync mode")
	}
	if names["agent_status"] || names["kill_agent"] {
		t.Error("agent_status / kill_agent should be withheld in sync mode")
	}
}

// TestAsyncModeAdvertisesStatusKill verifies the interactive (async) default
// still advertises the full set, including agent_status / kill_agent.
func TestAsyncModeAdvertisesStatusKill(t *testing.T) {
	mgr := NewSubAgentManager(&mockSpawner{}) // async
	SetDefaultSubAgentManager(mgr)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })

	names := advertisedNames()
	if !names["agent_status"] || !names["kill_agent"] {
		t.Error("agent_status / kill_agent should be advertised in async mode")
	}
}
