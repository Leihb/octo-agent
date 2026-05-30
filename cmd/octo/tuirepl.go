package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
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
//   - Enter      → steer  (folded into the turn at the next tool-batch boundary)
//   - Alt+Enter  → queue  (run as a fresh turn after this one finishes)
//   - Esc        → interrupt the current turn (queue survives)
//   - Ctrl+C     → interrupt if a turn runs, else save & quit
//   - Ctrl+D     → save & quit
func runTUI(cfg replConfig) int {
	defer tools.KillAllBackground()

	m := newTUIModel(cfg)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion())
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
	// the same steer path as background-process notices, and send a TUI msg
	// for scrollback display.
	if cfg.subAgentMgr != nil {
		tools.SetDefaultSubAgentManager(cfg.subAgentMgr)
		cfg.subAgentMgr.SetOnExit(func(ev tools.SubAgentNotification) {
			cfg.a.Steer(formatSubAgentNote(ev))
			p.Send(subAgentNoteMsg{ev})
		})
		defer func() {
			cfg.subAgentMgr.SetOnExit(nil)
			tools.SetDefaultSubAgentManager(nil)
			cfg.subAgentMgr.KillAll()
		}()
	}

	// Background-process completion notifications. Fired from a process's
	// waiter goroutine, so each path is goroutine-safe: Steer is mutex-guarded
	// (folds the notice into the next tool-batch boundary or turn), and
	// prog.Send marshals the scrollback notice onto the event loop. Without
	// this a finished background command is invisible until the model polls
	// terminal_output.
	tools.SetBackgroundOnExit(func(e tools.BgExit) {
		cfg.a.Steer(formatBgNote(e))
		p.Send(bgExitMsg{e})
	})
	defer tools.SetBackgroundOnExit(nil)

	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(cfg.stderr, "octo chat: tui: %v\n", err)
		return 1
	}

	// Dump scrollback to the main screen after alt-screen exits so the
	// conversation history survives the session.
	if m, ok := finalModel.(*tuiModel); ok && len(m.scrollback) > 0 {
		fmt.Println(strings.Join(m.scrollback, "\n"))
	}

	// Final save on exit (mirrors runREPL's exit save).
	if !cfg.noSave {
		cfg.session.SyncFrom(cfg.a.History)
		if err := cfg.session.Save(); err != nil {
			fmt.Fprintf(cfg.stderr, "session save: %v\n", err)
			return 1
		}
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

	// ti is the single-line text input (bubbles/textinput).
	ti textinput.Model

	// inputHistory stores submitted lines for ↑/↓ recall.
	inputHistory    []string
	inputHistoryIdx int // -1 = not browsing, 0..len-1 = browsing

	// turnRunning is true between starting a turn and turnFinishedMsg.
	turnRunning bool
	cancelTurn  context.CancelFunc

	// partial holds the in-progress assistant line not yet committed to the
	// scrollback (committed line-by-line on '\n').
	partial strings.Builder
	// streaming tracks whether the current turn has emitted any output yet
	// (drives the "thinking…" placeholder).
	streaming bool

	// toolInput caches each tool call's input from EventToolStarted so the
	// matching EventToolDone can render a card (tool_result events don't carry
	// the input back). Keyed by ToolID; entry removed on done/error.
	toolInput map[string]map[string]any

	// queue holds Alt+Enter messages to run as future turns (design §8/§10).
	queue []pendingItem

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

	// scrollback holds committed transcript lines (user messages, assistant
	// text, tool cards, notices) rendered inside the alt-screen below the banner.
	// On exit the buffer is dumped back to the main screen so history survives.
	scrollback []string

	// scrollOffset is the number of lines the user has scrolled up in the
	// scrollback (0 = pinned to bottom, showing the newest content).
	scrollOffset int

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
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "Ask anything…"
	ti.Focus()
	// Disable bubbles' built-in up/down (suggestion navigation) so we can use
	// them for input-history recall instead.
	ti.KeyMap.NextSuggestion = key.Binding{}
	ti.KeyMap.PrevSuggestion = key.Binding{}
	style := "dark"
	if !tui.IsDark() {
		style = "light"
	}
	return &tuiModel{cfg: cfg, a: cfg.a, cwd: abbreviateHome(workingDir()), ti: ti, inputHistoryIdx: -1, md: markdownRenderer{style: style}}
}

func (m *tuiModel) Init() tea.Cmd { return textinput.Blink }

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
		m.pushScrollback(userEchoStyle.Render("> ") + echo)
	}
	return tickCmd()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ti.Width = msg.Width - 4 // account for border + padding
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case turnStartedMsg:
		return m, nil

	case tickMsg:
		// Animate while a turn runs OR while background processes are still
		// going (so the live "background (N running)" panel keeps ticking even
		// between turns); let the ticker die once both are quiet.
		if !m.turnRunning && len(tools.RunningBackground()) == 0 {
			return m, nil
		}
		m.spinnerFrame++
		return m, tickCmd()

	case agentEventMsg:
		return m, m.handleEvent(msg.ev)

	case noticeMsg:
		m.pushScrollback(noticeStyle.Render(msg.text))
		return m, nil

	case bgExitMsg:
		// Async background-process completion: show a one-line scrollback notice
		// (the full output rode into the conversation via Steer).
		m.pushScrollback(noticeStyle.Render(fmt.Sprintf(
			"↳ %s (%s) %s", msg.e.ID, truncate1Line(msg.e.Command), msg.e.Status)))
		// Idle auto-turn: if no turn is running and nothing is queued, drain the
		// steer buffer (which holds the full <system-reminder> notice) and start a
		// turn so the model sees the completion immediately — matching the plain
		// REPL's idleSteerWait behaviour (runREPL select on idleSteerWait).
		if !m.turnRunning && len(m.queue) == 0 {
			if s := m.a.DrainSteer(); s != "" {
				return m, m.startTurnEcho(s, "")
			}
		}
		return m, nil

	case subAgentNoteMsg:
		// Async sub-agent completion: show a one-line scrollback notice (the
		// full result rode into the conversation via Steer).
		label := "completed"
		if msg.ev.Kind == "message_reply" {
			label = "replied"
		}
		m.pushScrollback(noticeStyle.Render(fmt.Sprintf(
			"↳ sub-agent %s (%s) %s", msg.ev.AgentID, truncate1Line(msg.ev.Description), label)))
		// Idle auto-turn: same logic as bgExitMsg — drain steer and trigger a
		// turn so the model sees the notification immediately.
		if !m.turnRunning && len(m.queue) == 0 {
			if s := m.a.DrainSteer(); s != "" {
				return m, m.startTurnEcho(s, "")
			}
		}
		return m, nil

	case turnEndedMsg:
		// Flush any trailing assistant block (markdown-rendered); then render
		// the cache/error footer.
		if s, ok := m.flushTextString(); ok {
			m.pushScrollback(s)
		}
		if msg.err != nil && msg.err != context.Canceled {
			m.pushScrollback(errorStyle.Render("error: " + msg.err.Error()))
		} else if msg.err == context.Canceled {
			m.pushScrollback(noticeStyle.Render("^C interrupted"))
		} else if c := cacheLine(m.cfg.verbosity, msg.reply); c != "" {
			m.pushScrollback(c)
		}
		return m, nil

	case turnFinishedMsg:
		return m.handleTurnFinished()

	case askMsg:
		m.openModal(msg)
		return m, nil

	case goalPlannedMsg:
		return m.onGoalPlanned(msg)

	case goalRunMsg:
		return m, m.startGoalRun(msg.task.ID)

	case goalCancelledMsg:
		m.turnRunning = false
		m.cancelTurn = nil
		m.pushScrollback(noticeStyle.Render(fmt.Sprintf(
			"Cancelled. Planned as %s — run later with /goal resume %s (or octo goal run %s).",
			msg.task.ShortID(), msg.task.ShortID(), msg.task.ShortID())))
		return m, nil

	case goalDoneMsg:
		return m.onGoalDone(msg)
	}
	return m, nil
}

// handleEvent commits streaming text and tool events to the scrollback,
// line-by-line, returning a tea.Println Cmd for each completed line.
func (m *tuiModel) handleEvent(ev agent.AgentEvent) tea.Cmd {
	m.streaming = true
	switch ev.Kind {
	case agent.EventTextDelta:
		return m.appendText(ev.Text)

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
			return nil
		}
		return m.commitToolLine(fmt.Sprintf("↳ %s: %s", ev.ToolName, summariseInput(ev.Input)))

	case agent.EventToolProgress:
		// Card tools defer all output to the done card; dropping progress avoids
		// noise above the card.
		if m.rendersCard(ev.ToolName) {
			return nil
		}
		return m.commitToolLine("│ " + ev.Chunk)

	case agent.EventToolDone:
		input := m.toolInput[ev.ToolID]
		delete(m.toolInput, ev.ToolID)
		m.running = nil // the finished card replaces the live indicator
		if m.rendersCard(ev.ToolName) {
			return m.commitToolLine(renderToolCard(ev.ToolName, input, ev.Output, false))
		}
		return m.commitToolLine(fmt.Sprintf("↳ %s ✓", ev.ToolName))

	case agent.EventToolError:
		input := m.toolInput[ev.ToolID]
		delete(m.toolInput, ev.ToolID)
		m.running = nil
		if m.rendersCard(ev.ToolName) {
			return m.commitToolLine(renderToolCard(ev.ToolName, input, firstNonEmpty(ev.Output, ev.Err), true))
		}
		return m.commitToolLine(toolErrStyle.Render(fmt.Sprintf("↳ %s ✗ — %s", ev.ToolName, truncate1Line(ev.Err))))

	case agent.EventSteerInjected:
		// Steer text is displayed at the exact moment it enters history,
		// after the tool_result it was folded into. This preserves
		// chronological order — the user sees their steer where the model
		// actually receives it, not prematurely when they typed it.
		// Also add to inputHistory so ↑ can recall each steer line.
		for _, line := range strings.Split(ev.Text, "\n\n") {
			if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != line {
				m.inputHistory = append(m.inputHistory, line)
			}
		}
		m.pushScrollback(userEchoStyle.Render("> ") + ev.Text)
		return nil
	}
	return nil
}

// rendersCard reports whether the TUI should render this tool as a rich card.
// --plain (cfg.plain) forces one-line status for every tool, matching the
// plain/headless path; otherwise card tools (cardVerbFor) get a card.
func (m *tuiModel) rendersCard(toolName string) bool {
	return !m.cfg.plain && cardVerbFor(toolName) != ""
}

// appendText buffers a streamed text delta. Under --plain it commits whole
// lines as they complete (raw). Otherwise it accumulates markdown and commits
// complete blocks (up to the last blank line outside a code fence) through
// glamour, keeping the in-progress block live in the View region.
func (m *tuiModel) appendText(text string) tea.Cmd {
	m.partial.WriteString(text)

	if m.cfg.plain {
		buf := m.partial.String()
		idx := strings.LastIndexByte(buf, '\n')
		if idx < 0 {
			return nil
		}
		complete := buf[:idx]
		m.partial.Reset()
		m.partial.WriteString(buf[idx+1:])
		m.pushScrollback(complete)
		return nil
	}

	commit, rest := splitCommittableMarkdown(m.partial.String())
	if commit == "" {
		return nil
	}
	m.partial.Reset()
	m.partial.WriteString(rest)
	m.pushScrollback(m.md.render(commit, m.width))
	return nil
}

// flushText commits whatever assistant text is still buffered (the final or
// pre-tool block), rendered through glamour unless --plain. Returns nil when
// nothing is pending.
func (m *tuiModel) flushText() tea.Cmd {
	s, ok := m.flushTextString()
	if !ok {
		return nil
	}
	m.pushScrollback(s)
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

// commitToolLine flushes any in-progress text, then appends the tool line/card
// to the scrollback.
func (m *tuiModel) commitToolLine(line string) tea.Cmd {
	if s, ok := m.flushTextString(); ok {
		m.pushScrollback(s)
	}
	m.pushScrollback(line)
	return nil
}

// pushScrollback appends a line to the internal scrollback buffer.
func (m *tuiModel) pushScrollback(line string) {
	m.scrollback = append(m.scrollback, line)
}

func (m *tuiModel) handleTurnFinished() (tea.Model, tea.Cmd) {
	m.turnRunning = false
	m.cancelTurn = nil
	m.streaming = false
	m.running = nil // clear any live tool indicator (e.g. on interrupt)

	// Auto-save (history is well-formed even after an interrupt).
	if !m.cfg.noSave {
		m.cfg.session.SyncFrom(m.a.History)
		_ = m.cfg.session.Save()
	}

	// Degrade-to-queue: a steer that never hit a tool-batch boundary runs as
	// the next turn, ahead of explicitly-queued items (design §8).
	if s := m.a.DrainSteer(); s != "" {
		// Steer was never injected into a tool_result — show it now so the
		// user sees it before the degraded turn starts, and add it to
		// inputHistory so ↑ can recall it for re-editing.
		for _, line := range strings.Split(s, "\n\n") {
			m.pushScrollback(userEchoStyle.Render("> ") + line)
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
