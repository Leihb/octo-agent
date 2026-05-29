package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubSpawner records the requests it received and replays canned responses
// in order (or repeats the last entry). Concurrent-safe so the parallel
// dispatch test can fan out multiple Spawn calls.
type stubSpawner struct {
	mu       sync.Mutex
	replies  []SpawnResult
	calls    []SpawnRequest
	delay    time.Duration // optional sleep so two concurrent calls actually overlap
	err      error
	spawnCnt int32

	// Continue support.
	continueReply SpawnResult
	continueErr   error
	contAgentID   string
	contMessage   string
}

func (s *stubSpawner) Spawn(_ context.Context, req SpawnRequest) (SpawnResult, error) {
	atomic.AddInt32(&s.spawnCnt, 1)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.err != nil {
		return SpawnResult{}, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if len(s.replies) == 0 {
		return SpawnResult{}, nil
	}
	if len(s.calls) <= len(s.replies) {
		return s.replies[len(s.calls)-1], nil
	}
	return s.replies[len(s.replies)-1], nil
}

func (s *stubSpawner) Continue(_ context.Context, agentID, message string) (SpawnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contAgentID = agentID
	s.contMessage = message
	if s.continueErr != nil {
		return SpawnResult{}, s.continueErr
	}
	return s.continueReply, nil
}

func useSpawner(t *testing.T, s Spawner) {
	t.Helper()
	SetSpawner(s)
	t.Cleanup(func() { SetSpawner(nil) })
}

func TestLaunchAgentTool_Schema(t *testing.T) {
	def := LaunchAgentTool{}.Definition()
	if def.Name != "launch_agent" {
		t.Errorf("Name = %q", def.Name)
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, want := range []string{"description", "prompt", "tools", "model"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema missing property %q", want)
		}
	}
	required, _ := def.Parameters["required"].([]string)
	if !containsString(required, "description") || !containsString(required, "prompt") {
		t.Errorf("description and prompt should be required, got %v", required)
	}
}

func TestLaunchAgentTool_Execute(t *testing.T) {
	useSpawner(t, &stubSpawner{
		replies: []SpawnResult{{Reply: "sub-agent finding", InputTokens: 100, OutputTokens: 50}},
	})

	out, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "Investigate X",
		"prompt":      "Find every reference to FooBar.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "sub-agent finding" {
		t.Errorf("Execute returned %q, want %q", out, "sub-agent finding")
	}
}

func TestLaunchAgentTool_PassesArgsThroughToSpawner(t *testing.T) {
	stub := &stubSpawner{replies: []SpawnResult{{Reply: "ok"}}}
	useSpawner(t, stub)

	_, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "Audit",
		"prompt":      "Run a security audit.",
		"tools":       []any{"read_file", "grep"},
		"model":       "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 spawner call, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	if got.Description != "Audit" || got.Prompt != "Run a security audit." || got.Model != "claude-haiku-4-5" {
		t.Errorf("spawner request mismatch: %+v", got)
	}
	if len(got.Tools) != 2 || got.Tools[0] != "read_file" || got.Tools[1] != "grep" {
		t.Errorf("tools allowlist not forwarded: %v", got.Tools)
	}
}

func TestLaunchAgentTool_DescriptionDefaultsFromPrompt(t *testing.T) {
	stub := &stubSpawner{replies: []SpawnResult{{Reply: "ok"}}}
	useSpawner(t, stub)

	_, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"prompt": "Examine the cache invalidation logic.\nReport back.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls[0].Description == "" {
		t.Error("missing description should fall back to first prompt line")
	}
}

func TestLaunchAgentTool_RecursionRefused(t *testing.T) {
	useSpawner(t, &stubSpawner{replies: []SpawnResult{{Reply: "would have worked"}}})

	// Mark the context as already inside a sub-agent.
	ctx := WithSubAgentMarker(context.Background())
	_, err := LaunchAgentTool{}.Execute(ctx, "launch_agent", map[string]any{
		"description": "x",
		"prompt":      "y",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot spawn") {
		t.Errorf("expected recursion refusal, got %v", err)
	}
}

func TestLaunchAgentTool_NoSpawnerConfigured(t *testing.T) {
	SetSpawner(nil)
	_, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "x",
		"prompt":      "y",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestLaunchAgentTool_PromptRequired(t *testing.T) {
	useSpawner(t, &stubSpawner{})
	_, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("expected 'prompt is required' error, got %v", err)
	}
}

func TestLaunchAgentTool_EmptyReplySurfacedExplicitly(t *testing.T) {
	useSpawner(t, &stubSpawner{replies: []SpawnResult{{Reply: "   "}}})

	out, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "silent run",
		"prompt":      "do nothing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "produced no reply") {
		t.Errorf("empty reply should be surfaced explicitly, got %q", out)
	}
}

func TestLaunchAgentTool_SpawnerErrorPropagates(t *testing.T) {
	useSpawner(t, &stubSpawner{err: errors.New("provider unreachable")})
	_, err := LaunchAgentTool{}.Execute(context.Background(), "launch_agent", map[string]any{
		"description": "x",
		"prompt":      "y",
	})
	if err == nil || !strings.Contains(err.Error(), "provider unreachable") {
		t.Errorf("expected propagated spawner error, got %v", err)
	}
}

func TestDefaultTools_LaunchAgentGatedOnSpawner(t *testing.T) {
	SetSpawner(nil)
	t.Cleanup(func() { SetSpawner(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "launch_agent" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("launch_agent should be absent when no spawner is configured")
	}
	useSpawner(t, &stubSpawner{})
	if !has() {
		t.Error("launch_agent should be present once a spawner is registered")
	}
}

func TestStringSliceArg(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"missing", nil, nil},
		{"strings slice", []string{"a", "b"}, []string{"a", "b"}},
		{"any slice", []any{"a", "b"}, []string{"a", "b"}},
		{"any slice with junk", []any{"a", 42, "", "b"}, []string{"a", "b"}},
		{"wrong type", "not-a-slice", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{}
			if tc.in != nil {
				input["k"] = tc.in
			}
			got := stringSliceArg(input, "k")
			if !sameStrings(got, tc.want) {
				t.Errorf("stringSliceArg = %v, want %v", got, tc.want)
			}
		})
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
