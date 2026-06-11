package main

import (
	"fmt"
	"io"

	"github.com/Leihb/octo-agent/internal/agent"
)

// runSessions handles `octo sessions`: print the most recent saved sessions
// so the user can pick an ID for `octo -c <id>`. The CLI twin of the TUI's
// /sessions command (it replaced the old --list-sessions flag).
func runSessions(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: octo sessions")
		return 2
	}
	sessions, err := agent.ListSessions(10)
	if err != nil {
		fmt.Fprintf(stderr, "octo sessions: %v\n", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No saved sessions.")
		return 0
	}
	fmt.Fprintln(stdout, "Recent sessions (newest first):")
	fmt.Fprintln(stdout, formatSessionList(sessions))
	fmt.Fprintln(stdout, "Resume with `octo -c <id>` (or `octo -c last`).")
	return 0
}
