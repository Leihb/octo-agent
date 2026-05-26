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

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "octo %s\n", version.String())
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	case "chat":
		return runChat(args[1:], stdin, stdout, stderr)
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
}
