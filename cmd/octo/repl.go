package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/hooks"
	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tools"
)

// replConfig holds everything runREPL needs.
type replConfig struct {
	a           *agent.Agent
	session     *agent.Session
	noSave      bool
	plain       bool               // true → fall back to terse ↳ status lines for all tool events
	verbosity   verbosity          // quiet | normal | verbose; controls spinner + chrome
	permEngine  *permission.Engine // nil → no tool-permission gating
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	tools       []agent.ToolDefinition
	executor    agent.ToolExecutor
	subAgentMgr *tools.SubAgentManager // nil → sub-agent tools disabled
	skillReg    *skills.Registry       // discovered skills; backs /skills and /<name>
	memStore    *memory.Store          // cross-session memory; backs /memory (nil → disabled)
	memRefresh  *memoryRefresher       // live cross-session memory delta; nil → disabled
	hooks       *hooks.Runner          // C9 Phase 3 pre/post-turn hooks; nil-safe via Configured()
	// reader, when non-nil, is the line reader to use instead of building
	// one fresh inside runREPL. Set by cmd/octo so the same instance is
	// shared with the permission gate and the ask_user_question asker.
	// Tests that only pass cfg.stdin leave this nil; runREPL builds a
	// scanner-backed reader over stdin for them.
	reader lineReader
	// view, when non-nil, is the ViewSink driving turn rendering and Ask
	// prompts. Set by cmd/octo so the same instance backs the turn loop, the
	// permission gate, and the asker. Tests leave it nil; runREPL builds a
	// plainView over the resolved reader.
	view ViewSink
}

// isFirstEverSession reports whether no sessions exist on disk yet — the
// signal for a genuinely new user, used to show one-time orientation. Called
// before the current session is saved, so an empty store means first run. A
// read error degrades to false (don't show the hint) rather than guessing.
func isFirstEverSession() bool {
	sessions, err := agent.ListSessions(1)
	return err == nil && len(sessions) == 0
}

// runREPL runs the interactive multi-turn loop until the user exits or EOF.
// It returns 0 on clean exit, 1 on unexpected error.
func runREPL(cfg replConfig) int {
	a := cfg.a
	sess := cfg.session

	// Kill any background processes (terminal background:true) on exit so none
	// outlive the session.
	defer tools.KillAllBackground()

	// Sub-agent manager: wire the onExit hook so completion notifications ride
	// the same steer path as background-process notices. Register the manager
	// globally so the built-in tools pick it up without per-instance injection.
	if cfg.subAgentMgr != nil {
		tools.SetDefaultSubAgentManager(cfg.subAgentMgr)
		cfg.subAgentMgr.SetOnExit(func(ev tools.SubAgentNotification) {
			a.Steer(formatSubAgentNote(ev))
		})
		defer func() {
			cfg.subAgentMgr.SetOnExit(nil)
			tools.SetDefaultSubAgentManager(nil)
			cfg.subAgentMgr.KillAll()
		}()
	}

	// Background-completion notice: inject into the conversation via Steer so
	// the model is told when a detached command finishes (drained at the next
	// tool-batch boundary, or prepended to the next turn — see turncore.go).
	// The plain path prints no async UI line (it would interleave with the
	// synchronous render loop); the TUI path adds a scrollback notice on top.
	tools.SetBackgroundOnExit(func(e tools.BgExit) { a.Steer(formatBgNote(e)) })
	defer tools.SetBackgroundOnExit(nil)

	// Startup banner — fully suppressed in quiet mode, expanded with
	// provider/endpoint context in verbose mode. Normal keeps today's two
	// lines because most users use them to confirm the session ID.
	if !cfg.verbosity.quiet() {
		turns := sess.TurnCount()
		if turns > 0 {
			fmt.Fprintf(cfg.stdout, "Resumed session %s (%d turn", sess.ID, turns)
			if turns != 1 {
				fmt.Fprint(cfg.stdout, "s")
			}
			fmt.Fprintln(cfg.stdout, ")")
		} else {
			fmt.Fprintf(cfg.stdout, "Starting session %s (%s)\n", sess.ID, sess.Model)
			// First-ever session: orient the newcomer once. Suppressed for
			// everyone with prior sessions so it doesn't nag, and only shown
			// when tools are actually on (it describes the tool surface).
			if len(cfg.tools) > 0 && isFirstEverSession() {
				fmt.Fprintln(cfg.stdout, "  Tools are on — I can run shell commands, read/edit files, and search.")
				fmt.Fprintln(cfg.stdout, "  Risky actions ask for your approval first. Run `octo config` to set defaults.")
			}
		}
		if cfg.verbosity.verbose() {
			fmt.Fprintf(cfg.stdout, "  model: %s\n", a.Model)
			if cfg.permEngine != nil {
				fmt.Fprintf(cfg.stdout, "  permissions: %s\n", cfg.permEngine.GetMode())
			}
			if len(cfg.tools) > 0 {
				fmt.Fprintf(cfg.stdout, "  tools: %d enabled\n", len(cfg.tools))
			}
		}
		fmt.Fprintln(cfg.stdout, `/exit, Ctrl-C, or Ctrl-D to quit.`)
		fmt.Fprintln(cfg.stdout)
	}

	// Wrap stdout/stderr in a syncWriter so the input goroutine (which prints
	// prompts) and the turn-rendering goroutine (which prints replies/events)
	// don't race on the same underlying writer (e.g. bytes.Buffer in tests).
	out := cfg.stdout
	errOut := cfg.stderr
	if _, ok := cfg.stdout.(*syncWriter); !ok {
		out = &syncWriter{w: cfg.stdout}
	}
	if _, ok := cfg.stderr.(*syncWriter); !ok {
		errOut = &syncWriter{w: cfg.stderr}
	}
	cfg.stdout = out
	cfg.stderr = errOut

	reader := cfg.reader
	if reader == nil {
		reader = newScannerLineReader(cfg.stdin, out)
	}
	defer reader.Close()

	// Ctrl-C handling: while a turn is running, SIGINT cancels just that turn
	// (the loop catches context.Canceled, finalizes well-formed history, and
	// returns to the prompt). At an idle prompt — no turn in flight — SIGINT
	// behaviour depends on the reader: readline catches it and returns
	// ErrInterrupt (we just re-prompt), while scanner mode falls through to
	// the signal handler which saves and exits.
	var (
		turnMu     sync.Mutex
		turnCancel context.CancelFunc
	)
	setTurnCancel := func(c context.CancelFunc) {
		turnMu.Lock()
		turnCancel = c
		turnMu.Unlock()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			turnMu.Lock()
			c := turnCancel
			turnMu.Unlock()
			if c != nil {
				c() // interrupt the in-flight turn; the loop returns to prompt
				continue
			}
			// Idle prompt with the scanner reader (no built-in SIGINT
			// handling): save and exit. The readline reader catches Ctrl-C
			// itself (returns ErrInterrupt), so this branch never fires in
			// interactive mode — its loop just re-prompts on the next pass.
			if _, ok := reader.(*scannerLineReader); ok {
				fmt.Fprintln(out, "\n^C")
				if !cfg.noSave {
					sess.SyncFrom(a.History)
					_ = sess.Save()
				}
				tools.KillAllBackground()
				os.Exit(0)
			}
		}
	}()

	// The view sink: renders the turn (spinner, tool-event lines, cache/^C/
	// error) and raises Ask prompts. cmd/octo supplies it (so the gate and
	// asker share the same instance); tests leave it nil and get a plainView
	// over the resolved reader.
	view := cfg.view
	if view == nil {
		view = newPlainView(reader, out, errOut, cfg.verbosity, cfg.plain)
	}

	// Permission gating raises its approval prompt through the view (stdin
	// line in plainView, modal in the TUI). Tool dispatch runs synchronously
	// inside RunStream, so the prompt and the loop never race on input.
	if cfg.permEngine != nil {
		a.Gate = &cliPermissionGate{
			engine: cfg.permEngine,
			ask:    view,
		}
	}

	// inputCh carries user input lines from the read goroutine to the main
	// loop. Closed on EOF so the select below exits cleanly.
	//
	// The goroutine never prints prompts itself — prompt rendering is the
	// responsibility of the turn-rendering loop (or the terminal driver for
	// readline). This avoids a data race between prompt printing and turn
	// output on the shared stdout (bytes.Buffer in tests).
	inputCh := make(chan string)
	go func() {
		defer close(inputCh)
		for {
			// Empty prompt so no output is written to stdout from the
			// goroutine. readline manages its own terminal; scanner mode
			// just reads lines without printing a prompt.
			raw, ok := readPromptLine(reader, "", "")
			if !ok {
				if reader.Interrupted() {
					// Ctrl-C at idle: re-prompt. The readline reader already
					// cleared the line; just loop back for the next input.
					continue
				}
				// EOF (Ctrl-D) or read error.
				return
			}
			inputCh <- raw
		}
	}()

	done := false
	for !done {
		var line string
		var isAutoTurn bool

		// Wait for either user input or a background-completion steer that
		// arrived while we were idle. The steer path auto-triggers a turn so
		// the model can react to the event without the user typing anything.
		select {
		case raw, ok := <-inputCh:
			if !ok {
				// EOF — input goroutine exited.
				fmt.Fprintln(cfg.stdout)
				done = true
				continue
			}
			line = strings.TrimSpace(raw)
			if line == "" {
				continue
			}
			if line == "/exit" || line == "/quit" {
				done = true
				continue
			}
		case <-idleSteerWait(a):
			// Background process finished while idle. Drain the steer buffer
			// and run an auto-turn so the model sees the notification.
			line = a.DrainSteer()
			isAutoTurn = true
		}

		if line == "" {
			continue
		}

		// Regular message — streaming turn (or agentic loop when tools enabled).
		// Each turn gets its own cancellable context so SIGINT can interrupt
		// just this turn without tearing down the session. Orchestration
		// (memory nudge, pre/post hooks, the streaming run) lives in runTurn;
		// all rendering flows through the view sink.
		turnCtx, cancelTurn := context.WithCancel(context.Background())
		setTurnCancel(cancelTurn)

		_, err := runTurn(turnCtx, a, cfg, view, line)

		setTurnCancel(nil)
		cancelTurn()

		// A hard (non-interrupt) error left history rolled back by the agent;
		// skip the save and re-prompt. An interrupt (context.Canceled) keeps a
		// well-formed history, so it falls through to auto-save like a success.
		if err != nil && !errors.Is(err, context.Canceled) {
			continue
		}

		// Auto-save after every turn (including interrupted ones — history is
		// well-formed) unless opted out.
		if !cfg.noSave {
			sess.SyncFrom(a.History)
			if err := sess.Save(); err != nil {
				// Non-fatal: warn but don't break the session.
				fmt.Fprintf(cfg.stderr, "(auto-save failed: %v)\n", err)
			}
		}

		// After an auto-turn, briefly yield so the user sees the model's
		// response before we loop back to waiting for input (or another
		// steer). This prevents a rapid-fire sequence of auto-turns from
		// feeling like a wall of text.
		if isAutoTurn {
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Wait for the input goroutine to finish so we don't leak it on exit.
	// (It will exit once it sees EOF or the deferred reader.Close() below.)
	// Drain any remaining input so the goroutine can terminate cleanly.
	for range inputCh {
	}

	// Final save on exit.
	if !cfg.noSave {
		sess.SyncFrom(a.History)
		if err := sess.Save(); err != nil {
			fmt.Fprintf(cfg.stderr, "session save: %v\n", err)
			return 1
		}
		if !cfg.verbosity.quiet() {
			path, _ := sess.SavePath()
			fmt.Fprintf(cfg.stdout, "\nSession saved → %s\n", path)
		}
	}
	return 0
}

// idleSteerWait returns a channel that receives a single value once a steer
// message is pending on the agent. It polls every 200 ms so the select in
// runREPL doesn't block indefinitely on user input when a background process
// completes. The returned channel is closed after firing; callers should not
// reuse it.
func idleSteerWait(a *agent.Agent) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		for {
			if a.HasPendingSteer() {
				ch <- struct{}{}
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return ch
}

// printTuiHelp lists the slash commands the TUI supports. Slash commands live
// only in the TUI; the plain REPL is a pure conversation loop (see runREPL).
func printTuiHelp(w io.Writer) {
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  /help       Show this message")
	fmt.Fprintln(w, "  /init       Analyze the repo and generate/update .octorules (needs --tools)")
	fmt.Fprintln(w, "  /save       Save the session now (it also auto-saves after each turn)")
	fmt.Fprintln(w, "  /sessions   List the 10 most recent sessions")
	fmt.Fprintln(w, "  /skills     List available skills (trigger one with /<name>)")
	fmt.Fprintln(w, "  /memory     List what's remembered across sessions")
	fmt.Fprintln(w, "  /mcp        Show connected MCP servers and their surfaces")
	fmt.Fprintln(w, "  /goal       Plan + run a goal as a task DAG (also: /goal list, /goal resume <id>)")
	fmt.Fprintln(w, "  /exit       Save and exit  (also: /quit, Ctrl-C, Ctrl-D)")
}

// printMemory shows what cross-session memory looks like: active entries
// (not yet consolidated), the consolidated summary (the actual injection
// source), and a pointer to the archive. Off / empty states are reported.
func printMemory(w io.Writer, store *memory.Store) {
	if store == nil {
		fmt.Fprintln(w, "Memory is disabled for this session (--no-memory).")
		return
	}
	active, err := store.List()
	if err != nil {
		fmt.Fprintf(w, "memory: %v\n", err)
		return
	}
	archived, _ := store.ListArchived()
	buckets, _ := store.Summaries()

	if len(active) == 0 && len(buckets) == 0 && len(archived) == 0 {
		fmt.Fprintln(w, "Nothing remembered yet.")
		return
	}

	if len(active) > 0 {
		fmt.Fprintln(w, "Active entries (not yet consolidated):")
		for _, e := range active {
			fmt.Fprintf(w, "  [%-9s] %s\n", e.Type, e.Description)
		}
	}
	for i, sb := range buckets {
		if i == 0 && len(active) > 0 {
			fmt.Fprintln(w)
		}
		if sb.Cwd == "" {
			fmt.Fprintln(w, "Consolidated summary — global (injected every session):")
		} else {
			fmt.Fprintf(w, "Consolidated summary — %s:\n", sb.Cwd)
		}
		for _, line := range strings.Split(sb.Body, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	if len(archived) > 0 {
		fmt.Fprintf(w, "\n(%d archived entr%s — `octo memory list --archive` to view)\n",
			len(archived), pluralEntries(len(archived)))
	}
}

func pluralEntries(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// printMCP lists the connected MCP servers and the surface they advertised
// (tool count + resource count + prompt count, plus the server-supplied
// instructions if non-empty). Drives the /mcp REPL command.
func printMCP(w io.Writer) {
	reg := tools.ActiveMCPRegistry()
	if reg == nil || reg.Len() == 0 {
		fmt.Fprintln(w, "No MCP servers connected.")
		fmt.Fprintln(w, "Configure one at ~/.octo/mcp.json (run `octo help mcp` for the format).")
		return
	}
	conns := reg.Connections()
	if len(conns) == 1 {
		fmt.Fprintln(w, "1 MCP server connected:")
	} else {
		fmt.Fprintf(w, "%d MCP servers connected:\n", len(conns))
	}
	for _, c := range conns {
		info := c.Client.ServerInfo()
		nameVer := info.Name
		if info.Version != "" {
			nameVer = fmt.Sprintf("%s %s", info.Name, info.Version)
		}
		fmt.Fprintf(w, "  %s (%s): %d tool%s, %d resource%s, %d prompt%s\n",
			c.Name, nameVer,
			len(c.Tools), pluralS(len(c.Tools)),
			len(c.Resources), pluralS(len(c.Resources)),
			len(c.Prompts), pluralS(len(c.Prompts)))
		if instr := c.Client.Instructions(); instr != "" {
			for _, line := range strings.Split(instr, "\n") {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// reservedReplCommands are the built-in slash commands; a skill may not shadow
// one (so /help always means help even if a skill dir is named "help").
var reservedReplCommands = map[string]bool{
	"init": true, "exit": true, "quit": true, "help": true,
	"save": true, "sessions": true, "skills": true,
	"memory": true, "mcp": true, "goal": true,
}

// skillTrigger reports whether line is a /<name> invocation of a discovered
// skill (and not a reserved command), returning the skill and any trailing
// args (the text after the command word).
func skillTrigger(reg *skills.Registry, line string) (skills.Skill, string, bool) {
	if reg == nil || !strings.HasPrefix(line, "/") {
		return skills.Skill{}, "", false
	}
	fields := strings.Fields(line)
	name := strings.TrimPrefix(fields[0], "/")
	if reservedReplCommands[strings.ToLower(name)] {
		return skills.Skill{}, "", false
	}
	s, ok := reg.Get(name)
	if !ok {
		return skills.Skill{}, "", false
	}
	args := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	return s, args, true
}

// inlineSkill builds the turn input for an explicit /<name> trigger: the skill
// body, optionally followed by the user's trailing arguments.
func inlineSkill(body, args string) string {
	if args == "" {
		return body
	}
	return body + "\n\nUser input: " + args
}

// printSkills lists the discovered skills, or a hint when there are none.
func printSkills(w io.Writer, reg *skills.Registry) {
	if reg == nil || reg.Len() == 0 {
		fmt.Fprintln(w, "No skills found (looked in ~/.octo/skills and ./.octo/skills).")
		return
	}
	fmt.Fprintln(w, "Available skills (trigger with /<name>):")
	for _, s := range reg.List() {
		fmt.Fprintf(w, "  /%-16s [%-7s] %s\n", s.Name, s.Source, s.Description)
	}
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
	fmt.Fprintln(w, formatSessionList(sessions))
	return nil
}

// formatSessionList renders the rightmost columns the user cares about for a
// "pick one to resume" overview: 8-char short ID (the thing they paste back
// into `octo chat -c`), a human-readable created-at, the model, and the
// turn count. Shared between `octo chat --list-sessions` and REPL /sessions
// so both views agree on shape.
func formatSessionList(sessions []*agent.Session) string {
	var b strings.Builder
	for _, s := range sessions {
		turns := s.TurnCount()
		plural := "s"
		if turns == 1 {
			plural = ""
		}
		when := s.CreatedAt.Local().Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "  %s  %s  %-30s  %d turn%s\n",
			s.ShortID(), when, s.Model, turns, plural)
	}
	// strings.Builder result has no trailing newline trimmed — printSessions
	// uses Fprintln below which would add one if we kept it. Drop the final
	// "\n" we just emitted so the outer caller controls spacing.
	out := b.String()
	return strings.TrimRight(out, "\n")
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
// plain is retained for signature stability but no longer toggles card
// rendering: the plain / non-TTY path is always one-line tool status now;
// rich cards are TUI-only (see dev-docs/tui-ux-upgrade-design.md decision #8).
func replToolEventHandler(stdout io.Writer, plain bool) func(agent.AgentEvent) {
	_ = plain
	// Per-tool-call start times so EventToolDone can report elapsed.
	startedAt := make(map[string]time.Time)
	// Track whether the previous event was a text delta — if so, a tool
	// status line needs a leading newline to start cleanly.
	prevWasText := false
	// Count of `·` typing-indicator dots emitted while the LLM streams the
	// in-flight tool's input arguments. Reset on tool_started.
	inputDots := 0

	return func(ev agent.AgentEvent) {
		switch ev.Kind {
		case agent.EventTextDelta:
			fmt.Fprint(stdout, ev.Text)
			prevWasText = true

		case agent.EventToolInputDelta:
			// One dot per JSON fragment, capped so a 500-line write_file
			// content field doesn't wrap the typing indicator across the
			// terminal. The dots line is closed when tool_started fires.
			if inputDots == 0 {
				if prevWasText {
					fmt.Fprintln(stdout)
					prevWasText = false
				}
				fmt.Fprint(stdout, "⋯ ")
			}
			if inputDots < inputDotsCap {
				fmt.Fprint(stdout, "·")
				inputDots++
			}

		case agent.EventToolStarted:
			if inputDots > 0 {
				// Close the typing-indicator line so the ↳ row starts clean.
				fmt.Fprintln(stdout)
				inputDots = 0
			} else if prevWasText {
				fmt.Fprintln(stdout)
				prevWasText = false
			}
			startedAt[ev.ToolID] = time.Now()
			fmt.Fprintf(stdout, "↳ %s: %s\n", ev.ToolName, summariseInput(ev.Input))

		case agent.EventToolProgress:
			fmt.Fprintf(stdout, "│ %s\n", ev.Chunk)

		case agent.EventToolDone:
			elapsed := time.Duration(0)
			if t, ok := startedAt[ev.ToolID]; ok {
				elapsed = time.Since(t).Round(time.Millisecond)
				delete(startedAt, ev.ToolID)
			}
			fmt.Fprintf(stdout, "↳ %s ✓ (%s)\n", ev.ToolName, elapsed)

		case agent.EventToolError:
			elapsed := time.Duration(0)
			if t, ok := startedAt[ev.ToolID]; ok {
				elapsed = time.Since(t).Round(time.Millisecond)
				delete(startedAt, ev.ToolID)
			}
			fmt.Fprintf(stdout, "↳ %s ✗ (%s) — %s\n", ev.ToolName, elapsed, truncate1Line(ev.Err))

			// EventTurnDone is silent — the trailing newline emitted by the
			// REPL loop after RunStream returns serves as the turn boundary.
		}
	}
}

// inputDotsCap caps the typing indicator at a width that fits in any
// reasonably-sized terminal without wrapping. The exact value isn't
// load-bearing — pick something visually pleasant.
const inputDotsCap = 40

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
