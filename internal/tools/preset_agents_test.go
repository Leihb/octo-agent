package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// presetByName returns the registered PresetAgentTool with the given name.
func presetByName(t *testing.T, name string) PresetAgentTool {
	t.Helper()
	for _, tl := range allTools {
		if p, ok := tl.(PresetAgentTool); ok && p.spec.name == name {
			return p
		}
	}
	t.Fatalf("preset agent %q is not registered in allTools", name)
	return PresetAgentTool{}
}

func TestPresetAgents_Registered(t *testing.T) {
	want := []string{"explore_agent", "plan_agent", "general_agent", "code_review_agent"}
	for _, name := range want {
		p := presetByName(t, name)
		def := p.Definition()
		if def.Name != name {
			t.Errorf("Definition().Name = %q, want %q", def.Name, name)
		}
		props, _ := def.Parameters["properties"].(map[string]any)
		if _, ok := props["prompt"]; !ok {
			t.Errorf("%s schema missing 'prompt'", name)
		}
		// Presets fix the toolbelt and model themselves — those must not be
		// caller-tunable arguments like launch_agent's.
		if _, ok := props["tools"]; ok {
			t.Errorf("%s should not expose a 'tools' argument", name)
		}
		if _, ok := props["model"]; ok {
			t.Errorf("%s should not expose a 'model' argument", name)
		}
		required, _ := def.Parameters["required"].([]string)
		if !containsString(required, "prompt") {
			t.Errorf("%s should require 'prompt', got %v", name, required)
		}
	}
}

func TestPresetAgents_ReadOnlyFlags(t *testing.T) {
	cases := map[string]bool{
		"explore_agent":     true,
		"plan_agent":        true,
		"code_review_agent": true,
		"general_agent":     false,
	}
	for name, wantReadOnly := range cases {
		if got := presetByName(t, name).spec.readOnly; got != wantReadOnly {
			t.Errorf("%s readOnly = %v, want %v", name, got, wantReadOnly)
		}
	}
}

func TestPresetAgentTool_ForwardsPersonaAndReadOnly(t *testing.T) {
	spawnCalled := make(chan struct{})
	stub := &stubSpawner{
		replies:     []SpawnResult{{Reply: "ok"}},
		spawnCalled: spawnCalled,
	}
	mgr := NewSubAgentManager(stub)

	explore := presetByName(t, "explore_agent")
	explore.mgr = mgr

	out, err := explore.Execute(context.Background(), "explore_agent", map[string]any{
		"prompt": "Where is the SSE aggregator?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Started explore_agent") {
		t.Errorf("Execute returned %q, want 'Started explore_agent' prefix", out.Text)
	}

	select {
	case <-spawnCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Spawn to be called")
	}

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 spawner call, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	if !got.ReadOnly {
		t.Error("explore_agent should forward ReadOnly=true to the spawner")
	}
	if !strings.Contains(got.SystemSuffix, "read-only exploration") {
		t.Errorf("explore_agent persona not forwarded as SystemSuffix: %q", got.SystemSuffix)
	}
	// Description should default to the preset label when the caller omits one.
	if got.Description != explore.spec.label {
		t.Errorf("Description = %q, want default label %q", got.Description, explore.spec.label)
	}
}

func TestPresetAgentTool_PromptRequired(t *testing.T) {
	mgr := NewSubAgentManager(&stubSpawner{})
	p := presetByName(t, "plan_agent")
	p.mgr = mgr
	_, err := p.Execute(context.Background(), "plan_agent", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("expected 'prompt is required' error, got %v", err)
	}
}

func TestPresetAgentTool_RecursionRefused(t *testing.T) {
	mgr := NewSubAgentManager(&stubSpawner{})
	p := presetByName(t, "general_agent")
	p.mgr = mgr
	ctx := WithSubAgentMarker(context.Background())
	_, err := p.Execute(ctx, "general_agent", map[string]any{"prompt": "do a thing"})
	if err == nil || !strings.Contains(err.Error(), "cannot spawn") {
		t.Errorf("expected recursion refusal, got %v", err)
	}
}

func TestPresetAgentTool_NoManagerConfigured(t *testing.T) {
	// Bare value (nil mgr) with no default manager registered.
	SetDefaultSubAgentManager(nil)
	t.Cleanup(func() { SetDefaultSubAgentManager(nil) })
	p := presetByName(t, "code_review_agent")
	p.mgr = nil
	_, err := p.Execute(context.Background(), "code_review_agent", map[string]any{"prompt": "review the diff"})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestDefaultTools_PresetsGatedOnManager(t *testing.T) {
	SetSpawner(nil)
	SetDefaultSubAgentManager(nil)
	t.Cleanup(func() {
		SetSpawner(nil)
		SetDefaultSubAgentManager(nil)
	})

	present := func(name string) bool {
		for _, d := range DefaultTools() {
			if d.Name == name {
				return true
			}
		}
		return false
	}

	for _, name := range []string{"explore_agent", "plan_agent", "general_agent", "code_review_agent"} {
		if present(name) {
			t.Errorf("%s should be absent with no manager configured", name)
		}
	}
	SetDefaultSubAgentManager(NewSubAgentManager(&stubSpawner{}))
	for _, name := range []string{"explore_agent", "plan_agent", "general_agent", "code_review_agent"} {
		if !present(name) {
			t.Errorf("%s should be present once a SubAgentManager is registered", name)
		}
	}
}
