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
		return runChat(args, stdin, stdout, stderr)
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
	case "__trash-backup":
		// Internal: the Windows safe-delete wrapper calls this to copy paths
		// into the trash before the real Remove-Item deletes them. Not
		// user-facing.
		return runTrashBackup(args[1:])
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
	case "init":
		return runInit(args[1:], stdin, stdout, stderr)
	case "config":
		return runConfig(args[1:], stdin, stdout, stderr)
	case "memory":
		return runMemory(args[1:], stdout, stderr)
	case "skills":
		return runSkills(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdin, stdout, stderr)
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	case "__complete":
		// Hidden subcommand the shell-completion scripts call back into.
		// Prints newline-separated candidates for the current command line.
		return runComplete(args[1:], stdout)
	default:
		// Anything else — flags or a positional message — is a chat
		// invocation: `octo "fix the bug"`, `octo -c last`, `octo --no-tools`.
		return runChat(args, stdin, stdout, stderr)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "octo — a functionality-first AI agent in a single Go binary.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: octo [flags] [\"message\"]")
	fmt.Fprintln(w, "       octo <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "With no arguments octo starts an interactive session; with a message")
	fmt.Fprintln(w, "(or piped stdin) it runs one headless agentic turn and exits.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, "  octo                                  Start an interactive session")
	fmt.Fprintln(w, "  octo \"summarise the README\"           Headless one-shot, then exit")
	fmt.Fprintln(w, "  echo \"explain this error\" | octo      Prompt from piped stdin")
	fmt.Fprintln(w, "  octo -c last                          Resume the most recent session")
	fmt.Fprintln(w, "  octo --list-sessions                  Show recent sessions and exit")
	fmt.Fprintln(w, "  octo --no-tools                       Plain chat — disable the built-in tools")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Common flags:")
	fmt.Fprintln(w, "  -c, --continue <id>      Resume a session — 'last', short ID, or substring of an ID")
	fmt.Fprintln(w, "  --no-tools               Disable built-in tools (terminal, edit_file, …) + MCP/skills")
	fmt.Fprintln(w, "  --provider <name>        anthropic (default) | openai")
	fmt.Fprintln(w, "  --model <name>           Override the default model for the provider")
	fmt.Fprintln(w, "  --no-save                Don't auto-save the session to ~/.octo/sessions")
	fmt.Fprintln(w, "  --no-memory              Disable cross-session memory injection")
	fmt.Fprintln(w, "  --sandbox                OS-enforced confinement for terminal commands (macOS/Linux)")
	fmt.Fprintln(w, "  --permission-mode <m>    interactive (default; prompts on ask) | strict | auto")
	fmt.Fprintln(w, "  --quiet / --verbose      Less / more status chrome")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  config     Set your default provider/model (~/.octo/config.yml)")
	fmt.Fprintln(w, "  serve      Start the HTTP server (REST + SSE + Web UI)")
	fmt.Fprintln(w, "  init       Analyze the repo and generate/update .octorules")
	fmt.Fprintln(w, "  memory     Manage cross-session memory (e.g. `octo memory list`)")
	fmt.Fprintln(w, "  completion Print shell-completion snippet (bash | zsh | fish)")
	fmt.Fprintln(w, "  version    Print the version and exit")
	fmt.Fprintln(w, "  help       Print this help and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Environment:")
	fmt.Fprintln(w, "  ANTHROPIC_API_KEY / OPENAI_API_KEY      Required for the chosen provider")
	fmt.Fprintln(w, "  ANTHROPIC_BASE_URL / OPENAI_BASE_URL    Override the endpoint (proxies, compatible servers)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `octo help <command>` for examples + env vars (e.g. `octo help mcp`),")
	fmt.Fprintln(w, "`octo <command> --help` for a command's flags, or `octo -help` for all session flags.")
}
