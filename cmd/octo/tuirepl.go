package main

import (
	"context"
	"fmt"
	"strings"

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
type askMsg struct {
	prompt UserPrompt
	resp   chan UserResponse
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

	// queue holds Alt+Enter messages to run as future turns (design §8/§10).
	queue []pendingItem

	// modal, when non-nil, is an active Ask prompt (design §6).
	modal *modalState

	width int
	quit  bool
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
	return &tuiModel{cfg: cfg, a: cfg.a}
}

func (m *tuiModel) Init() tea.Cmd { return nil }

// startTurn launches a turn in a background goroutine driven by runTurn. The
// sink streams events back as tea.Msgs; turnFinishedMsg fires when runTurn
// returns so Update can save, drain steer, and dequeue.
func (m *tuiModel) startTurn(line string) tea.Cmd {
	m.turnRunning = true
	m.streaming = false
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
	// surfaced as a bg notice and isn't user input.
	if strings.HasPrefix(line, "<system-reminder>") {
		return nil
	}
	return tea.Println(promptStyle.Render("you> ") + line)
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
		// Flush any trailing partial line; render the cache/error footer.
		var cmds []tea.Cmd
		if line := m.partial.String(); line != "" {
			cmds = append(cmds, tea.Println(line))
			m.partial.Reset()
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
		return m.commitToolLine(fmt.Sprintf("↳ %s: %s", ev.ToolName, summariseInput(ev.Input)))
	case agent.EventToolDone:
		return m.commitToolLine(fmt.Sprintf("↳ %s ✓", ev.ToolName))
	case agent.EventToolError:
		return m.commitToolLine(toolErrStyle.Render(fmt.Sprintf("↳ %s ✗ — %s", ev.ToolName, truncate1Line(ev.Err))))
	case agent.EventToolProgress:
		return m.commitToolLine("│ " + ev.Chunk)
	}
	return nil
}

// appendText buffers a text delta, emitting any newline-terminated lines to
// the scrollback and keeping the remainder live.
func (m *tuiModel) appendText(text string) tea.Cmd {
	m.partial.WriteString(text)
	buf := m.partial.String()
	idx := strings.LastIndexByte(buf, '\n')
	if idx < 0 {
		return nil
	}
	complete := buf[:idx] // up to and excluding the last newline
	m.partial.Reset()
	m.partial.WriteString(buf[idx+1:])
	var cmds []tea.Cmd
	for _, line := range strings.Split(complete, "\n") {
		cmds = append(cmds, tea.Println(line))
	}
	return tea.Batch(cmds...)
}

// commitToolLine flushes any in-progress text line, then prints the tool line.
func (m *tuiModel) commitToolLine(line string) tea.Cmd {
	var cmds []tea.Cmd
	if p := m.partial.String(); p != "" {
		cmds = append(cmds, tea.Println(p))
		m.partial.Reset()
	}
	cmds = append(cmds, tea.Println(line))
	return tea.Batch(cmds...)
}

func (m *tuiModel) handleTurnFinished() (tea.Model, tea.Cmd) {
	m.turnRunning = false
	m.cancelTurn = nil
	m.streaming = false

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
