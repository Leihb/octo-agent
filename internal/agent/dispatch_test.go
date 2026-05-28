package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCanParallelize(t *testing.T) {
	ro := func(name string) toolCall { return toolCall{block: NewToolUseBlock("id", name, nil)} }

	cases := []struct {
		name  string
		calls []toolCall
		want  bool
	}{
		{"two read-only", []toolCall{ro("read_file"), ro("grep")}, true},
		{"three read-only", []toolCall{ro("read_file"), ro("grep"), ro("glob")}, true},
		{"single read-only", []toolCall{ro("read_file")}, false},
		{"mixed read-only + write", []toolCall{ro("read_file"), ro("write_file")}, false},
		{"mixed read-only + terminal", []toolCall{ro("grep"), ro("terminal")}, false},
		{"empty", nil, false},
		{
			"denied call ignored, remaining single → serial",
			[]toolCall{{block: NewToolUseBlock("a", "read_file", nil), denyReason: "no"}, ro("grep")},
			false,
		},
		{
			"denied call ignored, two executable read-only → parallel",
			[]toolCall{{block: NewToolUseBlock("a", "write_file", nil), denyReason: "no"}, ro("grep"), ro("glob")},
			true,
		},
		{
			// launch_agent is in the parallel-safe set even though sub-agents can
			// have side effects — the LLM is supposed to fan out unrelated
			// research/sub-tasks via this tool, and the dispatcher running them
			// concurrently is the whole point. See readOnlyTools' comment.
			"two launch_agent calls → parallel",
			[]toolCall{ro("launch_agent"), ro("launch_agent")},
			true,
		},
		{
			"launch_agent + read_file → parallel",
			[]toolCall{ro("launch_agent"), ro("read_file")},
			true,
		},
	}
	for _, tc := range cases {
		if got := canParallelize(tc.calls); got != tc.want {
			t.Errorf("%s: canParallelize = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// barrierExec proves concurrency: every Execute marks arrival then waits for
// ALL expected calls to arrive. If the dispatcher runs them serially the first
// call blocks forever (the others never start) and the test's timeout fires.
type barrierExec struct {
	wg    sync.WaitGroup
	mu    sync.Mutex
	order []string
}

func (b *barrierExec) Execute(_ context.Context, name string, _ map[string]any) (string, error) {
	b.mu.Lock()
	b.order = append(b.order, name)
	b.mu.Unlock()
	b.wg.Done() // arrived
	b.wg.Wait() // release only once everyone has arrived → requires concurrency
	return "ok:" + name, nil
}

func TestDispatchTools_ParallelReadOnly(t *testing.T) {
	exec := &barrierExec{}
	exec.wg.Add(3)
	blocks := []ContentBlock{
		NewToolUseBlock("c1", "read_file", map[string]any{"path": "a"}),
		NewToolUseBlock("c2", "grep", map[string]any{"pattern": "x"}),
		NewToolUseBlock("c3", "glob", map[string]any{"pattern": "*"}),
	}

	done := make(chan []ContentBlock, 1)
	go func() {
		r, _ := dispatchTools(context.Background(), exec, blocks, nil, nil)
		done <- r
	}()

	select {
	case results := <-done:
		// Results must be in block order even though execution was concurrent.
		want := []string{"c1", "c2", "c3"}
		if len(results) != 3 {
			t.Fatalf("got %d results, want 3", len(results))
		}
		for i, w := range want {
			if results[i].ToolUseID != w {
				t.Errorf("results[%d].ToolUseID = %q, want %q", i, results[i].ToolUseID, w)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read-only batch did not run concurrently — the arrival barrier never released (serial dispatch)")
	}
}

// countingExec is a concurrency-safe executor for the serial-path test.
type countingExec struct {
	mu     sync.Mutex
	called []string
}

func (c *countingExec) Execute(_ context.Context, name string, _ map[string]any) (string, error) {
	c.mu.Lock()
	c.called = append(c.called, name)
	c.mu.Unlock()
	return "ok:" + name, nil
}

func TestDispatchTools_MixedBatchSerialCorrect(t *testing.T) {
	// A write_file in the batch forces the serial path; results must still be
	// correct and in order.
	exec := &countingExec{}
	blocks := []ContentBlock{
		NewToolUseBlock("c1", "read_file", map[string]any{"path": "a"}),
		NewToolUseBlock("c2", "write_file", map[string]any{"path": "b", "content": "x"}),
		NewToolUseBlock("c3", "grep", map[string]any{"pattern": "y"}),
	}
	results, err := dispatchTools(context.Background(), exec, blocks, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []string{"c1", "c2", "c3"} {
		if results[i].ToolUseID != w {
			t.Errorf("results[%d] = %q, want %q", i, results[i].ToolUseID, w)
		}
		if results[i].Result != "ok:"+blocks[i].Name {
			t.Errorf("results[%d].Result = %q", i, results[i].Result)
		}
	}
}

// denyOneGate denies a specific tool name and allows everything else.
type denyOneGate struct{ deny string }

func (g denyOneGate) Check(_ context.Context, name string, _ map[string]any) (bool, string) {
	if name == g.deny {
		return false, "permission_denied: " + name
	}
	return true, ""
}

func TestDispatchTools_DeniedCallInParallelBatch(t *testing.T) {
	// All read-only (parallel-eligible), but the gate denies the grep. The
	// denied call yields an IsError result; the others still execute.
	exec := &countingExec{}
	gate := denyOneGate{deny: "grep"}
	blocks := []ContentBlock{
		NewToolUseBlock("c1", "read_file", map[string]any{"path": "a"}),
		NewToolUseBlock("c2", "grep", map[string]any{"pattern": "x"}),
		NewToolUseBlock("c3", "glob", map[string]any{"pattern": "*"}),
	}
	results, err := dispatchTools(context.Background(), exec, blocks, nil, gate)
	if err != nil {
		t.Fatal(err)
	}
	if !results[1].IsError || !strings.Contains(results[1].Result, "permission_denied") {
		t.Errorf("grep result should be a denial error, got %+v", results[1])
	}
	if results[0].IsError || results[2].IsError {
		t.Errorf("allowed calls should have executed: %+v / %+v", results[0], results[2])
	}
	// Executor saw only the two allowed calls.
	if len(exec.called) != 2 {
		t.Errorf("executor calls = %d, want 2 (grep was gated out)", len(exec.called))
	}
}
