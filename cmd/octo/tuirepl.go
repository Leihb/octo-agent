package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/mcp"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
//   - Esc                → take the turn back if no output yet (text returns to the input), else interrupt; queue survives
//   - Ctrl+C             → interrupt if a turn runs, else save & quit
//   - Ctrl+D             → save & quit
func runTUI(cfg replConfig) int {
	defer tools.KillAllBackground()
	defer tools.CleanSpillFiles()
	// MCP connects in the background (mcpBoot → connectMCPCmd → mcpReadyMsg),
	// which installs the registry globally. Tear it down here on exit since the
	// TUI path owns its lifecycle (chat.go only defers cleanup for the
	// synchronous headless connect). nil-safe when no servers connected.
	defer func() {
		if reg := tools.ActiveMCPRegistry(); reg != nil {
			tools.SetMCPRegistry(nil)
			reg.Close()
		}
	}()

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
		cfg.a.Gate = newCLIGate(cfg.permEngine, sink)
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
		// Runtime activity (started + per-tool) for the live bottom panel.
		cfg.subAgentMgr.SetOnEvent(func(ev tools.SubAgentEvent) {
			p.Send(subAgentEventMsg{ev})
		})
		defer func() {
			cfg.subAgentMgr.SetOnExit(nil)
			cfg.subAgentMgr.SetOnEvent(nil)
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
		cfg.a.Inbox.Enqueue(tools.FormatBgNote(e))
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

// inputPlaceholder is the idle hint shown in the empty input box. It's swapped
// for a pending follow-up suggestion (ghost text) when one is available.
const inputPlaceholder = "Ask anything…"

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
type mcpReadyMsg struct{ reg *mcp.Registry }                 // background MCP connect finished (async)
type subAgentEventMsg struct{ ev tools.SubAgentEvent }       // a sub-agent's runtime activity (async)
type suggestionMsg struct{ text string }                     // an after-turn follow-up suggestion (async)
type titleMsg struct{ text string }                          // a generated session title (async)
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
	// blocks carries image attachments that rode a steer which wasn't drained
	// in-loop (e.g. enqueued in the narrow window as a turn was ending). They
	// are attached to the next user message via AttachUserBlocks on dequeue so
	// the image is never silently dropped.
	blocks []agent.ContentBlock
}

// ── model ──

// pendingAttachment is an image captured from the clipboard, waiting to be
// folded into the next user message. block carries the bytes for the model;
// label is the human-readable chip text (e.g. "image (PNG, 84 KB)").
type pendingAttachment struct {
	block agent.ContentBlock
	label string
}

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
	// inputDraft holds the text that was in the input box before the user
	// started browsing history with ↑, so ↓ can restore it.
	inputDraft string

	// Slash-command completion menu. complItems is the current filtered
	// candidate set (built-in commands + skills); non-empty means the menu is
	// open. complIdx is the highlighted row. Recomputed on every keystroke when
	// the input is a bare "/command" token (see updateCompletion).
	complItems []complItem
	complIdx   int

	// turnRunning is true between starting a turn and turnFinishedMsg.
	turnRunning bool
	cancelTurn  context.CancelFunc

	// echoPending is the user-message echo held in the live View() area instead
	// of being committed straight to the scrollback. It commits on the turn's
	// first output (commitEcho) so the message lands above the assistant reply,
	// or is dropped if the user hits Esc before the model responds. Empty means
	// nothing deferred (already committed, or a turn with no user echo).
	echoPending string
	// echoRestore is the raw typed text put back into the input box when the
	// user takes the turn back with Esc before any output. Empty when the turn
	// isn't a typed user message (e.g. a skill, /init, or a dequeued item).
	echoRestore string

	// partial holds the in-progress assistant text not yet committed to the
	// scrollback.  In the TUI the raw partial is rendered live in the View()
	// area so the user sees tokens arrive in real time; only complete blocks
	// (identified by splitCommittableMarkdown) are promoted to printlnBuf and
	// flushed to the terminal scrollback via tea.Println.
	partial strings.Builder

	// thinkPartial holds the in-progress reasoning trace not yet committed to
	// the scrollback. Complete lines are flushed dimmed; the trailing partial
	// line is flushed when the answer (or a tool event) begins.
	thinkPartial strings.Builder
	// thinkingStarted marks that this turn's reasoning trace has begun, so the
	// 💭 prefix is emitted once rather than per line. Reset each turn.
	thinkingStarted bool

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

	// pendingAttachments holds images pasted with Ctrl+V, waiting to ride the
	// next user turn. Shown as chips above the input; cleared on submit (sent)
	// or Esc (discarded).
	pendingAttachments []pendingAttachment

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

	// toolStream* track a tool call whose arguments are still streaming from the
	// model — e.g. a large write_file whose whole content arrives as one long
	// input_json_delta. Without a readout the turn shows only the generic
	// "Thinking" spinner for many seconds, reading as a freeze; instead View
	// shows a live "Writing… N KB" line whose byte counter visibly grows.
	// toolStreamID detects when a new tool's args start (reset the counter).
	toolStreamID    string
	toolStreamName  string
	toolStreamBytes int

	// compacting is set between EventCompactStarted and EventCompactDone while
	// history compaction runs. It drives a dedicated live spinner line so the
	// user sees the conversation isn't frozen while the summary streams in.
	compacting       bool
	compactStart     time.Time
	compactTokens    int    // running estimate of the summary generated so far
	compactMaxTokens int    // the summary's output-token cap (denominator)
	compactPreview   string // tail of the streamed summary, shown under the spinner

	// printlnBuf accumulates lines to emit via tea.Println at the end of each
	// Update cycle. In inline mode, committed output goes to the terminal
	// scrollback above the live View() area — no alt-screen buffer needed.
	printlnBuf []string

	// historyReplayed guards the one-time replay of a resumed session's prior
	// turns into the scrollback. Done on the first WindowSizeMsg so markdown
	// wraps to the real terminal width rather than the Init-time fallback.
	historyReplayed bool

	// height is the terminal height in cells, updated by WindowSizeMsg.
	height int

	// suggestion is a pending after-turn follow-up suggestion shown as ghost
	// text in the empty input box (the textarea placeholder). Accepted with
	// Tab/→, cleared when a turn starts. Empty means none pending.
	suggestion string

	// titlePending guards the one-shot async title generation so a second turn
	// finishing before the first title returns doesn't fire it twice.
	titlePending bool

	// subAgents holds the live state of currently-running sub-agents, keyed by
	// manager handle (agent_N), rendered as a bottom panel. An entry appears on
	// the "started" event and is removed when its completion note arrives.
	// subAgentOrder preserves launch order for stable rendering.
	subAgents     map[string]*subAgentUI
	subAgentOrder []string

	// subAgentFocus is the index into subAgentOrder of the currently-focused
	// sub-agent in the panel (-1 when the input box has focus). Navigation with
	// ↑/↓ moves the focus; Enter toggles expand/collapse; Esc returns focus to
	// the input box.
	subAgentFocus int
}

// subAgentUI is the live panel state for one running sub-agent.
type subAgentUI struct {
	description string
	start       time.Time
	toolCount   int
	recent      []string // last few tool names, for the live chain (collapsed view)
	history     []string // complete tool history (expanded view)
	errored     bool
	expanded    bool // true when the panel shows the full history
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
	// otherActive is true when the user has selected "Other" and is now typing
	// free text inline inside the modal.
	otherActive bool
	otherInput  string
}

func newTUIModel(cfg replConfig) *tuiModel {
	ta := textarea.New()
	ta.Placeholder = inputPlaceholder
	ta.Focus()
	ta.ShowLineNumbers = false
	// Only the first line shows "> "; subsequent lines are padded with spaces
	// so text aligns (Claude Code style).
	ta.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "> "
		}
		return "  "
	})
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	// Disable up/down line navigation so we can use them for input-history
	// recall instead.
	ta.KeyMap.LineNext = key.Binding{}
	ta.KeyMap.LinePrevious = key.Binding{}
	style := "dark"
	if !tui.IsDark() {
		style = "light"
	}
	m := &tuiModel{cfg: cfg, a: cfg.a, cwd: abbreviateHome(workingDir()), ta: ta, inputHistoryIdx: -1, md: markdownRenderer{style: style}, subAgents: map[string]*subAgentUI{}, subAgentFocus: -1}
	_ = m.updateTextAreaHeight()
	return m
}

func (m *tuiModel) Init() tea.Cmd {
	boot := tea.Sequence(
		tea.Println(tui.Banner("", m.a.Model, m.cwd, m.width)),
		textarea.Blink,
	)
	// Connect MCP concurrently with the first paint (tea.Batch, not Sequence)
	// so the banner + input appear immediately and the servers' handshake cost
	// is hidden. The result lands as mcpReadyMsg.
	if m.cfg.mcpBoot != nil {
		return tea.Batch(boot, m.connectMCPCmd())
	}
	return boot
}

// connectMCPCmd returns a tea.Cmd that runs the (slow) MCP handshake off the
// event loop and reports the live registry back as mcpReadyMsg. Bubbletea runs
// each Cmd in its own goroutine, so this overlaps the rest of startup. The
// OAuth device-flow prompt and connect-failure warnings are routed to the TUI
// sink — never the raw terminal, which bubbletea owns.
func (m *tuiModel) connectMCPCmd() tea.Cmd {
	boot := m.cfg.mcpBoot
	sink := m.sink
	return func() tea.Msg {
		reg := mcp.ConnectAll(
			context.Background(),
			boot.cfg,
			boot.info,
			func(serverName string) mcp.OAuthPrompt { return newTUIOAuthPrompt(sink, serverName) },
			sinkWriter{sink: sink},
			boot.childErr,
		)
		return mcpReadyMsg{reg: reg}
	}
}

// startTurn launches a turn whose transcript echo is the submitted line itself.
func (m *tuiModel) startTurn(line string) tea.Cmd {
	return m.startTurnEcho(line, line)
}

// pendingFromInbox folds drained inbox items into one follow-up turn item,
// joining their text and collecting any image blocks so attachments survive the
// requeue (the in-loop drain normally consumes steers first; this covers a
// steer that raced in as the turn was ending).
func pendingFromInbox(items []agent.InboxItem) pendingItem {
	var blocks []agent.ContentBlock
	for _, it := range items {
		blocks = append(blocks, it.Blocks...)
	}
	return pendingItem{text: strings.Join(agent.Texts(items), "\n\n"), blocks: blocks}
}

// startQueued launches a dequeued follow-up turn, attaching any image blocks the
// item carried so they ride the new user message rather than being dropped.
func (m *tuiModel) startQueued(it pendingItem) tea.Cmd {
	if len(it.blocks) > 0 {
		m.a.AttachUserBlocks(it.blocks)
	}
	return m.startTurnEcho(it.text, "") // echo already shown when queued
}

// startTurnEcho launches a turn in a background goroutine driven by runTurn. The
// sink streams events back as tea.Msgs; turnFinishedMsg fires when runTurn
// returns so Update can save, drain steer, and dequeue. echo is the transcript
// line shown to the user — pass a short label (not line) when line is an
// expanded prompt that shouldn't be dumped verbatim (e.g. /init, /<skill>);
// pass "" to suppress the echo entirely.
func (m *tuiModel) startTurnEcho(line, echo string) tea.Cmd {
	return m.startTurnEchoRestore(line, echo, "")
}

// startTurnEchoRestore is startTurnEcho plus restore: the raw text put back
// into the input box if the user hits Esc before the model produces any output.
// Pass "" for turns that aren't a verbatim typed message (skills, /init,
// dequeued items) — those can't be meaningfully restored.
func (m *tuiModel) startTurnEchoRestore(line, echo, restore string) tea.Cmd {
	m.turnRunning = true
	m.turnStart = time.Now()
	m.spinnerFrame = 0
	m.running = nil
	m.partial.Reset()
	m.thinkPartial.Reset()
	m.thinkingStarted = false
	m.toolStreamName, m.toolStreamID, m.toolStreamBytes = "", "", 0
	m.clearSuggestion() // a new turn supersedes any pending follow-up
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
	// Defer the echo: hold it in the live View() area rather than committing it
	// to the scrollback now, so an Esc before the model responds can drop it (and
	// restore the typed text to the input) without leaving an orphan line in the
	// scrollback. It commits on the turn's first output via commitEcho. Excludes a
	// degraded background-notice "turn", which already surfaced as a bg notice and
	// isn't user input. Either way, start the animation ticker for the spinner.
	m.echoPending = ""
	m.echoRestore = ""
	if echo != "" && !strings.HasPrefix(echo, "<system-reminder>") {
		m.echoPending = userEchoStyle.Render("> ") + echo
		m.echoRestore = restore
	}
	return tickCmd()
}

// commitEcho promotes the deferred user-message echo from the live View() area
// to the scrollback, so it lands just above the turn's first output. Idempotent
// — a no-op once the echo has been committed or dropped.
func (m *tuiModel) commitEcho() {
	if m.echoPending == "" {
		return
	}
	m.println(m.echoPending)
	m.echoPending = ""
	m.echoRestore = ""
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ta.SetWidth(msg.Width)
		_ = m.updateTextAreaHeight()
		// Replay a resumed session's prior turns into the scrollback once, now
		// that the real wrap width is known. No-op for a fresh session.
		if !m.historyReplayed {
			m.historyReplayed = true
			for _, line := range m.replayHistoryLines() {
				m.println(line)
			}
		}
		return m, m.flushPrints()

	case tea.KeyMsg:
		return m.handleKey(msg)

	case turnStartedMsg:
		return m, m.flushPrints()

	case tickMsg:
		// Animate while a turn runs OR while background processes are still
		// going (so the live "background (N running)" panel keeps ticking even
		// between turns); let the ticker die once both are quiet.
		if !m.turnRunning && len(tools.RunningBackground()) == 0 && len(m.subAgents) == 0 {
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

	case mcpReadyMsg:
		// Background MCP connect finished. Install the registry and refresh the
		// tool list so the next turn carries the MCP surface. Only when built-in
		// tools were enabled (executor wired); plain chat keeps an empty surface.
		// Recomputing cfg.tools busts the provider's tools-prompt cache once —
		// acceptable, and only if the user already sent a turn this first second.
		if msg.reg != nil && msg.reg.Len() > 0 {
			tools.SetMCPRegistry(msg.reg)
			if m.cfg.executor != nil {
				m.cfg.tools = tools.DefaultToolsFor(m.cfg.a.Model)
			}
			if !m.cfg.verbosity.quiet() {
				m.println(noticeStyle.Render(fmt.Sprintf("● MCP ready — %d server(s) connected", msg.reg.Len())))
			}
		}
		return m, m.flushPrints()

	case bgExitMsg:
		// Print a concise, Claude-Code-style scrollback notice so the user
		// sees the completion even when the model-facing <system-reminder>
		// is folded into a later turn.
		notice := bgDoneStyle.Render(fmt.Sprintf("● Background process %s (`%s`) %s", msg.e.ID, msg.e.Command, msg.e.Status))
		// Idle auto-turn: if no turn is running and nothing is queued, drain the
		// inbox (which holds the full <system-reminder> notice) and start a
		// turn so the model sees the completion immediately — matching the plain
		// REPL's idleInboxWait behaviour.
		if !m.turnRunning && len(m.queue) == 0 {
			if items := m.a.Inbox.Drain(); len(items) > 0 {
				s := strings.Join(agent.Texts(items), "\n\n")
				return m, tea.Sequence(tea.Println(notice), m.startTurnEcho(s, ""))
			}
		}
		return m, tea.Sequence(tea.Println(notice), m.flushPrints())

	case subAgentEventMsg:
		m.handleSubAgentEvent(msg.ev)
		// Ensure the animation ticker is running so the panel's spinner/elapsed
		// updates even between turns.
		if !m.turnRunning {
			return m, tea.Batch(tickCmd(), m.flushPrints())
		}
		return m, m.flushPrints()

	case suggestionMsg:
		// Show the follow-up only while idle with an empty input — a new turn or
		// in-progress typing makes a stale suggestion unwelcome.
		if !m.turnRunning && strings.TrimSpace(m.ta.Value()) == "" {
			m.setSuggestion(msg.text)
		}
		return m, m.flushPrints()

	case titleMsg:
		// Persist the generated title (silent — it surfaces in the session list).
		m.titlePending = false
		if msg.text != "" && !m.cfg.noSave {
			_ = m.cfg.session.SetTitle(msg.text)
		}
		return m, m.flushPrints()

	case subAgentNoteMsg:
		// A sub-agent finished this round — drop it from the live panel (it's
		// idle now; a later Continue re-adds it via a fresh "started").
		m.removeSubAgent(msg.ev.AgentID)
		// Async sub-agent completion: the full result rode into the conversation
		// via Inbox; no scrollback notice needed.
		// Idle auto-turn: same logic as bgExitMsg — drain inbox and trigger a
		// turn so the model sees the notification immediately.
		if !m.turnRunning && len(m.queue) == 0 {
			if items := m.a.Inbox.Drain(); len(items) > 0 {
				s := strings.Join(agent.Texts(items), "\n\n")
				return m, m.startTurnEcho(s, "")
			}
		}
		return m, m.flushPrints()

	case turnEndedMsg:
		// Commit a still-pending echo (a turn that produced no events — error,
		// or Ctrl+C). The Esc take-back path clears it before cancelling, so this
		// is a no-op there.
		m.commitEcho()
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
		// turnEndedMsg only marks the end of the agent loop's output; the turn
		// goroutine is still alive until turnFinishedMsg. Do NOT reset turnRunning
		// or start the next queued/inbox turn here — that is handleTurnFinished's
		// job, and it fires the moment the goroutine actually returns. Starting a
		// turn now (the old eager-restart path) launched a second runTurn goroutine
		// while the interrupted one was still draining a blocking tool dispatch;
		// the two then interleaved their History.Append calls and produced a
		// structurally invalid log (consecutive assistant messages, a tool_use
		// answered twice) that permanently 400'd the session. Keeping turnRunning
		// true until the goroutine returns makes every `!turnRunning` start-gate a
		// correct single-turn guardrail. A background notice that races in via the
		// Inbox during this window is drained by handleTurnFinished, so nothing is
		// lost — only deferred by the few ms it takes the cancelled turn to unwind.

		// On a clean completion, kick off the off-loop after-turn helpers:
		// a follow-up suggestion (when enabled via --suggest) and, once per
		// session, a generated title for the session list. Both are skipped on
		// error/interrupt.
		if msg.err == nil {
			cmds := []tea.Cmd{m.flushPrints()}
			if m.cfg.suggest {
				cmds = append(cmds, m.suggestCmd())
			}
			if c := m.titleCmd(); c != nil {
				cmds = append(cmds, c)
			}
			return m, tea.Batch(cmds...)
		}
		return m, m.flushPrints()

	case turnFinishedMsg:
		return m.handleTurnFinished()

	case askMsg:
		m.openModal(msg)
		return m, m.flushPrints()

	case conductPlannedMsg:
		return m.onConductPlanned(msg)

	case conductRunMsg:
		return m, m.startConductRun(msg.id)

	case conductCancelledMsg:
		m.turnRunning = false
		m.cancelTurn = nil
		short := msg.id
		if len(short) > 8 {
			short = short[len(short)-8:]
		}
		m.println(noticeStyle.Render(fmt.Sprintf(
			"Cancelled. Seeded as %s — conduct later with /conduct resume %s (or octo conduct resume %s).",
			short, short, short)))
		return m, m.flushPrints()

	case conductDoneMsg:
		return m.onConductDone(msg)

	case conductShowMsg:
		return m.onConductShow(msg)
	}
	return m, m.flushPrints()
}

// handleEvent updates model state for a streaming agent event.  Text deltas
// are accumulated in m.partial so they render live in View(); only complete
// markdown blocks are promoted to the scrollback via println/flushPrints.
// Tool events flush any pending text first, then commit their line/card.
// maxSubAgentRecentTools bounds the live tool-chain shown per sub-agent.
const maxSubAgentRecentTools = 4

// handleSubAgentEvent folds one sub-agent runtime event into the live panel
// state. "started" creates/refreshes the entry; "tool"/"tool_error" append to
// its chain.
func (m *tuiModel) handleSubAgentEvent(ev tools.SubAgentEvent) {
	sa := m.subAgents[ev.AgentID]
	if sa == nil {
		sa = &subAgentUI{description: ev.Description, start: time.Now()}
		m.subAgents[ev.AgentID] = sa
		m.subAgentOrder = append(m.subAgentOrder, ev.AgentID)
	}
	switch ev.Kind {
	case "started":
		// A fresh round (sub_agent): reset the chain, keep the slot.
		sa.toolCount = 0
		sa.recent = nil
		sa.history = nil
		sa.errored = false
		if ev.Description != "" {
			sa.description = ev.Description
		}
	case "tool", "tool_error":
		sa.toolCount++
		name := ev.ToolName
		if ev.Kind == "tool_error" {
			sa.errored = true
			name += " ✗"
		}
		sa.recent = append(sa.recent, name)
		if len(sa.recent) > maxSubAgentRecentTools {
			sa.recent = sa.recent[len(sa.recent)-maxSubAgentRecentTools:]
		}
		sa.history = append(sa.history, name)
	}
}

// removeSubAgent drops a sub-agent from the live panel once it finishes a round.
func (m *tuiModel) removeSubAgent(id string) {
	if _, ok := m.subAgents[id]; !ok {
		return
	}
	delete(m.subAgents, id)
	removedIdx := -1
	for i, x := range m.subAgentOrder {
		if x == id {
			m.subAgentOrder = append(m.subAgentOrder[:i], m.subAgentOrder[i+1:]...)
			removedIdx = i
			break
		}
	}
	// Adjust focus if the removed agent had focus.
	if m.subAgentFocus >= 0 {
		if removedIdx < m.subAgentFocus {
			m.subAgentFocus--
		} else if removedIdx == m.subAgentFocus {
			if m.subAgentFocus >= len(m.subAgentOrder) {
				m.subAgentFocus = len(m.subAgentOrder) - 1
			}
			if m.subAgentFocus < 0 {
				m.subAgentFocus = -1
			}
		}
	}
}

// suggestCmd asks the model for one follow-up suggestion off the event loop.
// A nil/empty result yields a nil msg, which bubbletea ignores.
func (m *tuiModel) suggestCmd() tea.Cmd {
	a := m.a
	// Same toolbelt as the agentic loop, so the suggest request's
	// tools→system→history prefix matches and hits the prompt cache.
	tools := m.cfg.tools
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s, err := a.Suggest(ctx, tools)
		if err != nil || strings.TrimSpace(s) == "" {
			return nil
		}
		return suggestionMsg{text: s}
	}
}

// titleCmd generates a session title off the event loop, once per session. It
// returns nil (no-op) when the session is already titled, a generation is
// already in flight, saving is off, or there's no turn to summarize yet — so it
// fires exactly once, after the first completed turn. The result is persisted
// by the titleMsg handler.
func (m *tuiModel) titleCmd() tea.Cmd {
	if m.cfg.noSave || m.titlePending || m.cfg.session.Title != "" {
		return nil
	}
	if m.a.History.Len() == 0 {
		return nil
	}
	m.titlePending = true
	a := m.a
	tools := m.cfg.tools // same toolbelt as the loop, so the request hits the prompt cache
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		t, err := a.GenerateTitle(ctx, tools)
		if err != nil {
			return titleMsg{text: ""}
		}
		return titleMsg{text: t}
	}
}

// setSuggestion shows s as ghost text in the empty input box (the placeholder),
// truncated to one line so it never wraps the input.
func (m *tuiModel) setSuggestion(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	m.suggestion = s
	m.ta.Placeholder = truncate1Line(s)
}

// clearSuggestion drops the pending suggestion and restores the idle hint.
func (m *tuiModel) clearSuggestion() {
	if m.suggestion == "" {
		return
	}
	m.suggestion = ""
	m.ta.Placeholder = inputPlaceholder
}

// acceptSuggestion fills the input with the pending suggestion for the user to
// edit or send, then clears it.
func (m *tuiModel) acceptSuggestion() {
	if m.suggestion == "" {
		return
	}
	m.ta.SetValue(m.suggestion)
	m.ta.CursorEnd()
	m.clearSuggestion()
}

func (m *tuiModel) handleEvent(ev agent.AgentEvent) {
	// Promote the deferred user echo above this turn's first output so the
	// transcript order (your message → reply) is preserved.
	m.commitEcho()
	// The reasoning trace precedes the answer; flush any buffered thinking
	// before rendering the first non-thinking output of the turn.
	if ev.Kind != agent.EventThinkingDelta {
		m.flushThinking()
	}
	switch ev.Kind {
	case agent.EventThinkingDelta:
		m.appendThinking(ev.Text)
		return

	case agent.EventToolInputDelta:
		// The model is still streaming this tool call's arguments (e.g. a large
		// write_file content). Surface a live byte counter so the wait reads as
		// progress, not a freeze. Reset when a different tool's args begin.
		if ev.ToolID != m.toolStreamID {
			m.toolStreamID = ev.ToolID
			m.toolStreamBytes = 0
		}
		m.toolStreamName = ev.ToolName
		m.toolStreamBytes += len(ev.InputDelta)
		return

	case agent.EventTextDelta:
		m.appendText(ev.Text)
		return

	case agent.EventToolStarted:
		// Args finished streaming; the tool now executes. Clear the stream
		// readout so the running-card spinner (or one-line status) takes over.
		m.toolStreamName, m.toolStreamID, m.toolStreamBytes = "", "", 0
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

	case agent.EventCompactStarted:
		m.compacting = true
		m.compactStart = time.Now()
		m.compactTokens = 0
		m.compactPreview = ""
		if ev.Compact != nil {
			m.compactMaxTokens = ev.Compact.MaxTokens
		}
		return

	case agent.EventCompactProgress:
		if ev.Compact != nil {
			m.compactTokens = ev.Compact.SummaryTokens
		}
		// Keep a bounded tail of the streamed summary for the live preview.
		m.compactPreview = lastRunes(m.compactPreview+ev.Chunk, 240)
		return

	case agent.EventCompactDone:
		m.compacting = false
		m.compactPreview = ""
		// Only announce a real reduction; a no-op (summary failed / unchanged)
		// clears the indicator silently rather than implying work was done.
		if c := ev.Compact; c != nil && c.AfterTokens > 0 && c.AfterTokens < c.BeforeTokens {
			m.println(noticeStyle.Render(fmt.Sprintf(
				"✦ compacted context · folded %d message(s) · ~%s → ~%s tokens",
				c.FoldedMsgs, humanTokens(c.BeforeTokens), humanTokens(c.AfterTokens))))
		}
		return

	case agent.EventSteerInjected:
		// Inbox drained mid-turn: print the steer messages to the scrollback
		// immediately so they appear in chronological order (before the next
		// assistant reply), and remove them from the pending live display.
		// First commit any buffered assistant text so the prior reply's trailing
		// paragraph stays above the steer line (e.g. when a steer lands right
		// after the model finished a text answer and the turn loops to answer it).
		if s, ok := m.flushTextString(); ok {
			m.println(s)
		}
		// Skip <system-reminder> blocks (model-facing context) and empty text
		// (an image-only steer carries its payload in blocks, not Messages).
		for _, s := range ev.Messages {
			if s != "" && !strings.HasPrefix(s, "<system-reminder>") {
				m.println(userEchoStyle.Render("> ") + s)
			}
		}
		// pendingSteer is FIFO and mirrors the inbox, so the drained messages
		// are always a prefix.
		n := len(ev.Messages)
		if n >= len(m.pendingSteer) {
			m.pendingSteer = nil
		} else {
			m.pendingSteer = m.pendingSteer[n:]
		}
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
// appendThinking buffers a streamed reasoning-trace delta and commits each
// completed line to the scrollback, dimmed, so the trace stays in the
// transcript above the answer. The trailing partial line is held until
// flushThinking (called when the answer or a tool event begins).
func (m *tuiModel) appendThinking(text string) {
	m.thinkPartial.WriteString(text)
	buf := m.thinkPartial.String()
	for {
		idx := strings.IndexByte(buf, '\n')
		if idx < 0 {
			break
		}
		m.println(m.styleThinking(buf[:idx]))
		buf = buf[idx+1:]
	}
	m.thinkPartial.Reset()
	m.thinkPartial.WriteString(buf)
}

// flushThinking commits any buffered trailing reasoning line and ends the
// current reasoning block. A turn's agentic loop produces a fresh block of
// reasoning before each model round-trip (between tool calls); resetting
// thinkingStarted here means every block gets its own 💭 marker, matching the
// plain/headless renderer.
func (m *tuiModel) flushThinking() {
	if m.thinkPartial.Len() > 0 {
		m.println(m.styleThinking(m.thinkPartial.String()))
		m.thinkPartial.Reset()
	}
	m.thinkingStarted = false
}

// styleThinking renders one reasoning line dimmed, prefixing 💭 on the first
// line of each reasoning block so the block reads as one unit.
func (m *tuiModel) styleThinking(line string) string {
	if !m.thinkingStarted {
		m.thinkingStarted = true
		return thinkingStyle.Render("💭 " + line)
	}
	return thinkingStyle.Render("   " + line)
}

// replayHistoryLines renders a resumed session's prior turns into scrollback
// lines: user prompts and assistant replies verbatim (markdown unless --plain),
// each tool call collapsed to a one-line ✓/✗ summary, closed by a dim "resumed"
// marker. Returns nil for a fresh session (empty history).
func (m *tuiModel) replayHistoryLines() []string {
	msgs := m.a.History.Snapshot()
	if len(msgs) == 0 {
		return nil
	}
	// Pair each tool_use with its tool_result (by ID) so the collapsed line can
	// show the call's outcome.
	results := map[string]agent.ContentBlock{}
	for _, msg := range msgs {
		for _, b := range msg.Blocks {
			if b.Type == "tool_result" {
				results[b.ToolUseID] = b
			}
		}
	}

	var out []string
	for _, msg := range msgs {
		switch msg.Role {
		case agent.RoleUser:
			// Skip pure tool_result carriers — only real typed prompts echo.
			if t := userMessageText(msg); strings.TrimSpace(t) != "" {
				out = append(out, userEchoStyle.Render("> ")+t)
			}
		case agent.RoleAssistant:
			if len(msg.Blocks) == 0 {
				if s := m.renderReplayText(msg.Content); s != "" {
					out = append(out, s)
				}
				continue
			}
			for _, b := range msg.Blocks {
				switch b.Type {
				case "text":
					if s := m.renderReplayText(b.Text); s != "" {
						out = append(out, s)
					}
				case "tool_use":
					out = append(out, replayToolLine(b, results[b.ID]))
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append(out, hintStyle.Render(fmt.Sprintf("── resumed · %d messages ──", len(msgs))))
}

// renderReplayText styles one block of restored assistant/user prose: markdown
// unless --plain. Empty input renders to "".
func (m *tuiModel) renderReplayText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	if m.cfg.plain {
		return strings.TrimRight(s, "\n")
	}
	return m.md.render(s, m.width)
}

// userMessageText returns a user message's human-typed text, ignoring
// tool_result carriers and non-text (e.g. image) blocks.
func userMessageText(msg agent.Message) string {
	if len(msg.Blocks) == 0 {
		return msg.Content
	}
	var b strings.Builder
	for _, blk := range msg.Blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// replayToolLine collapses a tool_use (and its paired result) into a single
// ↳ verb(target) ✓/✗ line — the resumed-history counterpart of the live card.
func replayToolLine(use, result agent.ContentBlock) string {
	label := "↳ " + use.Name
	if target := cardTargetFor(use.Name, use.Input); target != "" {
		label += "(" + target + ")"
	}
	if result.Type == "tool_result" && result.IsError {
		return toolErrStyle.Render(label + " ✗")
	}
	return label + " ✓"
}

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
	m.running = nil // clear any live tool indicator (e.g. on interrupt)

	// Any steer messages that weren't drained via EventSteerInjected during
	// the turn (e.g. typed after the last loop iteration) are printed now so
	// they don't vanish from the transcript.
	for _, s := range m.pendingSteer {
		m.println(userEchoStyle.Render("> ") + s)
	}
	m.pendingSteer = nil

	// Auto-save (history is well-formed even after an interrupt).
	if !m.cfg.noSave {
		m.cfg.session.SyncFrom(m.a.History)
		_ = m.cfg.session.Save()
	}

	// Drain any inbox messages that weren't consumed during the turn and
	// run them as the next turn, ahead of explicitly-queued items.
	if items := m.a.Inbox.Drain(); len(items) > 0 {
		it := pendingFromInbox(items)
		for _, line := range strings.Split(it.text, "\n\n") {
			if line != "" && !strings.HasPrefix(line, "<system-reminder>") {
				m.println(userEchoStyle.Render("> ") + line)
			}
		}
		m.queue = append([]pendingItem{it}, m.queue...)
	}

	// Dequeue the next pending turn, if any.
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m, m.startQueued(next)
	}
	return m, nil
}
