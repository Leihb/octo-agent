// Command octo is the Go implementation of the Octo AI agent.
//
// At this scaffolding stage the binary only resolves the version subcommand;
// the agent loop, providers, tools, skills, web server, and IM bridges all
// land in subsequent milestones (M1..M5). See dev-docs/CATCHUP_PLAN.md and
// the README "🚧 Octo is being rewritten in Go" callout for the migration
// plan.
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
	default:
		fmt.Fprintf(stderr, "octo: unknown command %q (the Go rewrite is in early scaffolding — only `version` is wired up)\n", args[0])
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
	fmt.Fprintln(w, "  version    Print the version and exit")
	fmt.Fprintln(w, "  help       Print this help and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "More commands will land as the rewrite progresses. The Ruby")
	fmt.Fprintln(w, "implementation lives on the archive/ruby branch in the meantime.")
}
