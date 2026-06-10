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
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/hooks"
	"github.com/Leihb/octo-agent/internal/mcp"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/charmbracelet/lipgloss"
)

// replConfig holds everything runREPL needs.
type replConfig struct {
	a           *agent.Agent
	session     *agent.Session
	noSave      bool
	suggest     bool               // true → after each turn, offer an LLM follow-up suggestion (TUI ghost text)
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
	memDir      string                 // per-repo memory directory; backs /memory ("" → disabled)
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
	// mcpBoot, when non-nil, tells runTUI to connect the MCP servers in the
	// background after first paint instead of blocking startup on them. Only
	// the TUI path sets it; the headless one-shot connects synchronously
	// (its single turn needs the full tool surface up front). nil → no MCP.
	mcpBoot *mcpBootstrap
	// modelName is the resolved model displayed in the TUI status bar.
	modelName string
	// reasoningEffort is the resolved reasoning level ("low" | "medium" | "high" | "")
	// displayed in the TUI status bar; empty means off.
	reasoningEffort string
	// providerName is the resolved provider (e.g. "anthropic", "openai") used
	// to rebuild the sender when the user switches model or thinking level.
	providerName string
}

// mcpBootstrap carries the inputs runTUI needs to connect MCP servers from a
// background tea.Cmd: the resolved config, our client identity, and the writer
// that receives stdio servers' child stderr (a log file under the TUI).
type mcpBootstrap struct {
	cfg      *mcp.Config
	info     mcp.Implementation
	childErr io.Writer
}

// isFirstEverSession reports whether no sessions exist on disk yet — the
// signal for a genuinely new user, used to show one-time orientation. Called
// before the current session is saved, so an empty store means first run. A
// read error degrades to false (don't show the hint) rather than guessing.
func isFirstEverSession() bool {
	sessions, err := agent.ListSessions(1)
	return err == nil && len(sessions) == 0
}

// runOnce executes a single agentic turn headlessly, then exits — octo's
// claude -p-style mode. It backs every non-TUI invocation: a positional
// message, --prompt-file, or piped stdin. The full tool loop runs (tools are
// on by default); rendering, the spinner, and permission/Ask prompts all flow
// through the same plainView the interactive path used. Interactive multi-turn
// now lives only in the TUI.
//
// One-shot does not persist a session (matching the original single-turn mode);
// resuming with -c stays a TUI-only affordance.
//
// stream=true (the default) renders the turn live through the view; stream=false
// runs the same agentic loop but prints only the final reply text to stdout,
// keeping it clean for capture (`octo chat --stream=false ... > out`).
func runOnce(cfg replConfig, prompt string, stream bool) int {
	a := cfg.a

	// Reap background processes the turn spawned so none outlive the run.
	defer tools.KillAllBackground()
	defer tools.CleanSpillFiles()

	// Sub-agent completions and background-process notices ride the inbox, so a
	// later iteration of this single agentic turn picks them up (the agent
	// drains the inbox at the start of each loop step).
	if cfg.subAgentMgr != nil {
		tools.SetDefaultSubAgentManager(cfg.subAgentMgr)
		cfg.subAgentMgr.SetOnExit(func(ev tools.SubAgentNotification) {
			a.Inbox.Enqueue(tools.FormatSubAgentNote(ev))
		})
		defer func() {
			cfg.subAgentMgr.SetOnExit(nil)
			tools.SetDefaultSubAgentManager(nil)
			cfg.subAgentMgr.KillAll()
		}()
	}
	tools.SetBackgroundOnExit(func(e tools.BgExit) { a.Inbox.Enqueue(tools.FormatBgNote(e)) })
	defer tools.SetBackgroundOnExit(nil)

	// The view sink renders the turn (spinner, streamed text, tool-event lines,
	// cache/error) and raises Ask prompts. With a TTY reader (a positional
	// message typed at a terminal) permission prompts are interactive; over a
	// pipe the reader is exhausted, so plainView auto-denies — the headless
	// posture.
	view := cfg.view
	if view == nil {
		view = newPlainView(cfg.reader, cfg.stdout, cfg.stderr, cfg.verbosity, cfg.plain)
	}
	if cfg.permEngine != nil {
		a.Gate = newCLIGate(cfg.permEngine, view)
	}

	// SIGINT cancels the in-flight turn; the agent finalizes well-formed history
	// and runTurn returns context.Canceled, which we treat as a clean stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	// Buffered: run the full agentic loop silently, then print just the final
	// reply text. Bypasses the live view (and its spinner / tool lines) so
	// captured stdout carries only the answer.
	if !stream {
		reply, err := a.Run(ctx, prompt, cfg.tools, cfg.executor)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return 0
			}
			fmt.Fprintf(cfg.stderr, "octo chat: %v\n", err)
			return 1
		}
		fmt.Fprintln(cfg.stdout, reply.Content)
		if !cfg.verbosity.quiet() {
			printUsageLine(cfg.stderr, reply)
		}
		return 0
	}

	_, err := runTurn(ctx, a, cfg, view, prompt)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 1
	}
	return 0
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
	fmt.Fprintln(w, "  /conduct    Conduct a goal to completion, unattended (also: /conduct list, /conduct resume <id>)")
	fmt.Fprintln(w, "  /exit       Save and exit  (also: /quit, Ctrl-C, Ctrl-D)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Keys:")
	fmt.Fprintln(w, "  Ctrl+V      Paste an image from the clipboard (rides your next message; Esc discards)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Type / to open the completion menu (Tab/↑↓ select, Enter complete) — it lists")
	fmt.Fprintln(w, "every command and skill, so you don't have to remember names.")
}

// printMemory lists the project's memory directory: MEMORY.md (the index
// injected every session) plus any topic files the agent has created. The
// agent reads/writes/edits these with its file tools; this is just a viewer.
func printMemory(w io.Writer, memDir string) {
	if memDir == "" {
		fmt.Fprintln(w, "Memory is disabled for this session (--no-memory).")
		return
	}
	fmt.Fprintf(w, "Memory directory: %s\n", memDir)
	entries, err := os.ReadDir(memDir)
	if err != nil || len(entries) == 0 {
		fmt.Fprintln(w, "  (empty — nothing remembered yet)")
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(w, "  %s/\n", e.Name())
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			fmt.Fprintf(w, "  %s\n", e.Name())
			continue
		}
		fmt.Fprintf(w, "  %-28s %5dB\n", e.Name(), info.Size())
	}
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
	"memory": true, "mcp": true, "conduct": true,
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
		// Re-scan once in case the skill was added after session start: the
		// frozen manifest won't list it, but the user can still trigger it by
		// name. Mirrors the skill tool's reload-on-miss; the rescan only runs
		// on a non-reserved /command that didn't match a known skill.
		reg.Reload()
		s, ok = reg.Get(name)
	}
	if !ok {
		return skills.Skill{}, "", false
	}
	args := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	return s, args, true
}

// inlineSkill builds the turn input for an explicit /<name> trigger: the skill
// rendered with its directory header (so referenced files resolve) plus any
// trailing user arguments. Same text the `skill` tool returns for a
// model-initiated load — see skills.RenderSkill.
func inlineSkill(s skills.Skill, args string) string {
	return skills.RenderSkill(s, args)
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

// formatSessionList renders the columns the user cares about for a "pick one to
// resume" overview: 8-char short ID (the thing they paste back into
// `octo chat -c`), a human-readable created-at, the title (generated, or a
// first-message fallback so older sessions stay recognisable), the model, and
// the turn count. Shared between `octo chat --list-sessions` and REPL /sessions
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
		// padCol (not %-40s) because titles are often CJK: %-Ns pads by byte/rune
		// count, which over- or under-pads double-width runes and leaves the model
		// column ragged.
		title := padCol(s.DisplayTitle(), 40)
		fmt.Fprintf(&b, "  %s  %s  %s  %-22s  %d turn%s\n",
			s.ShortID(), when, title, s.Model, turns, plural)
	}
	// strings.Builder result has no trailing newline trimmed — printSessions
	// uses Fprintln below which would add one if we kept it. Drop the final
	// "\n" we just emitted so the outer caller controls spacing.
	out := b.String()
	return strings.TrimRight(out, "\n")
}

// padCol collapses whitespace in s, truncates it to maxW display columns
// (appending an ellipsis when cut), and right-pads with spaces to exactly maxW
// columns. It measures with lipgloss.Width so wide (CJK) runes count as two
// columns, keeping the following column aligned.
func padCol(s string, maxW int) string {
	s = strings.Join(strings.Fields(s), " ")
	if lipgloss.Width(s) > maxW {
		r := []rune(s)
		for len(r) > 0 && lipgloss.Width(string(r))+1 > maxW {
			r = r[:len(r)-1]
		}
		s = string(r) + "…"
	}
	if pad := maxW - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
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
	// thinkingOpen tracks the dimmed reasoning trace: opened on the first
	// thinking delta (ESC[2m + 💭), closed (ESC[0m + newline) before the first
	// non-thinking output so the answer starts clean and undimmed.
	thinkingOpen := false
	closeThinking := func() {
		if thinkingOpen {
			fmt.Fprint(stdout, "\x1b[0m\n")
			thinkingOpen = false
		}
	}

	return func(ev agent.AgentEvent) {
		// Any non-thinking event ends the dimmed trace first.
		if ev.Kind != agent.EventThinkingDelta {
			closeThinking()
		}
		switch ev.Kind {
		case agent.EventThinkingDelta:
			if !thinkingOpen {
				fmt.Fprint(stdout, "\x1b[2m\U0001F4AD ")
				thinkingOpen = true
			}
			fmt.Fprint(stdout, ev.Text)

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
