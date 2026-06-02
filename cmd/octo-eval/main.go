// Command octo-eval runs a lightweight, Docker-free eval suite against octo.
//
// Each task under --tasks-dir is a local fixture: octo edits a copy of repo/,
// then hidden judging files are injected and verify.sh decides pass/fail. No
// repo cloning, no image builds — a suite runs in seconds.
//
//	octo-eval list
//	octo-eval run --octo ./octo --model <model> --provider anthropic
//	octo-eval run --filter fix-nil-panic   # run one task
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/eval"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "list":
		err = runList(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `octo-eval — lightweight Docker-free eval suite

usage:
  octo-eval list [--tasks-dir DIR]
  octo-eval run  [--tasks-dir DIR] [--octo PATH] [--model M] [--provider P]
                 [--workdir DIR] [--filter NAME] [--max-turns N]
                 [--timeout DUR] [--allow-net]`)
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	tasksDir := fs.String("tasks-dir", "evals/tasks", "directory of task fixtures")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tasks, err := eval.LoadTasks(*tasksDir)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Printf("no tasks in %s\n", *tasksDir)
		return nil
	}
	for _, t := range tasks {
		fmt.Printf("%-24s %s\n", t.Name, firstLine(t.Prompt))
	}
	return nil
}

func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	tasksDir := fs.String("tasks-dir", "evals/tasks", "directory of task fixtures")
	octoBin := fs.String("octo", "./octo", "path to the octo binary")
	model := fs.String("model", "", "model passed to octo (empty = octo default)")
	provider := fs.String("provider", "", "provider passed to octo (empty = octo default)")
	workdir := fs.String("workdir", filepath.Join(os.TempDir(), "octo-eval"), "scratch dir for per-task copies + octo HOME")
	filter := fs.String("filter", "", "run only tasks whose name contains this substring")
	maxTurns := fs.Int("max-turns", 50, "octo --max-turns: model round-trips per task")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-task octo timeout (a task's own timeout overrides)")
	verifyTimeout := fs.Duration("verify-timeout", 2*time.Minute, "cap on a task's verify command")
	allowNet := fs.Bool("allow-net", false, "allow octo network access (default false — hermetic)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tasks, err := eval.LoadTasks(*tasksDir)
	if err != nil {
		return err
	}
	if *filter != "" {
		tasks = filterTasks(tasks, *filter)
	}
	if len(tasks) == 0 {
		return fmt.Errorf("no matching tasks in %s", *tasksDir)
	}
	octoAbs, err := filepath.Abs(*octoBin)
	if err != nil {
		return err
	}

	opt := eval.Options{
		OctoBin:     octoAbs,
		WorkDir:     *workdir,
		Model:       *model,
		Provider:    *provider,
		MaxTurns:    *maxTurns,
		AllowNet:    *allowNet,
		Timeout:     *timeout,
		VerifyAfter: *verifyTimeout,
	}

	ctx := context.Background()
	var resolved int
	for i, t := range tasks {
		fmt.Printf("[%d/%d] %s — run octo + verify…\n", i+1, len(tasks), t.Name)
		res := eval.RunTask(ctx, t, opt)
		switch {
		case res.Err != nil:
			fmt.Printf("        ! harness error: %v\n", res.Err)
		case res.Resolved:
			resolved++
			fmt.Printf("        ✓ resolved (%.1fs)\n", res.Duration.Seconds())
		default:
			fmt.Printf("        ✗ unresolved (%.1fs) — log: %s\n", res.Duration.Seconds(), res.OctoLog)
			if tail := lastLines(res.Verify, 8); tail != "" {
				fmt.Printf("          verify tail:\n%s\n", indent(tail, "            "))
			}
		}
	}
	fmt.Printf("\nresolved %d/%d\n", resolved, len(tasks))
	return nil
}

func filterTasks(tasks []eval.Task, sub string) []eval.Task {
	var out []eval.Task
	for _, t := range tasks {
		if strings.Contains(t.Name, sub) {
			out = append(out, t)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 72 {
		return s[:69] + "..."
	}
	return s
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
