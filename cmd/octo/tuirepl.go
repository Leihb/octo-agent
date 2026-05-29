package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
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
	p := tea.NewProgram(m)
	sink := &tuiSink{prog: p}
	m.sink = sink

	// Gate + asker raise their prompts through the same sink, so they render
	// as modals on this event loop instead of reading stdin (which bubbletea
	// owns). Overrides whatever the plain path wired.
	if cfg.permEngine != nil {
		cfg.a.Gate = &cliPermissionGate{engine: cfg.permEngine, ask: sink}
	}
	tools.SetAsker(newREPLAsker(sink))

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

	if _, err := p.Run(); err != nil {
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
type bgExitMsg struct{ e tools.BgExit } // a background process finished (async)
type tickMsg struct{}                   // animation tick while a turn runs
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

	// input is the single-line edit buffer (manual; no bubbles dependency).
	input []rune

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
	return &tuiModel{cfg: cfg, a: cfg.a, cwd: abbreviateHome(workingDir())}
}

func (m *tuiModel) Init() tea.Cmd { return nil }

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
	var echoCmd tea.Cmd
	if echo != "" && !strings.HasPrefix(echo, "<system-reminder>") {
		echoCmd = tea.Println(promptStyle.Render("you> ") + echo)
	}
	return tea.Batch(echoCmd, tickCmd())
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

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
		return m, tea.Println(noticeStyle.Render(msg.text))

	case bgExitMsg:
		// Async background-process completion: show a one-line scrollback notice
		// (the full output rode into the conversation via Steer). Safe to print
		// mid-turn — it's just another committed line.
		return m, tea.Println(noticeStyle.Render(fmt.Sprintf(
			"↳ %s (%s) %s", msg.e.ID, truncate1Line(msg.e.Command), msg.e.Status)))

	case turnEndedMsg:
		// Flush any trailing assistant block (markdown-rendered); then render
		// the cache/error footer.
		var cmds []tea.Cmd
		if flush := m.flushText(); flush != nil {
			cmds = append(cmds, flush)
		}
		if msg.err != nil && msg.err != context.Canceled {
			cmds = append(cmds, tea.Println(errorStyle.Render("error: "+msg.err.Error())))
		} else if msg.err == context.Canceled {
			cmds = append(cmds, tea.Println(noticeStyle.Render("^C interrupted")))
		} else if c := cacheLine(m.cfg.verbosity, msg.reply); c != "" {
			cmds = append(cmds, tea.Println(c))
		}
		return m, tea.Batch(cmds...)

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
		return m, tea.Println(noticeStyle.Render(fmt.Sprintf(
			"Cancelled. Planned as %s — run later with /goal resume %s (or octo goal run %s).",
			msg.task.ShortID(), msg.task.ShortID(), msg.task.ShortID())))

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
		var cmds []tea.Cmd
		for _, line := range strings.Split(complete, "\n") {
			cmds = append(cmds, tea.Println(line))
		}
		return tea.Batch(cmds...)
	}

	commit, rest := splitCommittableMarkdown(m.partial.String())
	if commit == "" {
		return nil
	}
	m.partial.Reset()
	m.partial.WriteString(rest)
	return tea.Println(m.md.render(commit, m.width))
}

// flushText commits whatever assistant text is still buffered (the final or
// pre-tool block), rendered through glamour unless --plain. Returns nil when
// nothing is pending.
func (m *tuiModel) flushText() tea.Cmd {
	p := m.partial.String()
	if p == "" {
		return nil
	}
	m.partial.Reset()
	if m.cfg.plain {
		return tea.Println(p)
	}
	return tea.Println(m.md.render(p, m.width))
}

// commitToolLine flushes any in-progress text, then prints the tool line/card.
func (m *tuiModel) commitToolLine(line string) tea.Cmd {
	if flush := m.flushText(); flush != nil {
		return tea.Batch(flush, tea.Println(line))
	}
	return tea.Println(line)
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
		m.queue = append([]pendingItem{{text: s}}, m.queue...)
	}

	// Dequeue the next pending turn, if any.
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m, m.startTurn(next.text)
	}
	return m, nil
}
