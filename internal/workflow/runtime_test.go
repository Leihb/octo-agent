package workflow

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echoAgent replies "reply<prompt>" with a fixed token cost.
func echoAgent(_ context.Context, prompt string) AgentResult {
	return AgentResult{Reply: "reply<" + prompt + ">", InputTokens: 5, OutputTokens: 7}
}

func TestRun_AgentRoundTrip(t *testing.T) {
	got, err := Run(context.Background(),
		`a = agent("hi"); "got: #{a}"`,
		Options{Agent: echoAgent})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Output != "got: reply<hi>" {
		t.Errorf("Output = %q", got.Output)
	}
	if got.OutputTokens != 7 || got.InputTokens != 5 {
		t.Errorf("usage = in %d out %d, want 5/7", got.InputTokens, got.OutputTokens)
	}
}

// TestRun_ParallelConcurrent proves real concurrency by counting how many
// agents are in flight at once — robust against wazero/race startup overhead
// (which a wall-clock ceiling is not). With no MaxConcurrent, all n branches
// should overlap, and results must come back in input order.
func TestRun_ParallelConcurrent(t *testing.T) {
	const n = 5
	var inFlight, peak int64
	track := func(_ context.Context, p string) AgentResult {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond) // hold so siblings pile up
		atomic.AddInt64(&inFlight, -1)
		return AgentResult{Reply: "r<" + p + ">"}
	}
	got, err := Run(context.Background(),
		`parallel([1,2,3,4,5]) { |i| agent("t#{i}") }.join(",")`,
		Options{Agent: track})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Output != "r<t1>,r<t2>,r<t3>,r<t4>,r<t5>" {
		t.Errorf("Output = %q", got.Output)
	}
	if peak != n {
		t.Errorf("peak in-flight = %d, want %d (all branches concurrent)", peak, n)
	}
}

func TestRun_MaxConcurrent(t *testing.T) {
	var inFlight, peak int64
	track := func(_ context.Context, p string) AgentResult {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return AgentResult{Reply: p}
	}
	_, err := Run(context.Background(),
		`parallel([1,2,3,4,5,6,7,8]) { |i| agent("x") }.size.to_s`,
		Options{Agent: track, MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2 (MaxConcurrent)", peak)
	}
}

func TestRun_Pipeline(t *testing.T) {
	// stage1 echoes; stage2 appends the stage1 result length.
	agent := func(_ context.Context, p string) AgentResult {
		return AgentResult{Reply: p + "!"}
	}
	script := `
		s1 = ->(x) { agent("a#{x}") }
		s2 = ->(prev) { "#{prev}/len#{prev.length}" }
		pipeline([1,2], s1, s2).join(" | ")
	`
	got, err := Run(context.Background(), script, Options{Agent: agent})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// s1(1) -> "aa1!"? no: agent("a1") -> "a1!" (len 3); s2 -> "a1!/len3"
	want := "a1!/len3 | a2!/len3"
	if got.Output != want {
		t.Errorf("Output = %q, want %q", got.Output, want)
	}
}

func TestRun_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocked := func(c context.Context, _ string) AgentResult {
		<-c.Done() // never returns until canceled
		return AgentResult{Err: c.Err()}
	}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res, err := Run(ctx,
		`parallel([1,2,3]) { |i| agent("x") }.join`,
		Options{Agent: blocked})
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !res.Canceled {
		t.Error("Result.Canceled should be true")
	}
}

func TestRun_BudgetExhausted(t *testing.T) {
	// First agent spends 100 output tokens; Budget is 50, so the budget is
	// already over after one call and the next agent() raises.
	costly := func(_ context.Context, _ string) AgentResult {
		return AgentResult{Reply: "ok", OutputTokens: 100}
	}
	script := `
		agent("first")
		begin
			agent("second")
			"no-raise"
		rescue => e
			"raised: #{e.message}"
		end
	`
	got, err := Run(context.Background(), script, Options{Agent: costly, Budget: 50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(got.Output, "budget exhausted") {
		t.Errorf("Output = %q, want budget-exhausted raise", got.Output)
	}
}

func TestRun_ProgressLifecycle(t *testing.T) {
	var prog []string
	_, err := Run(context.Background(),
		`parallel([1,2]) { |i| agent("task-#{i}") }.join`,
		Options{
			Agent:    echoAgent,
			Progress: func(s string) { prog = append(prog, s) },
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two starts (→) and two finishes (✓), labels carrying the prompt.
	var starts, dones int
	for _, p := range prog {
		switch {
		case strings.HasPrefix(p, "→ "):
			starts++
		case strings.HasPrefix(p, "✓ "):
			dones++
		}
	}
	if starts != 2 || dones != 2 {
		t.Errorf("progress = %v; want 2 starts + 2 dones", prog)
	}
}

func TestRun_Log(t *testing.T) {
	var lines []string
	got, err := Run(context.Background(),
		`log("starting"); log("done"); "ok"`,
		Options{Agent: echoAgent, Log: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Output != "ok" {
		t.Errorf("Output = %q", got.Output)
	}
	if len(lines) != 2 || lines[0] != "starting" || lines[1] != "done" {
		t.Errorf("log lines = %v", lines)
	}
}

func TestRun_ExceptionHandling(t *testing.T) {
	// Exercises mruby begin/rescue (setjmp/longjmp via the wasm EH proposal).
	got, err := Run(context.Background(),
		`begin; raise "boom"; rescue => e; "caught: #{e.message}"; end`,
		Options{Agent: echoAgent})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Output != "caught: boom" {
		t.Errorf("Output = %q", got.Output)
	}
}

func TestRun_ScriptError(t *testing.T) {
	_, err := Run(context.Background(),
		`this_method_does_not_exist(42)`,
		Options{Agent: echoAgent})
	if err == nil || !strings.Contains(err.Error(), "script error") {
		t.Errorf("err = %v, want script error", err)
	}
}

func TestRun_RequiresAgent(t *testing.T) {
	_, err := Run(context.Background(), `"x"`, Options{})
	if err == nil || !strings.Contains(err.Error(), "Agent is required") {
		t.Errorf("err = %v, want Agent-required", err)
	}
}

// Ensure distinct prompts flow through correctly under concurrency.
func TestRun_ParallelResultsMatchOrder(t *testing.T) {
	got, err := Run(context.Background(),
		`parallel((1..20).to_a) { |i| agent(i.to_s) }.join(",")`,
		Options{Agent: echoAgent})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var want []string
	for i := 1; i <= 20; i++ {
		want = append(want, fmt.Sprintf("reply<%d>", i))
	}
	if got.Output != strings.Join(want, ",") {
		t.Errorf("Output = %q", got.Output)
	}
}
