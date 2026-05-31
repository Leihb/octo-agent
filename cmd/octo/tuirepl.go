package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// runTUI is the interactive bubbletea REPL: the TTY counterpart to runREPL's
// plain-text loop. The event loop owns the main goroutine; each turn runs in a
// background goroutine driven by runTurn through a tuiSink, which marshals the
// agent's streaming events back onto the loop as tea.Msgs and blocks the
// agent goroutine on a channel for Ask prompts (modals). See
// dev-docs/tui-input-modes-design.md §3, §6.
//
// Input modes while a turn runs (design §7):
//   - Enter              → steer  (folded into the turn at the next tool-batch boundary)
//   - Shift+Enter        → newline (on terminals that emit CSI-u sequences)
//   - Alt+Enter          → newline (traditional escape sequence)
//   - Ctrl+J             → newline (LF — works on all terminals)
//   - Ctrl+Q             → queue  (run as a fresh turn after this one finishes)
//   - Esc                → interrupt the current turn (queue survives)
//   - Ctrl+C             → interrupt if a turn runs, else save & quit
//   - Ctrl+D             → save & quit
func runTUI(cfg replConfig) int {
	defer tools.KillAllBackground()

	m := newTUIModel(cfg)
	p := tea.NewProgram(m, tea.WithFilter(shiftEnterFilter))
	sink := &tuiSink{prog: p}
	m.sink = sink

	// Eagerly probe terminal background colour before bubbletea owns stdin.
	// lipgloss.AdaptiveColor sends an OSC 11 query on first use; if that
	// happens after p.Run() the terminal's response (e.g. "11;rgb:1e1e/1e1e/2e2e\\")
	// leaks into bubbletea's input reader and shows up as apparent user input.
	//
	// tui.IsDark() guards the probe with sync.Once, so calling it here warms
	// the cache. The result is also passed to markdownRenderer so glamour can
	// use an explicit dark/light style instead of WithAutoStyle(), which would
	// fire its own (uncached) termenv query later.
	_ = tui.IsDark()

	// Gate + asker raise their prompts through the same sink, so they render
	// as modals on this event loop instead of reading stdin (which bubbletea
	// owns). Overrides whatever the plain path wired.
	if cfg.permEngine != nil {
		cfg.a.Gate = &cliPermissionGate{engine: cfg.permEngine, ask: sink}
	}
	tools.SetAsker(newREPLAsker(sink))

	// Sub-agent manager: wire the onExit hook so completion notifications ride
	// the same inbox path as background-process notices, and send a TUI msg
	// for scrollback display.
	if cfg.subAgentMgr != nil {
		tools.SetDefaultSubAgentManager(cfg.subAgentMgr)
		cfg.subAgentMgr.SetOnExit(func(ev tools.SubAgentNotification) {
			cfg.a.Inbox.Enqueue(formatSubAgentNote(ev))
			p.Send(subAgentNoteMsg{ev})
		})
		defer func() {
			cfg.subAgentMgr.SetOnExit(nil)
			tools.SetDefaultSubAgentManager(nil)
			cfg.subAgentMgr.KillAll()
		}()
	}

	// Background-process completion notifications. Fired from a process's
	// waiter goroutine, so each path is goroutine-safe: Inbox.Enqueue is
	// mutex-guarded, and prog.Send marshals the scrollback notice onto the
	// event loop. Without this a finished background command is invisible
	// until the model polls terminal_output.
	tools.SetBackgroundOnExit(func(e tools.BgExit) {
		cfg.a.Inbox.Enqueue(formatBgNote(e))
		p.Send(bgExitMsg{e})
	})
	defer tools.SetBackgroundOnExit(nil)

	_, err := p.Run()
	if err != nil {
		fmt.Fprintf(cfg.stderr, "octo chat: tui: %v\n", err)
		return 1
	}

	// Final save on exit (mirrors runREPL's exit save).
	if !cfg.noSave {
		cfg.session.SyncFrom(cfg.a.History)
		if err := cfg.session.Save(); err != nil {
			fmt.Fprintf(cfg.stderr, "session save: %v\n", err)
			return 1
		}
	}
	if !cfg.verbosity.quiet() {
		fmt.Fprintf(cfg.stdout, "\nSession saved. Resume anytime with: octo chat -c %s\n", cfg.session.ShortID())
	}
	return 0
}

// ── messages marshalled from the agent goroutine onto the event loop ──

type turnStartedMsg struct{}
type agentEventMsg struct{ ev agent.AgentEvent }
type turnEndedMsg struct {
	reply agent.Reply
	err   error
}
type turnFinishedMsg struct{ err error } // the turn goroutine returned
type noticeMsg struct{ text string }
type bgExitMsg struct{ e tools.BgExit }                      // a background process finished (async)
type subAgentNoteMsg struct{ ev tools.SubAgentNotification } // a sub-agent completed (async)
type tickMsg struct{}                                        // animation tick while a turn runs
type askMsg struct {
	prompt UserPrompt
	resp   chan UserResponse
}

// tickInterval drives the spinner / elapsed-clock animation. ~8 Hz is smooth
// without being wasteful; it only runs while a turn is in flight.
const tickInterval = 120 * time.Millisecond

// shiftEnterFilter intercepts Kitty-keyboard-protocol CSI sequences that
// bubbletea v1 does not natively understand (e.g. Shift+Enter = \x1b[13;2u)
// and converts them into plain KeyMsg events the rest of the app already
// handles. This is a no-op on terminals that do not emit CSI-u sequences.
func shiftEnterFilter(_ tea.Model, msg tea.Msg) tea.Msg {
	// unknownCSISequenceMsg is unexported in bubbletea, so we identify it by
	// reflection. Its underlying type is []byte.
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return msg
	}
	seq := string(v.Bytes())
	switch seq {
	case "\x1b[13;2u": // Shift+Enter (kitty CSI u)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "\x1b[13;3u": // Alt+Enter (CSI-u form)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "\x1b[13;5u": // Ctrl+Enter (CSI-u form)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "\x1b[27;2;13~": // Shift+Enter (xterm modifyOtherKeys)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "\x1b[27;3;13~": // Alt+Enter (modifyOtherKeys)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "\x1b[27;5;13~": // Ctrl+Enter (modifyOtherKeys)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "\x1b[27;6;13~": // Shift+Ctrl+Enter (modifyOtherKeys)
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	}
	return msg
}

// tickCmd schedules the next animation tick.
func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// tuiSink implements ViewSink by sending each call onto the bubbletea program.
// Ask blocks the calling (agent) goroutine until the modal is answered or the
// turn's context is cancelled.
type tuiSink struct {
	prog interface{ Send(tea.Msg) }
}

func (s *tuiSink) TurnStarted()             { s.prog.Send(turnStartedMsg{}) }
func (s *tuiSink) Emit(ev agent.AgentEvent) { s.prog.Send(agentEventMsg{ev}) }
func (s *tuiSink) Notice(msg string)        { s.prog.Send(noticeMsg{msg}) }
func (s *tuiSink) TurnEnded(r agent.Reply, e error) {
	s.prog.Send(turnEndedMsg{reply: r, err: e})
}

func (s *tuiSink) Ask(ctx context.Context, p UserPrompt) (UserResponse, error) {
	ch := make(chan UserResponse, 1)
	s.prog.Send(askMsg{prompt: p, resp: ch})
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		// Turn interrupted while the modal was open — deny/cancel gracefully.
		return UserResponse{Cancelled: true}, nil
	}
}

// ── pending queue ──

type pendingItem struct {
	text string
}

// ── model ──

type tuiModel struct {
	cfg  replConfig
	a    *agent.Agent
	sink *tuiSink

	// md renders committed assistant markdown blocks (glamour). Skipped under
	// --plain (cfg.plain), where text commits as raw lines.
	md markdownRenderer

	// ta is the multi-line text input (bubbles/textarea).
	ta textarea.Model

	// inputHistory stores submitted lines for ↑/↓ recall.
	inputHistory    []string
	inputHistoryIdx int // -1 = not browsing, 0..len-1 = browsing

	// turnRunning is true between starting a turn and turnFinishedMsg.
	turnRunning bool
	cancelTurn  context.CancelFunc

	// partial holds the in-progress assistant text not yet committed to the
	// scrollback.  In the TUI the raw partial is rendered live in the View()
	// area so the user sees tokens arrive in real time; only complete blocks
	// (identified by splitCommittableMarkdown) are promoted to printlnBuf and
	// flushed to the terminal scrollback via tea.Println.
	partial strings.Builder
	// streaming tracks whether the current turn has emitted any output yet
	// (drives the "thinking…" placeholder).
	streaming bool

	// toolInput caches each tool call's input from EventToolStarted so the
	// matching EventToolDone can render a card (tool_result events don't carry
	// the input back). Keyed by ToolID; entry removed on done/error.
	toolInput map[string]map[string]any

	// queue holds Ctrl+Q messages to run as future turns (design §8/§10).
	queue []pendingItem

	// pendingSteer holds steer messages typed during a running turn that
	// haven't been drained yet. Shown in the live View area (below the
	// scrollback) so the user sees immediate feedback without breaking
	// the chronological message order (Claude Code style).
	pendingSteer []string

	// modal, when non-nil, is an active Ask prompt (design §6).
	modal *modalState

	width int
	quit  bool

	// cwd is the home-abbreviated working directory shown in the status bar
	// (computed once — it doesn't change during a session).
	cwd string
	// turnStart timestamps the current turn so the status bar can show elapsed.
	turnStart time.Time

	// spinnerFrame advances on every tickMsg while a turn runs, animating the
	// thinking placeholder, the running-tool indicator, and the elapsed clock.
	spinnerFrame int
	// running, when non-nil, is a card tool executing right now — shown as a
	// live spinner line until its done event commits the finished card. (Card
	// tools suppress their started line, so without this they'd show nothing
	// until completion.)
	running *runningTool

	// printlnBuf accumulates lines to emit via tea.Println at the end of each
	// Update cycle. In inline mode, committed output goes to the terminal
	// scrollback above the live View() area — no alt-screen buffer needed.
	printlnBuf []string

	// height is the terminal height in cells, updated by WindowSizeMsg.
	height int
}

// runningTool is the live indicator state for an in-flight card tool.
type runningTool struct {
	verb   string
	target string
	start  time.Time
}

// modalState renders a permission / question prompt and collects the answer.
type modalState struct {
	prompt UserPrompt
	resp   chan UserResponse
	// cursor is the highlighted option (question mode); options include the
	// trailing "Other (free text)" slot.
	cursor   int
	options  []string // rendered option labels (questions only)
	selected map[int]bool
}

func newTUIModel(cfg replConfig) *tuiModel {
	ta := textarea.New()
	ta.Placeholder = "Ask anything…"
	ta.Focus()
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	// Disable up/down line navigation so we can use them for input-history
	// recall instead.
	ta.KeyMap.LineNext = key.Binding{}
	ta.KeyMap.LinePrevious = key.Binding{}
	style := "dark"
	if !tui.IsDark() {
		style = "light"
	}
	m := &tuiModel{cfg: cfg, a: cfg.a, cwd: abbreviateHome(workingDir()), ta: ta, inputHistoryIdx: -1, md: markdownRenderer{style: style}}
	_ = m.updateTextAreaHeight()
	return m
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Sequence(
		tea.Println(tui.Banner("", m.a.Model, m.cwd, m.width)),
		textarea.Blink,
	)
}

// startTurn launches a turn whose transcript echo is the submitted line itself.
func (m *tuiModel) startTurn(line string) tea.Cmd {
	return m.startTurnEcho(line, line)
}

// startTurnEcho launches a turn in a background goroutine driven by runTurn. The
// sink streams events back as tea.Msgs; turnFinishedMsg fires when runTurn
// returns so Update can save, drain steer, and dequeue. echo is the transcript
// line shown to the user — pass a short label (not line) when line is an
// expanded prompt that shouldn't be dumped verbatim (e.g. /init, /<skill>);
// pass "" to suppress the echo entirely.
func (m *tuiModel) startTurnEcho(line, echo string) tea.Cmd {
	m.turnRunning = true
	m.streaming = false
	m.turnStart = time.Now()
	m.spinnerFrame = 0
	m.running = nil
	m.partial.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	a := m.a
	cfg := m.cfg
	sink := m.sink
	prog := m.sink.prog
	go func() {
		_, err := runTurn(ctx, a, cfg, sink, line)
		cancel()
		prog.Send(turnFinishedMsg{err: err})
	}()
	// Echo the submitted message into the scrollback so it reads like a
	// transcript — except a degraded background-notice "turn", which already
	// surfaced as a bg notice and isn't user input. Either way, start the
	// animation ticker for the spinner + elapsed clock.
	if echo != "" && !strings.HasPrefix(echo, "<system-reminder>") {
		m.println(userEchoStyle.Render("> ") + echo)
	}
	return tickCmd()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ta.SetWidth(msg.Width - 4) // account for border + padding
		_ = m.updateTextAreaHeight()
		return m, m.flushPrints()

	case tea.KeyMsg:
		return m.handleKey(msg)

	case turnStartedMsg:
		return m, m.flushPrints()

	case tickMsg:
		// Animate while a turn runs OR while background processes are still
		// going (so the live "background (N running)" panel keeps ticking even
		// between turns); let the ticker die once both are quiet.
		if !m.turnRunning && len(tools.RunningBackground()) == 0 {
			return m, m.flushPrints()
		}
		m.spinnerFrame++
		return m, tickCmd()

	case agentEventMsg:
		m.handleEvent(msg.ev)
		return m, m.flushPrints()

	case noticeMsg:
		m.println(noticeStyle.Render(msg.text))
		return m, m.flushPrints()

	case bgExitMsg:
		// Async background-process completion: show a one-line scrollback notice
		// (the full output rode into the conversation via Inbox).
		m.println(noticeStyle.Render(fmt.Sprintf(
			"↳ %s (%s) %s", msg.e.ID, truncate1Line(msg.e.Command), msg.e.Status)))
		// Idle auto-turn: if no turn is running and nothing is queued, drain the
		// inbox (which holds the full <system-reminder> notice) and start a
		// turn so the model sees the completion immediately — matching the plain
		// REPL's idleInboxWait behaviour.
		if !m.turnRunning && len(m.queue) == 0 {
			if msgs := m.a.Inbox.Drain(); len(msgs) > 0 {
				s := strings.Join(msgs, "\n\n")
				return m, m.startTurnEcho(s, "")
			}
		}
		return m, m.flushPrints()

	case subAgentNoteMsg:
		// Async sub-agent completion: show a one-line scrollback notice (the
		// full result rode into the conversation via Steer).
		label := "completed"
		if msg.ev.Kind == "message_reply" {
			label = "replied"
		}
		m.println(noticeStyle.Render(fmt.Sprintf(
			"↳ sub-agent %s (%s) %s", msg.ev.AgentID, truncate1Line(msg.ev.Description), label)))
		// Idle auto-turn: same logic as bgExitMsg — drain inbox and trigger a
		// turn so the model sees the notification immediately.
		if !m.turnRunning && len(m.queue) == 0 {
			if msgs := m.a.Inbox.Drain(); len(msgs) > 0 {
				s := strings.Join(msgs, "\n\n")
				return m, m.startTurnEcho(s, "")
			}
		}
		return m, m.flushPrints()

	case turnEndedMsg:
		// Flush any trailing assistant block (markdown-rendered); then render
		// the cache/error footer.
		if s, ok := m.flushTextString(); ok {
			m.println(s)
		}
		if msg.err != nil && msg.err != context.Canceled {
			m.println(errorStyle.Render("error: " + msg.err.Error()))
		} else if c := cacheLine(m.cfg.verbosity, msg.reply); c != "" {
			m.println(c)
		}
		// On interrupt, eagerly reset turnRunning so background-process
		// auto-turn (bgExitMsg) can fire immediately instead of waiting for
		// turnFinishedMsg. The inbox drain and dequeue still happen in
		// handleTurnFinished when the goroutine actually returns.
		if msg.err == context.Canceled {
			m.turnRunning = false
			m.running = nil
			// Eagerly drain inbox so a pending message (or background-process
			// notice that raced in via Inbox) starts immediately rather than
			// waiting for turnFinishedMsg.
			if msgs := m.a.Inbox.Drain(); len(msgs) > 0 {
				s := strings.Join(msgs, "\n\n")
				for _, line := range strings.Split(s, "\n\n") {
					m.println(userEchoStyle.Render("> ") + line)
					if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != line {
						m.inputHistory = append(m.inputHistory, line)
					}
				}
				m.queue = append([]pendingItem{{text: s}}, m.queue...)
			}
			if len(m.queue) > 0 {
				next := m.queue[0]
				m.queue = m.queue[1:]
				return m, tea.Sequence(m.flushPrints(), m.startTurnEcho(next.text, ""))
			}
		}
		return m, m.flushPrints()

	case turnFinishedMsg:
		return m.handleTurnFinished()

	case askMsg:
		m.openModal(msg)
		return m, m.flushPrints()

	case goalPlannedMsg:
		return m.onGoalPlanned(msg)

	case goalRunMsg:
		return m, m.startGoalRun(msg.task.ID)

	case goalCancelledMsg:
		m.turnRunning = false
		m.cancelTurn = nil
		m.println(noticeStyle.Render(fmt.Sprintf(
			"Cancelled. Planned as %s — run later with /goal resume %s (or octo goal run %s).",
			msg.task.ShortID(), msg.task.ShortID(), msg.task.ShortID())))
		return m, m.flushPrints()

	case goalDoneMsg:
		return m.onGoalDone(msg)
	}
	return m, m.flushPrints()
}

// handleEvent updates model state for a streaming agent event.  Text deltas
// are accumulated in m.partial so they render live in View(); only complete
// markdown blocks are promoted to the scrollback via println/flushPrints.
// Tool events flush any pending text first, then commit their line/card.
func (m *tuiModel) handleEvent(ev agent.AgentEvent) {
	m.streaming = true
	switch ev.Kind {
	case agent.EventTextDelta:
		m.appendText(ev.Text)
		return

	case agent.EventToolStarted:
		if m.toolInput == nil {
			m.toolInput = map[string]map[string]any{}
		}
		m.toolInput[ev.ToolID] = ev.Input
		// Card tools render their whole story in the done card; suppress the
		// started line and instead show a live spinner indicator in the View
		// region (animated by the ticker) until the done event commits the card.
		if m.rendersCard(ev.ToolName) {
			m.running = &runningTool{
				verb:   cardVerbFor(ev.ToolName),
				target: cardTargetFor(ev.ToolName, ev.Input),
				start:  time.Now(),
			}
			return
		}
		m.commitToolLine(fmt.Sprintf("↳ %s: %s", ev.ToolName, summariseInput(ev.Input)))
		return

	case agent.EventToolProgress:
		// Card tools defer all output to the done card; dropping progress avoids
		// noise above the card.
		if m.rendersCard(ev.ToolName) {
			return
		}
		m.commitToolLine("│ " + ev.Chunk)
		return

	case agent.EventToolDone:
		input := m.toolInput[ev.ToolID]
		delete(m.toolInput, ev.ToolID)
		m.running = nil // the finished card replaces the live indicator
		if m.rendersCard(ev.ToolName) {
			m.commitToolLine(renderToolCard(ev.ToolName, input, ev.Output, false))
			return
		}
		m.commitToolLine(fmt.Sprintf("↳ %s ✓", ev.ToolName))
		return

	case agent.EventToolError:
		input := m.toolInput[ev.ToolID]
		delete(m.toolInput, ev.ToolID)
		m.running = nil
		if m.rendersCard(ev.ToolName) {
			m.commitToolLine(renderToolCard(ev.ToolName, input, firstNonEmpty(ev.Output, ev.Err), true))
			return
		}
		m.commitToolLine(toolErrStyle.Render(fmt.Sprintf("↳ %s ✗ — %s", ev.ToolName, truncate1Line(ev.Err))))
		return
	}
}

// rendersCard reports whether the TUI should render this tool as a rich card.
// --plain (cfg.plain) forces one-line status for every tool, matching the
// plain/headless path; otherwise card tools (cardVerbFor) get a card.
func (m *tuiModel) rendersCard(toolName string) bool {
	return !m.cfg.plain && cardVerbFor(toolName) != ""
}

// appendText buffers a streamed text delta into m.partial.  The partial is
// rendered live in View() so the user sees tokens arrive in real time.
// When a complete block boundary is found (blank line outside a code fence),
// that prefix is promoted to the scrollback via println/flushPrints so it
// becomes part of the terminal's native scrollback history.
func (m *tuiModel) appendText(text string) {
	m.partial.WriteString(text)

	if m.cfg.plain {
		buf := m.partial.String()
		idx := strings.LastIndexByte(buf, '\n')
		if idx < 0 {
			return
		}
		complete := buf[:idx]
		m.partial.Reset()
		m.partial.WriteString(buf[idx+1:])
		m.println(complete)
		return
	}

	commit, rest := splitCommittableMarkdown(m.partial.String())
	if commit == "" {
		return
	}
	m.partial.Reset()
	m.partial.WriteString(rest)
	m.println(m.md.render(commit, m.width))
}

// flushText commits whatever assistant text is still buffered (the final or
// pre-tool block), rendered through glamour unless --plain. Returns nil when
// nothing is pending.
func (m *tuiModel) flushText() tea.Cmd {
	s, ok := m.flushTextString()
	if !ok {
		return nil
	}
	m.println(s)
	return nil
}

// flushTextString returns the buffered assistant text (rendered through glamour
// unless --plain) and true when there was pending text. Used internally when
// multiple lines must be emitted in strict order.
func (m *tuiModel) flushTextString() (string, bool) {
	p := m.partial.String()
	if p == "" {
		return "", false
	}
	m.partial.Reset()
	if m.cfg.plain {
		return p, true
	}
	return m.md.render(p, m.width), true
}

// commitToolLine flushes any in-progress text to the scrollback, then appends
// the tool line/card.
func (m *tuiModel) commitToolLine(line string) {
	if s, ok := m.flushTextString(); ok {
		m.println(s)
	}
	m.println(line)
}

// println queues a line for output to the terminal scrollback via
// tea.Println, emitted by flushPrints at the end of each Update cycle.
// In inline mode, committed lines scroll naturally above the live View() area.
func (m *tuiModel) println(line string) {
	m.printlnBuf = append(m.printlnBuf, line)
}

// flushPrints returns a Cmd that prints all queued lines to the terminal
// in order, above the live View() area.
func (m *tuiModel) flushPrints() tea.Cmd {
	if len(m.printlnBuf) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, len(m.printlnBuf))
	for i, line := range m.printlnBuf {
		cmds[i] = tea.Println(line)
	}
	m.printlnBuf = nil
	return tea.Sequence(cmds...)
}

func (m *tuiModel) handleTurnFinished() (tea.Model, tea.Cmd) {
	m.turnRunning = false
	m.cancelTurn = nil
	m.streaming = false
	m.running = nil      // clear any live tool indicator (e.g. on interrupt)
	m.pendingSteer = nil // clear pending steer display (drained or degraded)

	// Auto-save (history is well-formed even after an interrupt).
	if !m.cfg.noSave {
		m.cfg.session.SyncFrom(m.a.History)
		_ = m.cfg.session.Save()
	}

	// Drain any inbox messages that weren't consumed during the turn and
	// run them as the next turn, ahead of explicitly-queued items.
	if msgs := m.a.Inbox.Drain(); len(msgs) > 0 {
		s := strings.Join(msgs, "\n\n")
		for _, line := range strings.Split(s, "\n\n") {
			m.println(userEchoStyle.Render("> ") + line)
			if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != line {
				m.inputHistory = append(m.inputHistory, line)
			}
		}
		m.queue = append([]pendingItem{{text: s}}, m.queue...)
	}

	// Dequeue the next pending turn, if any.
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m, m.startTurnEcho(next.text, "") // echo already shown above
	}
	return m, nil
}
