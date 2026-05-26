package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// replConfig holds everything runREPL needs.
type replConfig struct {
	a        *agent.Agent
	session  *agent.Session
	noSave   bool
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	tools    []agent.ToolDefinition
	executor agent.ToolExecutor
}

// runREPL runs the interactive multi-turn loop until the user exits or EOF.
// It returns 0 on clean exit, 1 on unexpected error.
func runREPL(cfg replConfig) int {
	a := cfg.a
	sess := cfg.session

	turns := sess.TurnCount()
	if turns > 0 {
		fmt.Fprintf(cfg.stdout, "Resumed session %s (%d turn", sess.ID, turns)
		if turns != 1 {
			fmt.Fprint(cfg.stdout, "s")
		}
		fmt.Fprintln(cfg.stdout, ")")
	} else {
		fmt.Fprintf(cfg.stdout, "Starting session %s (%s)\n", sess.ID, sess.Model)
	}
	fmt.Fprintln(cfg.stdout, `Type /help for commands, Ctrl-C or /exit to quit.`)
	fmt.Fprintln(cfg.stdout)

	scanner := bufio.NewScanner(cfg.stdin)
	for {
		fmt.Fprint(cfg.stdout, "you> ")

		if !scanner.Scan() {
			// EOF (Ctrl-D) or read error.
			fmt.Fprintln(cfg.stdout)
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Slash commands.
		if strings.HasPrefix(line, "/") {
			cmd := strings.ToLower(strings.Fields(line)[0])
			switch cmd {
			case "/exit", "/quit":
				break
			case "/help":
				printReplHelp(cfg.stdout)
				continue
			case "/cost":
				printCost(cfg.stdout, a)
				continue
			case "/save":
				if err := saveSession(cfg.stdout, sess, a); err != nil {
					fmt.Fprintf(cfg.stderr, "save: %v\n", err)
				}
				continue
			case "/sessions":
				if err := printSessions(cfg.stdout); err != nil {
					fmt.Fprintf(cfg.stderr, "sessions: %v\n", err)
				}
				continue
			default:
				fmt.Fprintf(cfg.stdout, "Unknown command %q. Type /help for a list.\n", cmd)
				continue
			}
			// /exit or /quit falls through here.
			break
		}

		// Regular message — streaming turn (or agentic loop when tools enabled).
		startedAt := time.Now()
		var (
			reply agent.Reply
			err   error
		)
		if len(cfg.tools) > 0 && cfg.executor != nil {
			// Tool events become inline status lines so the user can see what
			// the agent is doing instead of staring at a blank terminal while
			// a tool runs. Text deltas stream as before. Output is muted on
			// EventToolDone — the tool's own product (file written, command
			// stdout, etc.) is conversational state for the LLM, not user-
			// facing chrome. EventTurnDone is also silent; the trailing
			// newline below marks the visible turn boundary.
			reply, err = a.RunStream(context.Background(), line, cfg.tools, cfg.executor, replToolEventHandler(cfg.stdout))
		} else {
			reply, err = a.TurnStream(context.Background(), line, func(delta string) {
				fmt.Fprint(cfg.stdout, delta)
			})
		}
		if err != nil {
			fmt.Fprintf(cfg.stderr, "\nerror: %v\n", err)
			continue
		}
		_ = startedAt
		_ = reply
		fmt.Fprintln(cfg.stdout) // newline after streamed reply

		// Auto-save after every turn unless opted out.
		if !cfg.noSave {
			sess.SyncFrom(a.History)
			if err := sess.Save(); err != nil {
				// Non-fatal: warn but don't break the session.
				fmt.Fprintf(cfg.stderr, "(auto-save failed: %v)\n", err)
			}
		}
	}

	// Final save on exit.
	if !cfg.noSave {
		sess.SyncFrom(a.History)
		if err := sess.Save(); err != nil {
			fmt.Fprintf(cfg.stderr, "session save: %v\n", err)
			return 1
		}
		path, _ := sess.SavePath()
		fmt.Fprintf(cfg.stdout, "\nSession saved → %s\n", path)
	}
	return 0
}

func printReplHelp(w io.Writer) {
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  /help       Show this message")
	fmt.Fprintln(w, "  /cost       Show token usage and estimated cost for this session")
	fmt.Fprintln(w, "  /save       Save the session now (it also auto-saves after each turn)")
	fmt.Fprintln(w, "  /sessions   List the 10 most recent sessions")
	fmt.Fprintln(w, "  /exit       Save and exit  (also: /quit, Ctrl-C, Ctrl-D)")
}

func printCost(w io.Writer, a *agent.Agent) {
	in, out := a.SessionTokens()
	cost := a.SessionCostUSD()
	fmt.Fprintf(w, "Tokens: %d in / %d out  |  est. $%.6f\n", in, out, cost)
}

func saveSession(w io.Writer, sess *agent.Session, a *agent.Agent) error {
	sess.SyncFrom(a.History)
	if err := sess.Save(); err != nil {
		return err
	}
	path, _ := sess.SavePath()
	fmt.Fprintf(w, "Saved → %s\n", path)
	return nil
}

func printSessions(w io.Writer) error {
	sessions, err := agent.ListSessions(10)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(w, "No saved sessions.")
		return nil
	}
	fmt.Fprintln(w, "Recent sessions (newest first):")
	for _, s := range sessions {
		turns := s.TurnCount()
		fmt.Fprintf(w, "  %s  %-30s  %d turn", s.ID, s.Model, turns)
		if turns != 1 {
			fmt.Fprint(w, "s")
		}
		fmt.Fprintln(w)
	}
	return nil
}

// replToolEventHandler returns an EventHandler that paints tool activity
// onto the terminal between assistant text streams. Layout:
//
//	…assistant text streamed…
//	↳ terminal: ls -la                                          ← tool_started
//	↳ terminal ✓ (142ms)                                        ← tool_done
//	…more assistant text…
//
// Status lines start with "↳ " so they're visually distinct from the
// assistant's reply, and each is on its own line. We also insert a leading
// newline before the FIRST status line of a turn — without it the marker
// would butt up against the trailing character of the assistant's
// "I'll now run terminal..." sentence.
func replToolEventHandler(stdout io.Writer) func(agent.AgentEvent) {
	// Per-tool-call start times so EventToolDone can report elapsed.
	startedAt := make(map[string]time.Time)
	// Track whether the previous event was a text delta — if so, a tool
	// status line needs a leading newline to start cleanly.
	prevWasText := false

	return func(ev agent.AgentEvent) {
		switch ev.Kind {
		case agent.EventTextDelta:
			fmt.Fprint(stdout, ev.Text)
			prevWasText = true

		case agent.EventToolStarted:
			if prevWasText {
				fmt.Fprintln(stdout)
				prevWasText = false
			}
			startedAt[ev.ToolID] = time.Now()
			fmt.Fprintf(stdout, "↳ %s: %s\n", ev.ToolName, summariseInput(ev.Input))

		case agent.EventToolDone:
			elapsed := ""
			if t, ok := startedAt[ev.ToolID]; ok {
				elapsed = fmt.Sprintf(" (%s)", time.Since(t).Round(time.Millisecond))
				delete(startedAt, ev.ToolID)
			}
			fmt.Fprintf(stdout, "↳ %s ✓%s\n", ev.ToolName, elapsed)

		case agent.EventToolError:
			elapsed := ""
			if t, ok := startedAt[ev.ToolID]; ok {
				elapsed = fmt.Sprintf(" (%s)", time.Since(t).Round(time.Millisecond))
				delete(startedAt, ev.ToolID)
			}
			fmt.Fprintf(stdout, "↳ %s ✗%s — %s\n", ev.ToolName, elapsed, truncate1Line(ev.Err))

			// EventTurnDone is silent — the trailing newline emitted by the
			// REPL loop after RunStream returns serves as the turn boundary.
		}
	}
}

// summariseInput renders a short single-line description of the tool's input
// for inline display. Keys are sorted so the line is stable; long values are
// truncated. The goal is "enough for the user to know which call this is",
// not full fidelity.
func summariseInput(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := fmt.Sprintf("%v", input[k])
		if len(v) > 60 {
			v = v[:57] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	joined := strings.Join(parts, " ")
	if len(joined) > 120 {
		joined = joined[:117] + "..."
	}
	return joined
}

// truncate1Line collapses a multi-line error to its first non-empty line and
// caps total length, so the status row stays single-line.
func truncate1Line(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:197] + "..."
		}
		return line
	}
	return "(empty error)"
}
