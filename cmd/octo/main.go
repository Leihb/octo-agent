// Command octo is the Go implementation of the Octo AI agent.
//
// Milestones so far:
//   - M1: version / help / CLI scaffold
//   - M2: chat (single-turn, streaming, Anthropic + OpenAI)
//   - M3: interactive REPL + session persistence (this milestone)
//
// Tool use, skills, the web server, and IM bridges land in M4+.
// See dev-docs/CATCHUP_PLAN.md for the wider plan.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Leihb/octo-agent/internal/sandbox"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable entry point. Splitting it out keeps main thin and
// lets the test harness drive the CLI without spawning a subprocess.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	// Materialize the binary's default skills to ~/.octo/skills-default so
	// they're discoverable like any user skill. Best-effort and a fast no-op
	// once current; skipped for the internal fast-path commands.
	if args[0] != "__sandboxed-exec" && args[0] != "__complete" {
		_ = skills.MaterializeDefaults(version.Version)
	}

	switch args[0] {
	case "__sandboxed-exec":
		// Internal: the OS-sandbox re-exec shim (Linux). Applies confinement to
		// itself, then execs the real command. Not user-facing.
		return sandbox.ShimMain()
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "octo %s\n", version.String())
		return 0
	case "help", "--help", "-h":
		// "octo help <cmd>" prints the rich per-command help (examples, env
		// vars, key flags). Bare "octo help" prints the top-level command
		// list. Unknown subcommand → exit 2 so scripts can tell the diff
		// between "user asked for help" and "user typo'd".
		if len(args) > 1 {
			if !printCommandHelp(args[1], stdout) {
				fmt.Fprintf(stderr, "octo help: no help available for %q\n", args[1])
				fmt.Fprintln(stderr, "Run `octo help` to see the command list.")
				return 2
			}
			return 0
		}
		printUsage(stdout)
		return 0
	case "chat":
		return runChat(args[1:], stdin, stdout, stderr)
	case "init":
		return runInit(args[1:], stdin, stdout, stderr)
	case "config":
		return runConfig(args[1:], stdin, stdout, stderr)
	case "memory":
		return runMemory(args[1:], stdout, stderr)
	case "task":
		return runTask(args[1:], stdin, stdout, stderr)
	case "skills":
		return runSkills(args[1:], stdout, stderr)
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	case "__complete":
		// Hidden subcommand the shell-completion scripts call back into.
		// Prints newline-separated candidates for the current command line.
		return runComplete(args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "octo: unknown command %q\n", args[0])
		fmt.Fprintln(stderr, "Run `octo help` for available commands.")
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "octo — a functionality-first AI agent in a single Go binary.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: octo <command>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  chat       Start an interactive session (or single-turn with a message)")
	fmt.Fprintln(w, "  config     Set your default provider/model (~/.octo/config.json)")
	fmt.Fprintln(w, "  init       Analyze the repo and generate/update .octorules")
	fmt.Fprintln(w, "  memory     Manage cross-session memory (e.g. `octo memory list`)")
	fmt.Fprintln(w, "  task       Autonomous task orchestration (M11; `octo task start \"<goal>\"`)")
	fmt.Fprintln(w, "  completion Print shell-completion snippet (bash | zsh | fish)")
	fmt.Fprintln(w, "  version    Print the version and exit")
	fmt.Fprintln(w, "  help       Print this help and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `octo help <command>` for examples + env vars (e.g. `octo help chat`),")
	fmt.Fprintln(w, "or `octo <command> --help` for the full flag list.")
}
