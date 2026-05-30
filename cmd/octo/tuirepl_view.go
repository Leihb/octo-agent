package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	promptStyle   = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	noticeStyle   = lipgloss.NewStyle().Foreground(tui.ColMuted)
	errorStyle    = lipgloss.NewStyle().Foreground(tui.ColDanger)
	toolErrStyle  = lipgloss.NewStyle().Foreground(tui.ColDanger)
	queueStyle    = lipgloss.NewStyle().Foreground(tui.ColAccent)
	modalStyle    = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	hintStyle     = lipgloss.NewStyle().Foreground(tui.ColDimmer).Italic(true)
	userEchoStyle = lipgloss.NewStyle().Foreground(tui.ColUserMsg).Bold(true)
)

// wheelScrollLines is how many lines one wheel tick scrolls.  Larger than 1
// so track-pad two-finger scrolling feels responsive (bubbletea delivers one
// MouseWheel event per gesture tick, not per pixel).
const wheelScrollLines = 4

// handleKey routes a keypress by context: a modal grabs all keys; otherwise the
// keymap depends on whether a turn is running (design §7).
func (m *tuiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Only handle wheel events; ignore clicks, drags, etc.
	switch msg.Type {
	case tea.MouseWheelUp:
		m.scrollOffset += wheelScrollLines
		return m, nil
	case tea.MouseWheelDown:
		if m.scrollOffset > 0 {
			m.scrollOffset -= wheelScrollLines
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
		}
		return m, nil
	}
	return m, nil
}

func (m *tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modal != nil {
		return m.handleModalKey(msg)
	}

	switch msg.Type {
	case tea.KeyCtrlD:
		m.quit = true
		return m, tea.Quit

	case tea.KeyCtrlC:
		if m.turnRunning {
			m.interrupt()
			return m, nil
		}
		m.quit = true
		return m, tea.Quit

	case tea.KeyEsc:
		if m.turnRunning {
			m.interrupt()
			return m, nil
		}
		// Idle: clear the input line.
		m.ti.Reset()
		m.inputHistoryIdx = -1
		return m, nil

	case tea.KeyCtrlX:
		// Cancel the most-recently queued message. The queue survives Esc
		// (interrupt only stops the running turn), so this is the way to undo a
		// mis-queued Alt+Enter; repeat to clear the queue.
		if n := len(m.queue); n > 0 {
			dropped := m.queue[n-1].text
			m.queue = m.queue[:n-1]
			m.pushScrollback(queueStyle.Render("✕ unqueued: " + dropped))
		}
		return m, nil

	case tea.KeyShiftTab:
		// Cycle permission mode: interactive → strict → auto → interactive.
		if m.cfg.permEngine != nil {
			var next permission.Mode
			switch m.cfg.permEngine.GetMode() {
			case permission.ModeInteractive:
				next = permission.ModeStrict
			case permission.ModeStrict:
				next = permission.ModeAutoApprove
			default:
				next = permission.ModeInteractive
			}
			m.cfg.permEngine.SetMode(next)
			m.pushScrollback(noticeStyle.Render("Permission mode: " + string(next)))
		}
		return m, nil

	case tea.KeyEnter:
		return m.submit(msg.Alt)

	case tea.KeyUp:
		if m.inputHistoryIdx+1 < len(m.inputHistory) {
			m.inputHistoryIdx++
			m.ti.SetValue(m.inputHistory[len(m.inputHistory)-1-m.inputHistoryIdx])
			m.ti.CursorEnd()
		}
		return m, nil

	case tea.KeyDown:
		if m.inputHistoryIdx > 0 {
			m.inputHistoryIdx--
			m.ti.SetValue(m.inputHistory[len(m.inputHistory)-1-m.inputHistoryIdx])
			m.ti.CursorEnd()
		} else if m.inputHistoryIdx == 0 {
			m.inputHistoryIdx = -1
			m.ti.Reset()
		}
		return m, nil
	}

	// Everything else (typing, left/right, backspace, word movement, etc.)
	// is handled by bubbles/textinput.
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	return m, cmd
}

// submit acts on Enter / Alt+Enter. Idle → start a turn. Running → steer (Enter)
// or queue (Alt+Enter). Empty input is ignored.
func (m *tuiModel) submit(alt bool) (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.ti.Value())
	if text == "" {
		return m, nil
	}
	m.ti.Reset()
	m.inputHistoryIdx = -1
	// Save to history for ↑/↓ recall (dedup consecutive identical lines).
	if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != text {
		m.inputHistory = append(m.inputHistory, text)
	}

	// Slash commands are the TUI's alone — the plain REPL is a pure conversation
	// loop. Only dispatched when idle; mid-turn input still steers / queues.
	if !m.turnRunning {
		if strings.HasPrefix(text, "/") {
			return m.dispatchSlash(text)
		}
		return m, m.startTurn(text)
	}

	if alt {
		// Queue: run as a future turn.
		m.queue = append(m.queue, pendingItem{text: text})
		m.pushScrollback(queueStyle.Render("＋ queued: " + text))
		return m, nil
	}
	// Steer: fold into the running turn at the next tool-batch boundary.
	// No immediate UI echo — the steer text is displayed later via
	// EventSteerInjected when it is actually drained into the tool_result,
	// preserving chronological order.
	m.a.Steer(text)
	return m, nil
}

// dispatchSlash handles a leading-"/" line when the session is idle.
// Recognised commands are dispatched immediately; anything else falls through
// to startTurn as ordinary user text (paths, regexes, etc.) matching the plain
// REPL behaviour.
func (m *tuiModel) dispatchSlash(text string) (tea.Model, tea.Cmd) {
	cfg := m.cfg

	// /init: generate .octorules as a normal tool-enabled turn.
	if text == "/init" {
		if len(cfg.tools) == 0 || cfg.executor == nil {
			m.pushScrollback(noticeStyle.Render("/init needs tools — restart with: octo chat --tools"))
			return m, nil
		}
		return m, m.startTurnEcho(initInstruction, "/init")
	}

	// /<skill> [args]: inline the skill body and run it as a turn.
	if s, args, ok := skillTrigger(cfg.skillReg, text); ok {
		echo := "/" + s.Name
		if args != "" {
			echo += " " + args
		}
		return m, m.startTurnEcho(inlineSkill(s.Body, args), echo)
	}

	first := strings.Fields(text)[0]
	cmd := strings.ToLower(first)
	switch cmd {
	case "/exit", "/quit":
		m.quit = true
		return m, tea.Quit
	case "/goal":
		return m.dispatchGoal(strings.TrimSpace(strings.TrimPrefix(text, first)))
	case "/help", "/save", "/sessions", "/skills", "/memory", "/mcp":
		var b bytes.Buffer
		switch cmd {
		case "/help":
			printTuiHelp(&b)
		case "/save":
			if err := saveSession(&b, cfg.session, m.a); err != nil {
				fmt.Fprintf(&b, "save: %v\n", err)
			}
		case "/sessions":
			if err := printSessions(&b); err != nil {
				fmt.Fprintf(&b, "sessions: %v\n", err)
			}
		case "/skills":
			printSkills(&b, cfg.skillReg)
		case "/memory":
			printMemory(&b, cfg.memStore)
		case "/mcp":
			printMCP(&b)
		}
		m.pushScrollback(strings.TrimRight(b.String(), "\n"))
		return m, nil
	default:
		// Not a recognised command — treat it as ordinary user text so
		// paths, regexes, and other /-prefixed messages reach the model.
		return m, m.startTurn(text)
	}
}

func (m *tuiModel) interrupt() {
	if m.cancelTurn != nil {
		m.cancelTurn()
	}
}

// ── modal (Ask) ──

func (m *tuiModel) openModal(msg askMsg) {
	st := &modalState{prompt: msg.prompt, resp: msg.resp, selected: map[int]bool{}}
	if msg.prompt.Kind == KindQuestion {
		st.options = append(st.options, msg.prompt.Options...)
		st.options = append(st.options, "Other (free text)")
	}
	m.modal = st
}

// answerModal sends a response and clears the modal.
func (m *tuiModel) answerModal(r UserResponse) {
	if m.modal == nil {
		return
	}
	select {
	case m.modal.resp <- r:
	default:
	}
	m.modal = nil
}

func (m *tuiModel) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	st := m.modal
	if st.prompt.Kind == KindPermission {
		switch {
		case keyIs(msg, 'y'):
			m.answerModal(UserResponse{Allow: true})
		case keyIs(msg, 'a'):
			m.answerModal(UserResponse{Allow: true, Always: true})
		case msg.Type == tea.KeyEsc, keyIs(msg, 'n'):
			m.answerModal(UserResponse{Allow: false})
		}
		return m, nil
	}

	// Question modal: arrow/j-k to move, space to toggle (multi), enter to
	// confirm, esc to cancel.
	switch msg.Type {
	case tea.KeyEsc:
		m.answerModal(UserResponse{Cancelled: true})
	case tea.KeyUp:
		if st.cursor > 0 {
			st.cursor--
		}
	case tea.KeyDown:
		if st.cursor < len(st.options)-1 {
			st.cursor++
		}
	case tea.KeySpace:
		if st.prompt.MultiSelect {
			st.selected[st.cursor] = !st.selected[st.cursor]
		}
	case tea.KeyEnter:
		m.confirmQuestion(st)
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "j":
			if st.cursor < len(st.options)-1 {
				st.cursor++
			}
		case "k":
			if st.cursor > 0 {
				st.cursor--
			}
		}
	}
	return m, nil
}

// confirmQuestion maps the modal selection onto a UserResponse. The trailing
// "Other" slot can't be resolved to free text without a second input step; for
// now picking it cancels (the model can ask again or pick a default). A future
// pass can add an inline free-text field.
func (m *tuiModel) confirmQuestion(st *modalState) {
	otherIdx := len(st.options) - 1

	if st.prompt.MultiSelect {
		var picks []string
		for i := range st.options {
			if i == otherIdx {
				continue
			}
			if st.selected[i] {
				picks = append(picks, st.prompt.Options[i])
			}
		}
		if len(picks) == 0 {
			m.answerModal(UserResponse{Cancelled: true})
			return
		}
		m.answerModal(UserResponse{Choices: picks})
		return
	}

	if st.cursor == otherIdx {
		m.answerModal(UserResponse{Cancelled: true})
		return
	}
	m.answerModal(UserResponse{Choices: []string{st.prompt.Options[st.cursor]}})
}

// keyIs reports whether msg is a single-rune press of r (case-insensitive).
func keyIs(msg tea.KeyMsg, r rune) bool {
	if msg.Type != tea.KeyRunes || len(msg.Runes) != 1 {
		return false
	}
	got := msg.Runes[0]
	return got == r || got == r-32 || got == r+32
}

// ── View ──

// liveHeight returns the number of lines the "live" region (partial text,
// spinner, queue, background, input box, status bar) occupies.
func (m *tuiModel) liveHeight() int {
	h := 0
	if m.partial.String() != "" {
		h++
	}
	if m.running != nil || (m.turnRunning && !m.streaming) {
		h++
	}
	if n := len(m.queue); n > 0 {
		h += 3 + n // panel border (2) + title (1) + n body lines
	}
	if bg := tools.RunningBackground(); len(bg) > 0 {
		h += 3 + len(bg) // panel border (2) + title (1) + body lines
	}
	h++ // input box
	if m.turnRunning {
		h += 3 // status bar with hint: separator + segments + hint
	} else {
		h += 2 // status bar without hint: separator + segments
	}
	return h
}

func (m *tuiModel) View() string {
	if m.quit {
		return ""
	}
	if m.modal != nil {
		return m.modalView()
	}

	var b strings.Builder

	// ── Banner ──
	b.WriteString(tui.Banner("", m.a.Model, m.cwd, m.width))
	b.WriteByte('\n')

	// ── Scrollback (message history) ──
	// Calculate how many lines we can show between header and live region.
	liveH := m.liveHeight()
	headerH := tui.BannerHeight + 1             // Banner + the newline after it
	available := m.height - headerH - liveH - 1 // -1 for safety margin
	if available < 0 {
		available = 0
	}
	start := 0
	if len(m.scrollback) > available {
		start = len(m.scrollback) - available
	}
	// Apply user scroll offset (mouse wheel), clamped so we never scroll past
	// the top or below the bottom.
	start -= m.scrollOffset
	if start < 0 {
		start = 0
	}
	maxOffset := len(m.scrollback) - available
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	for i := start; i < len(m.scrollback); i++ {
		b.WriteString(m.scrollback[i])
		b.WriteByte('\n')
	}

	// ── Live partial assistant text ──
	if p := m.partial.String(); p != "" {
		b.WriteString(m.md.render(p, m.width))
		b.WriteByte('\n')
	}

	// ── Activity indicator ──
	if m.running != nil {
		b.WriteString(m.spinnerLine(m.running.verb+"("+m.running.target+")", m.running.start))
		b.WriteByte('\n')
	} else if m.turnRunning && !m.streaming {
		b.WriteString(m.spinnerLine(m.thinkingPhrase(), m.turnStart))
		b.WriteByte('\n')
	}

	// ── Queue panel ──
	if len(m.queue) > 0 {
		var items strings.Builder
		for i, q := range m.queue {
			if i > 0 {
				items.WriteByte('\n')
			}
			items.WriteString(queueStyle.Render(fmt.Sprintf("%d. %s", i+1, q.text)))
		}
		b.WriteString(tui.Panel(fmt.Sprintf("queue (%d)", len(m.queue)), items.String()))
		b.WriteByte('\n')
	}

	// ── Background processes panel ──
	if bg := tools.RunningBackground(); len(bg) > 0 {
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		var lines strings.Builder
		for i, p := range bg {
			if i > 0 {
				lines.WriteByte('\n')
			}
			fmt.Fprintf(&lines, "%c %s (%s)", frame, truncate1Line(p.Command), time.Since(p.Start).Round(time.Second))
		}
		b.WriteString(tui.Panel(fmt.Sprintf("background (%d running)", len(bg)), lines.String()))
		b.WriteByte('\n')
	}

	// ── Input box + status bar ──
	b.WriteString(m.renderInputBox())
	b.WriteByte('\n')
	b.WriteString(m.renderStatusBar())
	return b.String()
}

// thinkingPhrases rotate (slowly) on the initial-wait placeholder so the
// pause feels alive, CC-style. Cycled by spinnerFrame.
var thinkingPhrases = []string{"Thinking", "Pondering", "Working", "Reasoning"}

func (m *tuiModel) thinkingPhrase() string {
	// ~16 ticks (~2s at 120ms) per phrase.
	return thinkingPhrases[(m.spinnerFrame/16)%len(thinkingPhrases)]
}

// spinnerLine renders one animated activity line: a braille frame, a label,
// and elapsed seconds since the given start. Reuses the plain spinner's frame
// set (spinner.go) so the two paths look consistent.
func (m *tuiModel) spinnerLine(label string, since time.Time) string {
	frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	return hintStyle.Render(fmt.Sprintf("%c %s (%s)", frame, label, time.Since(since).Round(time.Second)))
}

// renderInputBox draws the prompt + current input as a flat line (Claude Code
// style). No border — just the prompt prefix and the textinput content.
func (m *tuiModel) renderInputBox() string {
	return promptStyle.Render("> ") + m.ti.View()
}

// renderStatusBar renders the cwd / context% / permission / elapsed segments,
// with a separator line above and the contextual key hint below (Claude Code
// style).
func (m *tuiModel) renderStatusBar() string {
	var segs [][2]string
	if m.cwd != "" {
		segs = append(segs, [2]string{"cwd", m.cwd})
	}
	if used, window := m.a.ContextUsage(); window > 0 && used > 0 {
		pct := used * 100 / window
		if pct > 100 {
			pct = 100
		}
		segs = append(segs, [2]string{"ctx", fmt.Sprintf("%d%%", pct)})
	}
	if m.cfg.permEngine != nil {
		segs = append(segs, [2]string{"perm", string(m.cfg.permEngine.GetMode())})
	}
	if m.turnRunning && !m.turnStart.IsZero() {
		segs = append(segs, [2]string{"elapsed", time.Since(m.turnStart).Round(time.Second).String()})
	}

	var hint string
	if m.turnRunning {
		hint = "Enter steer · Alt+Enter queue · Esc interrupt"
		if len(m.queue) > 0 {
			hint += " · Ctrl+X unqueue"
		}
	}
	return tui.StatusBar(segs, hint, m.width)
}

// workingDir returns the current directory, or "" if it can't be determined.
func workingDir() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// abbreviateHome replaces the user's home-dir prefix with "~".
func abbreviateHome(path string) string {
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if path == home {
			return "~"
		}
		if strings.HasPrefix(path, home+string(os.PathSeparator)) {
			return "~" + path[len(home):]
		}
	}
	return path
}

func (m *tuiModel) modalView() string {
	st := m.modal
	var b strings.Builder

	if st.prompt.Kind == KindPermission {
		b.WriteString(modalStyle.Render("⚠ permission"))
		b.WriteByte('\n')
		b.WriteString(fmt.Sprintf("%s wants to run\n", st.prompt.ToolName))
		b.WriteString(fmt.Sprintf("  %s\n", summariseInput(st.prompt.ToolInput)))
		b.WriteString(hintStyle.Render("[y]es · [a]lways this session · [n]o/Esc"))
		return tui.Box(b.String())
	}

	header := st.prompt.Header
	if header == "" {
		header = "question"
	}
	b.WriteString(modalStyle.Render("[" + header + "]"))
	b.WriteByte('\n')
	b.WriteString("  " + st.prompt.Question + "\n")
	for i, opt := range st.options {
		cursor := "  "
		if i == st.cursor {
			cursor = "▸ "
		}
		mark := ""
		if st.prompt.MultiSelect {
			if st.selected[i] {
				mark = "[x] "
			} else {
				mark = "[ ] "
			}
		}
		b.WriteString(fmt.Sprintf("  %s%s%s\n", cursor, mark, opt))
	}
	hint := "↑/↓ move · Enter select · Esc cancel"
	if st.prompt.MultiSelect {
		hint = "↑/↓ move · Space toggle · Enter confirm · Esc cancel"
	}
	b.WriteString(hintStyle.Render(hint))
	return tui.Box(b.String())
}

// cacheLine formats the per-turn cache footer, or "" when nothing to show.
// Mirrors plainView's rule: shown when cache moved, always in verbose.
func cacheLine(v verbosity, reply agent.Reply) string {
	if v.quiet() {
		return ""
	}
	show := reply.CacheReadTokens > 0 || reply.CacheWriteTokens > 0
	if v.verbose() {
		show = true
	}
	if !show {
		return ""
	}
	return noticeStyle.Render(fmt.Sprintf("  ⓘ cache: %d read, %d write (in %d / out %d)",
		reply.CacheReadTokens, reply.CacheWriteTokens, reply.InputTokens, reply.OutputTokens))
}
