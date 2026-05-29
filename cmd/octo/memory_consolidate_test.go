package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/tools"
)

// stubConsolidationSpawner records what the consolidator handed to the sub-
// agent and replays a canned reply. It's just for the unit-level test —
// the real spawner is exercised by the M10 integration tests.
type stubConsolidationSpawner struct {
	reply string
	err   error

	called   bool
	lastReq  tools.SpawnRequest
	gotPrior bool // prompt mentioned the prior summary
	gotNotes bool // prompt mentioned the new notes
}

func (s *stubConsolidationSpawner) Spawn(_ context.Context, req tools.SpawnRequest) (tools.SpawnResult, error) {
	s.called = true
	s.lastReq = req
	if strings.Contains(req.Prompt, "PRIOR_SUMMARY_MARKER") {
		s.gotPrior = true
	}
	if strings.Contains(req.Prompt, "NEW_NOTES_MARKER") {
		s.gotNotes = true
	}
	if s.err != nil {
		return tools.SpawnResult{}, s.err
	}
	return tools.SpawnResult{Reply: s.reply}, nil
}

// Continue is unused by the consolidator (it only ever calls Spawn) but is
// required to satisfy tools.Spawner.
func (s *stubConsolidationSpawner) Continue(_ context.Context, _, _ string) (tools.SpawnResult, error) {
	return tools.SpawnResult{}, nil
}

func useConsolidationSpawner(t *testing.T, s tools.Spawner) {
	t.Helper()
	tools.SetSpawner(s)
	t.Cleanup(func() { tools.SetSpawner(nil) })
}

func TestConsolidateViaSubAgent_HappyPath(t *testing.T) {
	stub := &stubConsolidationSpawner{reply: "## Consolidated\n- one line\n- two lines"}
	useConsolidationSpawner(t, stub)

	out := consolidateViaSubAgent(context.Background(), memory.NewStoreAt(t.TempDir()), "", "PRIOR_SUMMARY_MARKER", "NEW_NOTES_MARKER")
	if out == "" {
		t.Fatalf("expected sub-agent path to return text, got empty")
	}
	if !strings.Contains(out, "Consolidated") {
		t.Errorf("unexpected sub-agent output: %q", out)
	}
	if !stub.called {
		t.Error("spawner was never called")
	}
	if !stub.gotPrior || !stub.gotNotes {
		t.Errorf("prompt should embed prior + new notes; gotPrior=%v gotNotes=%v", stub.gotPrior, stub.gotNotes)
	}
	if !sliceEquals(stub.lastReq.Tools, consolidationToolAllowlist) {
		t.Errorf("spawner request should restrict to %v, got %v", consolidationToolAllowlist, stub.lastReq.Tools)
	}
}

func TestConsolidateViaSubAgent_NoSpawner(t *testing.T) {
	tools.SetSpawner(nil)
	out := consolidateViaSubAgent(context.Background(), memory.NewStoreAt(t.TempDir()), "", "x", "y")
	if out != "" {
		t.Errorf("with no spawner, should return empty (caller falls back to side-call), got %q", out)
	}
}

func TestConsolidateViaSubAgent_SpawnerError(t *testing.T) {
	useConsolidationSpawner(t, &stubConsolidationSpawner{err: errors.New("provider down")})
	out := consolidateViaSubAgent(context.Background(), memory.NewStoreAt(t.TempDir()), "", "x", "y")
	if out != "" {
		t.Errorf("sub-agent error should be swallowed as empty (caller falls back), got %q", out)
	}
}

func TestConsolidateViaSubAgent_StripsCodeFence(t *testing.T) {
	stub := &stubConsolidationSpawner{
		reply: "```markdown\n- bullet a\n- bullet b\n```",
	}
	useConsolidationSpawner(t, stub)

	out := consolidateViaSubAgent(context.Background(), memory.NewStoreAt(t.TempDir()), "", "p", "n")
	if strings.Contains(out, "```") {
		t.Errorf("code fence should be stripped, got %q", out)
	}
	if !strings.Contains(out, "bullet a") {
		t.Errorf("body lost when stripping fence: %q", out)
	}
}

func TestConsolidateViaSubAgent_EmptyReply(t *testing.T) {
	useConsolidationSpawner(t, &stubConsolidationSpawner{reply: "   "})
	out := consolidateViaSubAgent(context.Background(), memory.NewStoreAt(t.TempDir()), "", "p", "n")
	if out != "" {
		t.Errorf("empty sub-agent reply should be reported as empty so caller can fall back, got %q", out)
	}
}

func TestBuildConsolidationPrompt_ContentShape(t *testing.T) {
	p := buildConsolidationPrompt("/tmp/mem", "/some/proj", "PRIOR", "NEW")
	for _, want := range []string{
		"cross-session memory",
		"/tmp/mem/MEMORY.md",
		"/some/proj", // project-scope line
		"PRIOR",
		"NEW",
		"read_file",
		"protocol marker",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestBuildConsolidationPrompt_HandlesEmptyPriorSummary(t *testing.T) {
	p := buildConsolidationPrompt("/tmp/mem", "", "", "NEW")
	if !strings.Contains(p, "first consolidation pass") {
		t.Errorf("empty prior summary should be labelled as first pass:\n%s", p)
	}
}

func sliceEquals(a, b []string) bool {
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
