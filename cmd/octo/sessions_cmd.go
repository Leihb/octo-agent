package main

import (
	"fmt"
	"io"
	"strings"

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
	fmt.Fprintln(stdout, "Resume with `octo -c <id>`, or bare `octo -c` to pick from a list.")
	return 0
}

// pickSessionSentinel is the value normalizeBareContinue inserts for a bare
// -c / --continue so flag parsing succeeds; runChat turns it into the
// interactive session picker. NUL can't collide with a real session ID.
const pickSessionSentinel = "\x00pick"

// normalizeBareContinue lets -c / --continue appear with no ID, meaning "pick
// a session interactively". The std flag package requires string flags to
// carry a value, so insert the sentinel when the flag is the last argument or
// the next one is another flag.
func normalizeBareContinue(args []string) []string {
	out := make([]string, 0, len(args)+1)
	for i, a := range args {
		out = append(out, a)
		if a == "-c" || a == "--continue" || a == "-continue" {
			if i == len(args)-1 || strings.HasPrefix(args[i+1], "-") {
				out = append(out, pickSessionSentinel)
			}
		}
	}
	return out
}

// sessionSelectItems renders sessions as picker rows: short ID + title as the
// label, created-at / model / turn count as the dimmed annotation. value
// carries the full ID so the resume path resolves it by exact match.
func sessionSelectItems(sessions []*agent.Session) []selectItem {
	items := make([]selectItem, 0, len(sessions))
	for _, s := range sessions {
		turns := s.TurnCount()
		plural := "s"
		if turns == 1 {
			plural = ""
		}
		items = append(items, selectItem{
			label: fmt.Sprintf("%s  %s", s.ShortID(), padCol(s.DisplayTitle(), 40)),
			desc:  fmt.Sprintf("%s  %s  %d turn%s", s.CreatedAt.Local().Format("2006-01-02 15:04"), s.Model, turns, plural),
			value: s.ID,
		})
	}
	return items
}
