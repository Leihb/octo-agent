// Command octo is the Go implementation of the Octo AI agent.
//
// At this milestone (M1.2) the binary wires up:
//   - `version` / `help` (M1)
//   - `chat` — single-turn Anthropic Messages call, the first end-to-end
//     proof that the agent core, the provider interface, and the
//     Anthropic adapter agree on shapes.
//
// Streaming, tool use, skills, the web server, and IM bridges land in
// M2..M5. See dev-docs/CATCHUP_PLAN.md and the README "🚧 Octo is being
// rewritten in Go" callout for the wider plan.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Leihb/octo/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. Splitting it out keeps main thin and
// lets the test harness drive the CLI without spawning a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "octo %s\n", version.String())
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	case "chat":
		return runChat(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "octo: unknown command %q\n", args[0])
		fmt.Fprintln(stderr, "Run `octo help` for available commands.")
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "octo — a functionality-first AI agent (Go rewrite in progress)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: octo <command>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  chat       Send one message to Anthropic and print the reply")
	fmt.Fprintln(w, "  version    Print the version and exit")
	fmt.Fprintln(w, "  help       Print this help and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `octo chat --help` for chat-specific flags.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "More commands will land as the rewrite progresses. The Ruby")
	fmt.Fprintln(w, "implementation lives on the archive/ruby branch in the meantime.")
}
