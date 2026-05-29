package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/taskgraph"
	"github.com/Leihb/octo-agent/internal/tools"
)

// runTask handles `octo goal <subcommand>`. PR2 (this file) wires only
// `start "<goal>"` — it plans the DAG via the LLM and persists to
// ~/.octo/tasks/<id>.json. The scheduler (`run`), inspection
// (`list / status / show`), and lifecycle (`resume / cancel`) commands
// land in subsequent PRs.
func runTask(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTaskUsage(stdout)
		return 2
	}

	switch args[0] {
	case "start":
		return runTaskStart(args[1:], stdout, stderr)
	case "run":
		return runTaskRun(args[1:], stdout, stderr)
	case "list", "ls":
		return runTaskList(args[1:], stdout, stderr)
	case "status":
		return runTaskStatus(args[1:], stdout, stderr)
	case "show":
		return runTaskShow(args[1:], stdout, stderr)
	case "resume":
		return runTaskResume(args[1:], stdout, stderr)
	case "cancel":
		return runTaskCancel(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		printTaskUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "octo goal: unknown subcommand %q\n", args[0])
		printTaskUsage(stderr)
		return 2
	}
}

func printTaskUsage(w io.Writer) {
	fmt.Fprintln(w, "octo goal — autonomous task orchestration (M11)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: octo goal <subcommand> [args...]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  start \"<goal>\" [--plan-only]   Plan + run a goal end-to-end (or just plan).")
	fmt.Fprintln(w, "  run <id>                       Execute a previously planned task.")
	fmt.Fprintln(w, "  list                           List every task, newest first.")
	fmt.Fprintln(w, "  status <id>                    Show one task's DAG state (status per subtask).")
	fmt.Fprintln(w, "  show <id> <subtask-id>         Show one subtask's full result / error / timing.")
	fmt.Fprintln(w, "  resume <id>                    Re-run failed / skipped / cancelled subtasks.")
	fmt.Fprintln(w, "  cancel <id>                    Mark a task cancelled (any in-flight `run` honors ctx separately).")
}

// runTaskStart handles `octo goal start "<goal>" [flags]`. It runs the
// planner side-call against the same provider chain as `octo chat`, then
// persists the resulting DAG. The scheduler isn't wired yet — this PR's
// end state is a `pending` task on disk that a later `octo goal run <id>`
// (PR3) will execute.
func runTaskStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "Provider: anthropic | openai (default from `octo config`, else anthropic)")
	model := fs.String("model", "", "Model name (defaults to the provider's cheapest reasoning model)")
	planOnly := fs.Bool("plan-only", false, "Plan the DAG and exit — don't run subtasks yet (use `octo goal run <id>` later)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: octo goal start \"<goal>\" [--provider …] [--model …] [--plan-only]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		fmt.Fprintln(stderr, "octo goal start: a goal is required (e.g. octo goal start \"migrate the auth middleware\")")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal start: %v\n", err)
		return 1
	}
	provName, resolvedModel, ok := resolveProviderModel(*providerName, *model, cfg)
	if !ok {
		fmt.Fprintf(stderr, "octo goal start: unknown provider %q (use 'anthropic' or 'openai')\n", provName)
		return 2
	}

	prov, err := buildProvider(provName, cfg, stderr)
	if err != nil {
		return 1
	}

	a := agent.New(providerSender{
		p:        prov,
		cacheKey: newCacheKey(),
	}, resolvedModel)
	a.MaxTokens = defaultMaxTokensForPlanner
	cwd, _ := os.Getwd()
	a.System = prompt.Compose("", cwd, buildEnvContext(cwd), "", "")

	fmt.Fprintf(stdout, "Planning…  goal: %s\n", oneLine(goal))
	res, err := a.PlanTask(context.Background(), goal)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal start: planner: %v\n", err)
		return 1
	}
	if len(res.Subtasks) == 0 {
		fmt.Fprintln(stderr, "octo goal start: planner returned no subtasks — refine the goal and try again")
		return 1
	}

	subs := make([]taskgraph.Subtask, 0, len(res.Subtasks))
	for i, ps := range res.Subtasks {
		subs = append(subs, taskgraph.Subtask{
			ID:          i + 1,
			Description: ps.Description,
			BlockedBy:   ps.BlockedBy,
			Status:      taskgraph.SubtaskPending,
		})
	}

	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal start: %v\n", err)
		return 1
	}
	task, err := store.Create(goal, subs)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal start: persist: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Created task %s\n\n", task.ID)
	printPlannedDAG(stdout, task)
	fmt.Fprintln(stdout)

	if *planOnly {
		fmt.Fprintln(stdout, "Plan-only mode. Run with: octo goal run "+task.ID)
		return 0
	}

	// Chain into the scheduler reusing the agent + provider we already
	// built — avoids a second provider handshake.
	fmt.Fprintln(stdout, "Running…")
	executor := tools.NewDefaultRegistry()
	tools.SetSpawner(newAgentSpawner(a, executor, tools.DefaultTools))
	defer tools.SetSpawner(nil)

	sch := taskgraph.NewScheduler(store, &spawnerExecutor{}, stdout)
	if err := sch.Run(context.Background(), task.ID); err != nil {
		fmt.Fprintf(stderr, "octo goal start: %v\n", err)
		return 1
	}
	return 0
}

// printPlannedDAG renders the planned subtasks under the goal so the user
// can sanity-check the planner output before running. Format mirrors the
// task_manager renderer for visual consistency.
func printPlannedDAG(w io.Writer, t *taskgraph.Task) {
	fmt.Fprintf(w, "Goal: %s\n\n", t.Goal)
	fmt.Fprintln(w, "Plan:")
	for _, s := range t.Subtasks {
		fmt.Fprintf(w, "  #%-2d %s\n", s.ID, s.Description)
		if len(s.BlockedBy) > 0 {
			fmt.Fprintf(w, "      ↳ depends on: %s\n", joinInts(s.BlockedBy))
		}
	}
}

// oneLine collapses a multi-line goal to a single-line preview for the
// status line. Long goals are truncated so they don't wrap awkwardly.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}

// joinInts formats a []int as "1, 3, 4" for human display.
func joinInts(in []int) string {
	parts := make([]string, len(in))
	for i, n := range in {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}

// defaultMaxTokensForPlanner mirrors what `octo chat` defaults to when
// nothing is passed; the planner's actual cap is the much smaller
// planMaxTokens inside agent.PlanTask, but the agent struct still wants
// a sensible value.
const defaultMaxTokensForPlanner = 4096

// runTaskRun handles `octo goal run <id> [flags]`. It loads the persisted
// task, wires an M10-backed Executor, hands them to taskgraph.Scheduler,
// and exits with 0 on success / 1 on failure / 2 on bad args. Mirrors the
// loop semantics already covered by scheduler_test.go — this is mostly
// CLI plumbing.
func runTaskRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "Provider: anthropic | openai (default from `octo config`, else anthropic)")
	model := fs.String("model", "", "Model name (defaults to the provider's cheapest reasoning model)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: octo goal run <id> [--provider …] [--model …]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "octo goal run: task id is required (try `octo goal list` once that lands)")
		return 2
	}
	id := fs.Arg(0)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal run: %v\n", err)
		return 1
	}
	provName, resolvedModel, ok := resolveProviderModel(*providerName, *model, cfg)
	if !ok {
		fmt.Fprintf(stderr, "octo goal run: unknown provider %q (use 'anthropic' or 'openai')\n", provName)
		return 2
	}

	prov, err := buildProvider(provName, cfg, stderr)
	if err != nil {
		return 1
	}

	// Parent agent acts as the spawner's anchor — it owns the Sender, the
	// system prompt every sub-agent inherits, and the token counter the
	// children roll back into. We're not running an interactive loop here,
	// but the sub_agent.go spawner closure needs an *agent.Agent to attach
	// to.
	parent := agent.New(providerSender{
		p:        prov,
		cacheKey: newCacheKey(),
	}, resolvedModel)
	parent.MaxTokens = defaultMaxTokensForPlanner
	cwd, _ := os.Getwd()
	parent.System = prompt.Compose("", cwd, buildEnvContext(cwd), "", "")

	executor := tools.NewDefaultRegistry()
	tools.SetSpawner(newAgentSpawner(parent, executor, tools.DefaultTools))
	defer tools.SetSpawner(nil)

	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal run: %v\n", err)
		return 1
	}
	resolvedID, err := store.ResolveID(id)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal run: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo goal list` to see what's available.")
		return 2
	}
	sch := taskgraph.NewScheduler(store, &spawnerExecutor{}, stdout)
	if err := sch.Run(context.Background(), resolvedID); err != nil {
		fmt.Fprintf(stderr, "octo goal run: %v\n", err)
		return 1
	}
	return 0
}

// runTaskList prints every task on disk, newest first, in a compact table.
func runTaskList(_ []string, stdout, stderr io.Writer) int {
	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal list: %v\n", err)
		return 1
	}
	tasks, err := store.List()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal list: %v\n", err)
		return 1
	}
	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "No tasks yet. Try `octo goal start \"<goal>\"`.")
		return 0
	}
	for _, t := range tasks {
		when := t.Created.Local().Format("2006-01-02 15:04")
		fmt.Fprintf(stdout, "%s  %s  %-9s  %s\n", t.ShortID(), when, t.Status, oneLine(t.Goal))
	}
	return 0
}

// runTaskStatus prints one task's full DAG state — every subtask with its
// status, dependencies, and one-line result snippet for completed work.
func runTaskStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "Usage: octo goal status <id>")
		return 2
	}
	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal status: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "octo goal status: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo goal list` to see what's available.")
		return 2
	}
	t, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal status: %v\n", err)
		return 1
	}
	printTaskStatus(stdout, t)
	return 0
}

// printTaskStatus is the formatter shared between `octo goal status` and
// the no-args summary `start` and `run` might print on completion later.
func printTaskStatus(w io.Writer, t *taskgraph.Task) {
	fmt.Fprintf(w, "Task %s — %s\n", t.ID, t.Status)
	fmt.Fprintf(w, "Goal: %s\n\n", t.Goal)
	for _, sub := range t.Subtasks {
		fmt.Fprintf(w, "  %s #%-2d %s\n", subtaskGlyph(sub.Status), sub.ID, sub.Description)
		if len(sub.BlockedBy) > 0 {
			fmt.Fprintf(w, "      ↳ blocked_by: %s\n", joinInts(sub.BlockedBy))
		}
		if sub.Status == taskgraph.SubtaskFailed && sub.Error != "" {
			fmt.Fprintf(w, "      ✗ %s\n", oneLine(sub.Error))
		}
		if sub.Status == taskgraph.SubtaskDone && sub.Result != "" {
			fmt.Fprintf(w, "      ↳ %s\n", oneLine(sub.Result))
		}
	}
}

// subtaskGlyph returns the same single-rune markers task_manager uses, so
// the visual language is consistent across the two task layers.
func subtaskGlyph(s taskgraph.SubtaskStatus) string {
	switch s {
	case taskgraph.SubtaskRunning:
		return "▶"
	case taskgraph.SubtaskPending:
		return "○"
	case taskgraph.SubtaskDone:
		return "✓"
	case taskgraph.SubtaskFailed:
		return "✗"
	case taskgraph.SubtaskSkipped:
		return "⊘"
	}
	return "·"
}

// runTaskShow prints one subtask's full result / error / timestamps —
// useful when `octo goal status` shows a Failed or interesting Done node
// and the user wants the verbatim payload.
func runTaskShow(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "Usage: octo goal show <id> <subtask-id>")
		return 2
	}
	subID := 0
	if _, err := fmt.Sscanf(args[1], "%d", &subID); err != nil || subID < 1 {
		fmt.Fprintf(stderr, "octo goal show: invalid subtask-id %q (want a positive integer)\n", args[1])
		return 2
	}
	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal show: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "octo goal show: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo goal list` to see what's available.")
		return 2
	}
	t, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal show: %v\n", err)
		return 1
	}
	sub := t.Find(subID)
	if sub == nil {
		fmt.Fprintf(stderr, "octo goal show: no subtask #%d in task %s\n", subID, t.ID)
		return 1
	}
	fmt.Fprintf(stdout, "Task %s · subtask #%d (%s)\n", t.ID, sub.ID, sub.Status)
	fmt.Fprintf(stdout, "Description:\n  %s\n\n", sub.Description)
	if len(sub.BlockedBy) > 0 {
		fmt.Fprintf(stdout, "Blocked by: %s\n", joinInts(sub.BlockedBy))
	}
	if sub.Started != nil {
		fmt.Fprintf(stdout, "Started:  %s\n", sub.Started.Format("2006-01-02 15:04:05 UTC"))
	}
	if sub.Finished != nil {
		fmt.Fprintf(stdout, "Finished: %s\n", sub.Finished.Format("2006-01-02 15:04:05 UTC"))
		if sub.Started != nil {
			fmt.Fprintf(stdout, "Duration: %s\n", sub.Finished.Sub(*sub.Started).Round(100_000_000)) // 0.1s precision
		}
	}
	if sub.Error != "" {
		fmt.Fprintf(stdout, "\nError:\n%s\n", sub.Error)
	}
	if sub.Result != "" {
		fmt.Fprintf(stdout, "\nResult:\n%s\n", sub.Result)
	}
	return 0
}

// resetForResume flips a task's Failed / Skipped subtasks back to Pending so
// the scheduler re-picks them and moves the task itself back to Pending. Shared
// by `octo goal resume` (CLI) and the TUI's /goal resume so the two can't drift.
// Done subtasks (and their results) are preserved.
func resetForResume(t *taskgraph.Task) error {
	if t.Status == taskgraph.TaskDone {
		return fmt.Errorf("task %s is already done — nothing to resume", t.ShortID())
	}
	for i := range t.Subtasks {
		switch t.Subtasks[i].Status {
		case taskgraph.SubtaskFailed, taskgraph.SubtaskSkipped:
			t.Subtasks[i].Status = taskgraph.SubtaskPending
			t.Subtasks[i].Error = ""
			t.Subtasks[i].Started = nil
			t.Subtasks[i].Finished = nil
		}
	}
	t.Status = taskgraph.TaskPending
	return nil
}

// runTaskResume re-runs a stalled task. Resets Failed / Skipped /
// Cancelled subtasks back to Pending so the scheduler picks them up; the
// task itself moves Failed/Cancelled → Pending. Done subtasks (and their
// results) are preserved.
func runTaskResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "Provider: anthropic | openai (default from `octo config`, else anthropic)")
	model := fs.String("model", "", "Model name (defaults to the provider's cheapest reasoning model)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "Usage: octo goal resume <id>")
		return 2
	}
	rawID := fs.Arg(0)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal resume: %v\n", err)
		return 1
	}

	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal resume: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(rawID)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal resume: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo goal list` to see what's available.")
		return 2
	}

	t, err := store.Update(id, resetForResume)
	if err != nil {
		fmt.Fprintf(stderr, "octo goal resume: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Resumed task %s — re-running pending subtasks…\n", t.ID)

	// Then drive Scheduler.Run the same way `run` does.
	return resumeAndRun(t, *providerName, *model, cfg, stdout, stderr)
}

// resumeAndRun is the shared "build provider+spawner, hand to Scheduler"
// path used by resume (and potentially by future commands). flagProvider /
// flagModel are the raw CLI values ("" = unset); cfg supplies the persisted
// fallbacks. Returns the CLI exit code.
func resumeAndRun(t *taskgraph.Task, flagProvider, flagModel string, cfg config.Config, stdout, stderr io.Writer) int {
	provName, resolvedModel, ok := resolveProviderModel(flagProvider, flagModel, cfg)
	if !ok {
		fmt.Fprintf(stderr, "unknown provider %q (use 'anthropic' or 'openai')\n", provName)
		return 2
	}
	prov, err := buildProvider(provName, cfg, stderr)
	if err != nil {
		return 1
	}
	parent := agent.New(providerSender{p: prov, cacheKey: newCacheKey()}, resolvedModel)
	parent.MaxTokens = defaultMaxTokensForPlanner
	cwd, _ := os.Getwd()
	parent.System = prompt.Compose("", cwd, buildEnvContext(cwd), "", "")

	executor := tools.NewDefaultRegistry()
	tools.SetSpawner(newAgentSpawner(parent, executor, tools.DefaultTools))
	defer tools.SetSpawner(nil)

	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	sch := taskgraph.NewScheduler(store, &spawnerExecutor{}, stdout)
	if err := sch.Run(context.Background(), t.ID); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// runTaskCancel marks a task Cancelled. Does NOT signal a concurrently-
// running scheduler — in v1 the scheduler is foreground-only, so cancel
// is mostly useful for tasks the user abandoned (Ctrl-C'd) so they don't
// get accidentally resumed.
func runTaskCancel(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "Usage: octo goal cancel <id>")
		return 2
	}
	store, err := taskgraph.NewStore()
	if err != nil {
		fmt.Fprintf(stderr, "octo goal cancel: %v\n", err)
		return 1
	}
	id, err := store.ResolveID(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "octo goal cancel: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo goal list` to see what's available.")
		return 2
	}
	t, err := store.Update(id, func(t *taskgraph.Task) error {
		if t.Status == taskgraph.TaskDone {
			return fmt.Errorf("task %s is done — nothing to cancel", t.ID)
		}
		t.Status = taskgraph.TaskCancelled
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "octo goal cancel: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Cancelled task %s.\n", t.ID)
	return 0
}

// spawnerExecutor adapts the package-global tools.ActiveSpawner into the
// taskgraph.Executor interface. Each subtask becomes one launch_agent
// invocation: the child sees the subtask description as its prompt, has
// the parent's full tool catalog (minus launch_agent so it can't recurse),
// and reports back a single final text reply.
type spawnerExecutor struct{}

func (spawnerExecutor) Execute(ctx context.Context, description string) (string, error) {
	sp := tools.ActiveSpawner()
	if sp == nil {
		return "", fmt.Errorf("task run: no Spawner configured")
	}
	res, err := sp.Spawn(ctx, tools.SpawnRequest{
		Description: oneLine(description),
		Prompt:      description,
	})
	if err != nil {
		return "", err
	}
	return res.Reply, nil
}
