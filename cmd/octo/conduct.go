package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/conductor"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/tools"
)

// runConduct handles `octo conduct <subcommand>` — the autonomous long-horizon
// orchestrator (the successor to `octo goal` for large coherent work). See
// dev-docs/autonomous-orchestrator-design.md.
func runConduct(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printConductUsage(stdout)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return runConductList(stdout, stderr)
	case "status", "report":
		return runConductStatus(args[1:], stdout, stderr)
	case "show":
		return runConductShow(args[1:], stdout, stderr)
	case "resume":
		return runConductResume(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		printConductUsage(stdout)
		return 0
	default:
		// Anything else is treated as the goal text to plan + run.
		return runConductStart(args, stdout, stderr)
	}
}

func printConductUsage(w io.Writer) {
	fmt.Fprintln(w, "octo conduct — autonomous long-horizon orchestration")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: octo conduct \"<goal>\" [flags]   Plan + run a goal to completion, unattended")
	fmt.Fprintln(w, "       octo conduct list                List every conducted goal, newest first")
	fmt.Fprintln(w, "       octo conduct status <id>         Show one ledger's per-unit state + report")
	fmt.Fprintln(w, "       octo conduct show <id> [unit]    Print the full result text of done units")
	fmt.Fprintln(w, "       octo conduct resume <id>         Re-run a stopped/blocked ledger")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags (start):")
	fmt.Fprintln(w, "  --provider, --model        Provider + model for the planner and workers")
	fmt.Fprintln(w, "  --plan-only                Seed the ledger and exit (run later with resume)")
	fmt.Fprintln(w, "  (default)                  No gate — a unit is done when its worker says so")
	fmt.Fprintln(w, "  --verify                   Gate each unit with an LLM judge")
	fmt.Fprintln(w, "  --verify-cmd \"<cmd>\"       Gate each unit with a shell command (e.g. go build/test)")
	fmt.Fprintln(w, "  --max-attempts N           Verify-fail retries per unit before it blocks (default 3)")
	fmt.Fprintln(w, "  --max-iterations N         Loop-turn budget backstop (default scales with units)")
	fmt.Fprintln(w, "  --stall-rounds N           Stop after N rounds with no unit completed (0 = off)")
	fmt.Fprintln(w, "  --concurrency N            Parallel workers in isolated worktrees (default 1)")
	fmt.Fprintln(w, "  --no-worktree              Run workers in the main tree even when concurrency=1")
	fmt.Fprintln(w, "  --replan                   Let the conductor revise the plan as it learns")
}

// conductFlags holds the parsed start-flags shared across the start/resume paths.
type conductFlags struct {
	provider      string
	model         string
	planOnly      bool
	verify        bool   // LLM judge gate
	verifyCmd     string // objective shell gate, e.g. "go build ./... && go test ./..."
	maxAttempts   int
	maxIterations int
	stallRounds   int
	concurrency   int
	noWorktree    bool
	replan        bool
}

func conductFlagSet(name string, f *conductFlags, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.provider, "provider", "", "Provider: anthropic | openai")
	fs.StringVar(&f.model, "model", "", "Model name")
	fs.BoolVar(&f.planOnly, "plan-only", false, "Seed the ledger and exit")
	fs.BoolVar(&f.verify, "verify", false, "Gate each unit with an LLM judge (default: no gate — trust the worker)")
	fs.StringVar(&f.verifyCmd, "verify-cmd", "", "Gate each unit with a shell command, e.g. \"go build ./... && go test ./...\" (overrides --verify)")
	fs.IntVar(&f.maxAttempts, "max-attempts", 0, "Verify-fail retries per unit before blocking")
	fs.IntVar(&f.maxIterations, "max-iterations", 0, "Loop-turn budget backstop")
	fs.IntVar(&f.stallRounds, "stall-rounds", 0, "Stop after N rounds with no progress (0=off)")
	fs.IntVar(&f.concurrency, "concurrency", 1, "Parallel workers in isolated worktrees")
	fs.BoolVar(&f.noWorktree, "no-worktree", false, "Run workers in the main tree")
	fs.BoolVar(&f.replan, "replan", false, "Let the conductor revise the plan as it learns")
	return fs
}

func (f conductFlags) config() conductor.Config {
	return conductor.Config{
		MaxAttempts:   f.maxAttempts,
		MaxIterations: f.maxIterations,
		MaxConcurrent: f.concurrency,
		StallRounds:   f.stallRounds,
	}
}

// verifier picks the completion gate. Default is no gate (trust the worker's
// own "done"); --verify-cmd installs an objective shell gate; --verify installs
// an LLM judge. --verify-cmd wins if both are given.
func (f conductFlags) verifier(a *agent.Agent) conductor.Verifier {
	switch {
	case strings.TrimSpace(f.verifyCmd) != "":
		return &conductor.CmdVerifier{Commands: []string{f.verifyCmd}}
	case f.verify:
		return newJudgeVerifier(a)
	default:
		return conductor.NopVerifier{}
	}
}

// runConductStart plans the goal into a seed ledger, then conducts it.
func runConductStart(args []string, stdout, stderr io.Writer) int {
	var f conductFlags
	fs := conductFlagSet("conduct", &f, stderr)
	fs.Usage = func() { printConductUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		fmt.Fprintln(stderr, "octo conduct: a goal is required (e.g. octo conduct \"port the parser package to Go\")")
		return 2
	}

	a, cleanup, code := buildConductAgent(f, stdout, stderr)
	if code != 0 {
		return code
	}
	defer cleanup()

	fmt.Fprintf(stdout, "Planning…  goal: %s\n", oneLine(goal))
	res, err := a.PlanTask(context.Background(), goal)
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: planner: %v\n", err)
		return 1
	}
	if len(res.Subtasks) == 0 {
		fmt.Fprintln(stderr, "octo conduct: planner returned no units — refine the goal and try again")
		return 1
	}

	units := make([]conductor.Unit, 0, len(res.Subtasks))
	for i, ps := range res.Subtasks {
		units = append(units, conductor.Unit{
			ID:          i + 1,
			Description: ps.Description,
			BlockedBy:   ps.BlockedBy,
			Status:      conductor.UnitPending,
		})
	}

	store, err := conductor.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	led, err := store.Create(goal, units)
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: persist: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Created ledger %s\n\n", led.ID)
	conductor.Report(stdout, led)
	fmt.Fprintln(stdout)

	if f.planOnly {
		fmt.Fprintln(stdout, "Plan-only. Run with: octo conduct resume "+led.ShortID())
		return 0
	}
	return conductLedger(store, led.ID, f, stdout, stderr)
}

// runConductResume re-runs a stopped/blocked ledger.
func runConductResume(args []string, stdout, stderr io.Writer) int {
	var f conductFlags
	fs := conductFlagSet("conduct resume", &f, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "Usage: octo conduct resume <id> [flags]")
		return 2
	}
	store, err := conductor.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v  (try `octo conduct list`)\n", err)
		return 1
	}
	// Clear blocked units back to pending so resume retries them with a fresh
	// attempt budget; in-progress units keep their handle and continue.
	_, err = store.Update(id, func(l *conductor.Ledger) error {
		for i := range l.Units {
			if l.Units[i].Status == conductor.UnitBlocked {
				l.Units[i].Status = conductor.UnitPending
				l.Units[i].Attempts = 0
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Resuming ledger %s…\n", id)
	return conductLedger(store, id, f, stdout, stderr)
}

// conductLedger wires the worker + verifier + (optional) worktrees + replanner
// and drives the loop. Shared by start and resume.
func conductLedger(store *conductor.Store, id string, f conductFlags, stdout, stderr io.Writer) int {
	a, cleanup, code := buildConductAgent(f, stdout, stderr)
	if code != 0 {
		return code
	}
	defer cleanup()

	worker := &spawnerWorker{}
	c := conductor.New(store, worker, f.verifier(a), stdout, f.config())

	if !f.noWorktree && f.concurrency > 1 {
		// Phase 2: isolate parallel workers in git worktrees so their edits +
		// shells don't collide. Engages only with --concurrency >1; at the
		// default (1) workers run in the main tree. Falls back gracefully if
		// the repo isn't a git work tree.
		if wt, err := newGitWorktrees(); err == nil {
			c = c.WithWorktrees(wt)
		} else {
			fmt.Fprintf(stderr, "octo conduct: worktree isolation unavailable (%v) — running in the main tree\n", err)
		}
	}
	if f.replan {
		c = c.WithReplanner(newAgentReplanner(a))
	}

	fmt.Fprintln(stdout, "Conducting…  (unattended — Ctrl-C to stop; resume later)")
	err := c.Run(context.Background(), id)

	led, gerr := store.Get(id)
	if gerr == nil {
		fmt.Fprintln(stdout)
		conductor.Report(stdout, led)
	}
	if err != nil {
		fmt.Fprintf(stderr, "\nocto conduct: %v\n", err)
		return 1
	}
	return 0
}

func runConductList(stdout, stderr io.Writer) int {
	store, err := conductor.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	leds, err := store.List()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	if len(leds) == 0 {
		fmt.Fprintln(stdout, "No conducted goals yet. Start one with: octo conduct \"<goal>\"")
		return 0
	}
	for _, l := range leds {
		done := 0
		for _, u := range l.Units {
			if u.Status == conductor.UnitDone {
				done++
			}
		}
		fmt.Fprintf(stdout, "%s  [%-9s]  %d/%d units  %s\n",
			l.ShortID(), l.Status, done, len(l.Units), oneLine(l.Goal))
	}
	return 0
}

func runConductStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "Usage: octo conduct status <id>")
		return 2
	}
	store, err := conductor.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	led, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	conductor.Report(stdout, led)
	return 0
}

// runConductShow prints the full result text of done units (and errors of
// blocked units). `octo conduct show <id>` shows all; an optional unit-id
// narrows to one. This is where the worker's actual output lives — `status`
// only previews it.
func runConductShow(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "Usage: octo conduct show <id> [unit-id]")
		return 2
	}
	store, err := conductor.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	unitID := 0
	if len(args) >= 2 {
		n, convErr := strconv.Atoi(args[1])
		if convErr != nil || n <= 0 {
			fmt.Fprintf(stderr, "octo conduct: unit-id must be a positive integer, got %q\n", args[1])
			return 2
		}
		unitID = n
	}
	led, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return 1
	}
	conductor.ShowResults(stdout, led, unitID)
	return 0
}

// buildConductAgent constructs the planner/worker parent agent and registers
// the spawner so launch_agent-style workers can run. The returned cleanup
// unregisters the spawner.
func buildConductAgent(f conductFlags, stdout, stderr io.Writer) (*agent.Agent, func(), int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo conduct: %v\n", err)
		return nil, func() {}, 1
	}
	provName, resolvedModel, ok := resolveProviderModel(f.provider, f.model, cfg)
	if !ok {
		fmt.Fprintf(stderr, "octo conduct: unknown provider %q (use 'anthropic' or 'openai')\n", provName)
		return nil, func() {}, 2
	}
	llmSender, err := buildSender(provName, cfg, stderr, senderTuning{})
	if err != nil {
		return nil, func() {}, 1
	}
	a := agent.New(llmSender, resolvedModel)
	a.MaxTokens = defaultMaxTokensForPlanner
	cwd, _ := os.Getwd()
	a.CWD = cwd
	a.System = prompt.Compose("", cwd, buildEnvContext(cwd), "", "", true)

	executor := tools.NewDefaultRegistry()
	tools.SetSpawner(newAgentSpawner(a, executor, tools.DefaultTools))
	return a, func() { tools.SetSpawner(nil) }, 0
}

// defaultMaxTokensForPlanner caps the planner/replanner side-call output.
const defaultMaxTokensForPlanner = 4096

// oneLine collapses a multi-line string to a single-line preview, truncated so
// it doesn't wrap awkwardly in status lines.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}

// spawnerWorker adapts the package-global tools.ActiveSpawner into a
// conductor.Worker. A max-turns checkpoint surfaces as Incomplete=true (the
// conductor continues from it) rather than an error.
type spawnerWorker struct{}

func (spawnerWorker) Run(ctx context.Context, spec conductor.WorkSpec) (conductor.WorkResult, error) {
	sp := tools.ActiveSpawner()
	if sp == nil {
		return conductor.WorkResult{}, fmt.Errorf("conduct: no Spawner configured")
	}
	if spec.Workdir != "" {
		ctx = tools.WithWorkingDir(ctx, spec.Workdir)
	}
	res, err := sp.Spawn(ctx, tools.SpawnRequest{Description: spec.Label, Prompt: spec.Prompt})
	if err != nil {
		return conductor.WorkResult{}, err
	}
	return conductor.WorkResult{
		Handle:     res.AgentID,
		Reply:      res.Reply,
		Incomplete: res.StopReason == agent.StopReasonMaxTurns,
	}, nil
}

func (spawnerWorker) Continue(ctx context.Context, handle, message string) (conductor.WorkResult, error) {
	sp := tools.ActiveSpawner()
	if sp == nil {
		return conductor.WorkResult{}, fmt.Errorf("conduct: no Spawner configured")
	}
	res, err := sp.Continue(ctx, handle, message)
	if err != nil {
		return conductor.WorkResult{}, err
	}
	return conductor.WorkResult{
		Handle:     res.AgentID,
		Reply:      res.Reply,
		Incomplete: res.StopReason == agent.StopReasonMaxTurns,
	}, nil
}
