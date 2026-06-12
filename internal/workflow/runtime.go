// Package workflow runs a Ruby (mruby) DSL for deterministic agent
// orchestration. The interpreter is mruby compiled to wasm32-wasi (embedded as
// mruby.wasm) executed by the pure-Go wazero runtime — no cgo, so the binary
// stays CGO_ENABLED=0 and cross-compiles unchanged.
//
// The DSL surface (agent / parallel / pipeline / log) is a Ruby prelude
// (prelude.rb) prepended to the user's script. Its primitives call host
// functions implemented here, which back onto an AgentFunc — in production the
// sub-agent Spawner, in tests a fake. parallel/pipeline get real concurrency
// from mruby Fibers cooperating with goroutines: agent() starts work in a
// goroutine and yields its fiber; the Ruby event loop starts every branch
// before awaiting any.
package workflow

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

//go:embed mruby.wasm
var mrubyWasm []byte

//go:embed prelude.rb
var preludeRB string

// AgentFunc runs one sub-agent task to completion. It backs the DSL's agent()
// primitive. Implementations must be safe for concurrent calls on distinct
// prompts (parallel() invokes it from multiple goroutines at once).
type AgentFunc func(ctx context.Context, prompt string) AgentResult

// AgentResult is one agent() call's outcome.
type AgentResult struct {
	Reply        string
	InputTokens  int
	OutputTokens int
	Err          error
}

// Options configures one workflow run.
type Options struct {
	// Agent backs agent(). Required.
	Agent AgentFunc
	// Log backs the script's log() calls; nil discards.
	Log func(string)
	// Progress, when set, receives human-readable lifecycle lines as the
	// workflow runs ("→ <label>" when an agent starts, "✓ <label>" when it
	// finishes). Lets a streaming caller surface live progress. Distinct from
	// Log, which is the script's own output. Invoked only from the single VM
	// goroutine, so it need not be concurrency-safe.
	Progress func(string)
	// Budget caps total output tokens across all agent() calls; <= 0 = unlimited.
	// When the budget is already spent, agent() raises in the script.
	Budget int64
	// MaxConcurrent caps in-flight agent() calls; <= 0 = unlimited.
	MaxConcurrent int
}

// Result is a finished workflow run.
type Result struct {
	Output       string // the script's final value, as a string
	InputTokens  int
	OutputTokens int
	Canceled     bool
}

// cancelToken is the sentinel agent_wait_any returns when ctx is canceled; the
// prelude turns it into a raised "workflow: canceled".
const cancelToken = 0

// Run executes script (the prelude is prepended) and returns its final value.
// A Ruby-level error surfaces as a non-nil error carrying the mruby backtrace.
func Run(ctx context.Context, script string, opt Options) (Result, error) {
	if opt.Agent == nil {
		return Result{}, fmt.Errorf("workflow: Options.Agent is required")
	}
	be := newBackend(ctx, opt)

	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesExceptionHandling))
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	if _, err := be.register(ctx, r); err != nil {
		return Result{}, fmt.Errorf("workflow: host module: %w", err)
	}

	var stdout, stderr bytes.Buffer
	full := preludeRB + "\n" + script + "\n"
	mc := wazero.NewModuleConfig().
		WithStdin(strings.NewReader(full)).
		WithStdout(&stdout).
		WithStderr(&stderr)

	_, err := r.InstantiateWithConfig(ctx, mrubyWasm, mc)
	in, out := be.usage()
	res := Result{InputTokens: in, OutputTokens: out, Canceled: be.canceled()}

	if err != nil {
		var ee *sys.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == 0 {
				res.Output = strings.TrimRight(stdout.String(), "\n")
				return res, nil
			}
			// Non-zero exit: a Ruby error (backtrace on stderr) or cancellation.
			if res.Canceled {
				return res, context.Canceled
			}
			return res, fmt.Errorf("workflow: script error: %s", strings.TrimSpace(stderr.String()))
		}
		if res.Canceled || ctx.Err() != nil {
			return res, context.Canceled
		}
		return res, fmt.Errorf("workflow: run: %w", err)
	}
	res.Output = strings.TrimRight(stdout.String(), "\n")
	return res, nil
}

// backend holds the host-side scheduler state for one Run.
type backend struct {
	ctx context.Context
	opt Options
	sem chan struct{} // concurrency limiter; nil when unlimited

	done chan uint32 // tokens of finished agents

	mu        sync.Mutex
	next      uint32
	results   map[uint32]AgentResult
	prompts   map[uint32]string // token => prompt, for progress labels
	inTok     int
	outTok    int
	wasCancel bool
}

func newBackend(ctx context.Context, opt Options) *backend {
	b := &backend{
		ctx:     ctx,
		opt:     opt,
		done:    make(chan uint32, 1024),
		results: map[uint32]AgentResult{},
		prompts: map[uint32]string{},
	}
	if opt.MaxConcurrent > 0 {
		b.sem = make(chan struct{}, opt.MaxConcurrent)
	}
	return b
}

func (b *backend) usage() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inTok, b.outTok
}
func (b *backend) canceled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wasCancel
}

func (b *backend) register(ctx context.Context, r wazero.Runtime) (api.Module, error) {
	return r.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(b.agentStart).Export("agent_start").
		NewFunctionBuilder().WithFunc(b.agentWaitAny).Export("agent_wait_any").
		NewFunctionBuilder().WithFunc(b.agentTake).Export("agent_take").
		NewFunctionBuilder().WithFunc(b.log).Export("log").
		NewFunctionBuilder().WithFunc(b.budgetRemaining).Export("budget_remaining").
		Instantiate(ctx)
}

// agentStart kicks off one agent() call on a goroutine and returns its token
// immediately (non-blocking). Returns -1 (cast) when the budget is exhausted.
func (b *backend) agentStart(_ context.Context, mod api.Module, ptr, length uint32) uint32 {
	promptBytes, _ := mod.Memory().Read(ptr, length)
	prompt := string(promptBytes)

	b.mu.Lock()
	if b.opt.Budget > 0 && int64(b.outTok) >= b.opt.Budget {
		b.mu.Unlock()
		return ^uint32(0) // -1: prelude raises "budget exhausted"
	}
	b.next++
	tok := b.next
	b.prompts[tok] = prompt
	b.mu.Unlock()

	if b.opt.Progress != nil {
		b.opt.Progress("→ " + label(prompt))
	}

	go func() {
		if b.sem != nil {
			select {
			case b.sem <- struct{}{}:
				defer func() { <-b.sem }()
			case <-b.ctx.Done():
				b.deliver(tok, AgentResult{Err: b.ctx.Err()})
				return
			}
		}
		res := b.opt.Agent(b.ctx, prompt)
		b.deliver(tok, res)
	}()
	return tok
}

func (b *backend) deliver(tok uint32, res AgentResult) {
	b.mu.Lock()
	b.results[tok] = res
	b.inTok += res.InputTokens
	b.outTok += res.OutputTokens
	b.mu.Unlock()
	b.done <- tok
}

// agentWaitAny blocks until some agent finishes, returning its token; returns
// cancelToken (0) when ctx is canceled so the prelude can unwind.
func (b *backend) agentWaitAny(_ context.Context, _ api.Module) uint32 {
	if b.ctx.Err() != nil {
		return b.markCanceled()
	}
	select {
	case tok := <-b.done:
		// A cancel that landed at the same time as a completion still cancels —
		// the workflow shouldn't half-finish on a canceled context.
		if b.ctx.Err() != nil {
			return b.markCanceled()
		}
		if b.opt.Progress != nil {
			b.mu.Lock()
			p := b.prompts[tok]
			b.mu.Unlock()
			b.opt.Progress("✓ " + label(p))
		}
		return tok
	case <-b.ctx.Done():
		return b.markCanceled()
	}
}

func (b *backend) markCanceled() uint32 {
	b.mu.Lock()
	b.wasCancel = true
	b.mu.Unlock()
	return cancelToken
}

// label trims a prompt to a short single-line progress label.
func label(prompt string) string {
	s := prompt
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	const max = 60
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// agentTake writes the result for token into the guest buffer, returning its
// length. A failed agent surfaces its error text as the result string.
func (b *backend) agentTake(_ context.Context, mod api.Module, token, outPtr, outCap uint32) uint32 {
	b.mu.Lock()
	res, ok := b.results[token]
	delete(b.results, token)
	delete(b.prompts, token)
	b.mu.Unlock()

	var s string
	switch {
	case !ok:
		s = "[workflow: unknown token]"
	case res.Err != nil:
		s = "[agent error] " + res.Err.Error()
	default:
		s = res.Reply
	}
	out := []byte(s)
	if uint32(len(out)) > outCap {
		out = out[:outCap]
	}
	mod.Memory().Write(outPtr, out)
	return uint32(len(out))
}

func (b *backend) log(_ context.Context, mod api.Module, ptr, length uint32) {
	msg, _ := mod.Memory().Read(ptr, length)
	if b.opt.Log != nil {
		b.opt.Log(string(msg))
	}
}

func (b *backend) budgetRemaining(_ context.Context, _ api.Module) uint64 {
	if b.opt.Budget <= 0 {
		return uint64(int64(^uint64(0) >> 1)) // max int64: "unlimited"
	}
	b.mu.Lock()
	rem := b.opt.Budget - int64(b.outTok)
	b.mu.Unlock()
	if rem < 0 {
		rem = 0
	}
	return uint64(rem)
}
