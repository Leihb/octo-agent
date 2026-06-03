package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var (
	promptStyle       = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	noticeStyle       = lipgloss.NewStyle().Foreground(tui.ColMuted)
	errorStyle        = lipgloss.NewStyle().Foreground(tui.ColDanger)
	toolErrStyle      = lipgloss.NewStyle().Foreground(tui.ColDanger)
	queueStyle        = lipgloss.NewStyle().Foreground(tui.ColAccent)
	modalStyle        = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	hintStyle         = lipgloss.NewStyle().Foreground(tui.ColDimmer).Italic(true)
	userEchoStyle     = lipgloss.NewStyle().Foreground(tui.ColUserMsg).Bold(true)
	pendingSteerStyle = lipgloss.NewStyle().Foreground(tui.ColMuted)
	complSelStyle     = lipgloss.NewStyle().Foreground(tui.ColBrand).Bold(true)
	complNameStyle    = lipgloss.NewStyle().Foreground(tui.ColAccent)
	bgDoneStyle       = lipgloss.NewStyle().Foreground(tui.ColAccent)
)

func (m *tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modal != nil {
		return m.handleModalKey(msg)
	}

	// Slash-command completion menu owns Tab/↑/↓/Enter/Esc while it's open, so
	// it can navigate and accept without those keys reaching history nav or
	// submit. Plain typing falls through and re-filters the menu below.
	if len(m.complItems) > 0 {
		switch msg.Type {
		case tea.KeyTab, tea.KeyDown:
			m.complIdx = (m.complIdx + 1) % len(m.complItems)
			return m, nil
		case tea.KeyUp:
			m.complIdx = (m.complIdx - 1 + len(m.complItems)) % len(m.complItems)
			return m, nil
		case tea.KeyEnter:
			if !msg.Alt {
				m.acceptCompletion()
				return m, m.updateTextAreaHeight()
			}
		case tea.KeyEsc:
			m.complItems = nil
			return m, nil
		}
	}

	// Ghost-text follow-up: Tab or → accepts the pending suggestion when the
	// input is empty, filling it in to edit or send. With text present these
	// keys keep their normal behaviour (cursor / tab insert).
	if m.suggestion != "" && strings.TrimSpace(m.ta.Value()) == "" {
		if msg.Type == tea.KeyTab || msg.Type == tea.KeyRight {
			m.acceptSuggestion()
			return m, m.updateTextAreaHeight()
		}
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
			// Take-back: Esc before the model has produced any output (the echo
			// is still pending in the live area). Drop the not-yet-committed echo
			// and restore the typed text to the input box for editing — the agent's
			// interrupt rolls the unanswered user message back out of history, so
			// it leaves no trace in the scrollback. Once output has streamed the
			// echo is already committed (echoPending == "") and Esc just interrupts.
			if m.echoPending != "" {
				restore := m.echoRestore
				m.echoPending = ""
				m.echoRestore = ""
				m.interrupt()
				if restore != "" {
					m.ta.SetValue(restore)
					m.ta.CursorEnd()
					m.inputHistoryIdx = -1
					return m, m.updateTextAreaHeight()
				}
				return m, nil
			}
			m.interrupt()
			return m, nil
		}
		// Idle: clear the input line and discard any pending attachments.
		m.pendingAttachments = nil
		m.ta.Reset()
		m.inputHistoryIdx = -1
		return m, m.updateTextAreaHeight()

	case tea.KeyCtrlJ:
		// Ctrl+J inserts a newline (LF) into the textarea. Works on all
		// terminals, including those where Alt+Enter is not mapped.
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if hcmd := m.updateTextAreaHeight(); hcmd != nil {
			cmd = tea.Batch(cmd, hcmd)
		}
		return m, cmd

	case tea.KeyCtrlQ:
		// Queue the current input to run as a future turn.
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return m, nil
		}
		m.ta.Reset()
		m.inputHistoryIdx = -1
		if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != text {
			m.inputHistory = append(m.inputHistory, text)
		}
		m.queue = append(m.queue, pendingItem{text: text})
		m.println(queueStyle.Render("＋ queued: " + text))
		return m, nil

	case tea.KeyCtrlX:
		// Cancel the most-recently queued message. The queue survives Esc
		// (interrupt only stops the running turn), so this is the way to undo a
		// mis-queued Ctrl+Q; repeat to clear the queue.
		if n := len(m.queue); n > 0 {
			dropped := m.queue[n-1].text
			m.queue = m.queue[:n-1]
			m.println(queueStyle.Render("✕ unqueued: " + dropped))
		}
		return m, nil

	case tea.KeyCtrlV:
		// Paste an image from the clipboard as an attachment on the next turn.
		// Normal text paste (Cmd+V on macOS) arrives as bracketed-paste runes
		// handled by the textarea, so this binding is free for image paste.
		return m.pasteClipboardImage()

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
			m.println(noticeStyle.Render("Permission mode: " + string(next)))
		}
		return m, nil

	case tea.KeyEnter:
		if msg.Alt {
			// Alt+Enter inserts a newline into the textarea.
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if hcmd := m.updateTextAreaHeight(); hcmd != nil {
				cmd = tea.Batch(cmd, hcmd)
			}
			return m, cmd
		}
		return m.submit()

	case tea.KeyUp:
		// Retract the most recent pending steer (typed mid-turn, not yet drained)
		// back into the empty input box for editing — and drop it from the queue
		// so it won't also be sent. If it was already drained (Inbox.Remove
		// fails) it's committed; fall through to ordinary history recall.
		if strings.TrimSpace(m.ta.Value()) == "" && len(m.pendingSteer) > 0 {
			last := m.pendingSteer[len(m.pendingSteer)-1]
			if m.a.Inbox.Remove(last) {
				m.pendingSteer = m.pendingSteer[:len(m.pendingSteer)-1]
				m.ta.SetValue(last)
				m.ta.CursorEnd()
				return m, m.updateTextAreaHeight()
			}
		}
		// If the cursor is not on the first display row, move up inside the
		// textarea (line navigation). Otherwise browse input history.
		if m.ta.Line() > 0 || m.ta.LineInfo().RowOffset > 0 {
			m.ta.CursorUp()
			return m, nil
		}
		if m.inputHistoryIdx+1 < len(m.inputHistory) {
			if m.inputHistoryIdx == -1 {
				m.inputDraft = m.ta.Value()
			}
			m.inputHistoryIdx++
			m.ta.SetValue(m.inputHistory[len(m.inputHistory)-1-m.inputHistoryIdx])
			m.ta.CursorEnd()
			return m, m.updateTextAreaHeight()
		}
		return m, nil

	case tea.KeyDown:
		// If the cursor is not on the last display row, move down inside the
		// textarea (line navigation). Otherwise browse input history.
		li := m.ta.LineInfo()
		lines := strings.Count(m.ta.Value(), "\n") + 1
		if m.ta.Line() < lines-1 || li.RowOffset < li.Height-1 {
			m.ta.CursorDown()
			return m, nil
		}
		if m.inputHistoryIdx > 0 {
			m.inputHistoryIdx--
			m.ta.SetValue(m.inputHistory[len(m.inputHistory)-1-m.inputHistoryIdx])
			m.ta.CursorEnd()
			return m, m.updateTextAreaHeight()
		} else if m.inputHistoryIdx == 0 {
			m.inputHistoryIdx = -1
			m.ta.SetValue(m.inputDraft)
			m.inputDraft = ""
			m.ta.CursorEnd()
			return m, m.updateTextAreaHeight()
		}
		return m, nil
	}

	// Everything else (typing, left/right, backspace, word movement, etc.)
	// is handled by bubbles/textarea.
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	// Detect a dropped image file path (some terminals paste the path as text
	// when a file is dragged in). If the input box now holds a single image
	// file path, swallow it and queue it as an attachment instead.
	if m.tryAttachDroppedImage() {
		return m, m.updateTextAreaHeight()
	}
	// A fresh edit re-filters the slash-completion menu from the top.
	m.complIdx = 0
	m.updateCompletion()
	if hcmd := m.updateTextAreaHeight(); hcmd != nil {
		cmd = tea.Batch(cmd, hcmd)
	}
	return m, cmd
}

// tryAttachDroppedImage checks whether the textarea currently contains an
// image file path (some terminals paste a dragged file's path as text).  The
// path may be surrounded by ordinary text, e.g. "look at /path/to/img.png and
// tell me", and may contain escaped spaces ("\ ").  If a valid image file
// path is found, it is removed from the textarea, queued as a pending
// attachment, and true is returned.
func (m *tuiModel) tryAttachDroppedImage() bool {
	full := m.ta.Value()
	// Unescape common shell escapes so paths with spaces are whole.
	unescaped := strings.ReplaceAll(full, `\ `, " ")
	unescaped = strings.ReplaceAll(unescaped, `\(`, "(")
	unescaped = strings.ReplaceAll(unescaped, `\)`, ")")

	// Scan for potential paths by looking for image extensions.
	lower := strings.ToLower(unescaped)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tiff", ".tif", ".heic", ".ico"} {
		idx := strings.Index(lower, ext)
		if idx == -1 {
			continue
		}
		// Extract the candidate path: walk back to the preceding space or
		// start of string, and forward to the following space or end.
		start := idx
		for start > 0 && unescaped[start-1] != ' ' && unescaped[start-1] != '\t' && unescaped[start-1] != '\n' {
			start--
		}
		end := idx + len(ext)
		for end < len(unescaped) && unescaped[end] != ' ' && unescaped[end] != '\t' && unescaped[end] != '\n' {
			end++
		}
		path := strings.Trim(unescaped[start:end], `"'`)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		mime := "image/" + strings.TrimPrefix(filepath.Ext(path), ".")
		if filepath.Ext(path) == ".jpg" {
			mime = "image/jpeg"
		}
		label := fmt.Sprintf("image (%s, %s)", shortMIME(mime), humanByteSize(len(data)))
		m.pendingAttachments = append(m.pendingAttachments, pendingAttachment{
			block: agent.NewImageBlock(mime, data),
			label: label,
		})
		// Remove the path from the original (escaped) textarea value.
		before := strings.TrimSpace(full[:start])
		after := strings.TrimSpace(full[end:])
		if before != "" && after != "" {
			m.ta.SetValue(before + " " + after)
		} else {
			m.ta.SetValue(before + after)
		}
		m.ta.CursorEnd()
		return true
	}
	return false
}

// pasteClipboardImage captures an image from the system clipboard and queues
// it as an attachment for the next turn. With no image in the clipboard (or on
// an unsupported platform) it surfaces a friendly notice and changes nothing.
func (m *tuiModel) pasteClipboardImage() (tea.Model, tea.Cmd) {
	data, mime, err := captureClipboardImage()
	if err != nil {
		m.println(noticeStyle.Render("📋 " + err.Error()))
		return m, nil
	}
	label := fmt.Sprintf("image (%s, %s)", shortMIME(mime), humanByteSize(len(data)))
	m.pendingAttachments = append(m.pendingAttachments, pendingAttachment{
		block: agent.NewImageBlock(mime, data),
		label: label,
	})
	return m, nil
}

// attachmentChips renders the pending attachments as a one-line summary for
// the echo / live view, e.g. "📎 image (PNG, 84 KB)".
func (m *tuiModel) attachmentChips() string {
	if len(m.pendingAttachments) == 0 {
		return ""
	}
	parts := make([]string, len(m.pendingAttachments))
	for i, a := range m.pendingAttachments {
		parts[i] = "📎 " + a.label
	}
	return strings.Join(parts, "  ")
}

// shortMIME turns "image/png" into "PNG" for the chip.
func shortMIME(mime string) string {
	if i := strings.LastIndex(mime, "/"); i >= 0 {
		return strings.ToUpper(mime[i+1:])
	}
	return strings.ToUpper(mime)
}

// humanTokens renders a token count compactly (e.g. 142000 → "142k").
func humanTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// lastRunes returns the final n runes of s (rune-safe so multi-byte CJK
// summaries aren't sliced mid-character). Used to bound the live compaction
// preview without growing it without limit.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// humanByteSize renders a byte count compactly (B / KB / MB).
func humanByteSize(n int) string {
	const kb, mb = 1024, 1024 * 1024
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// submit acts on Enter. Idle → start a turn. Running → steer. Empty input
// with no attachments is ignored.
func (m *tuiModel) submit() (tea.Model, tea.Cmd) {
	// Last-chance catch for image file paths that were pasted/dropped into the
	// textarea without triggering the per-keystroke detection (e.g. bracketed
	// paste from a terminal drag-and-drop).
	m.tryAttachDroppedImage()

	text := strings.TrimSpace(m.ta.Value())
	if text == "" && len(m.pendingAttachments) == 0 {
		return m, nil
	}
	m.ta.Reset()
	m.inputHistoryIdx = -1
	// Save to history for ↑/↓ recall (dedup consecutive identical lines).
	// Skip empty text (an image-only submit has nothing to recall).
	if text != "" && (len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != text) {
		m.inputHistory = append(m.inputHistory, text)
	}

	// Slash commands are the TUI's alone — the plain REPL is a pure conversation
	// loop. Only dispatched when idle; mid-turn input still steers / queues.
	// A leading "/" with attachments is treated as ordinary text, not a command.
	if !m.turnRunning {
		if strings.HasPrefix(text, "/") && len(m.pendingAttachments) == 0 {
			return m.dispatchSlash(text)
		}
		// Fold any pending image attachments into this turn's user message.
		echo := text
		if len(m.pendingAttachments) > 0 {
			blocks := make([]agent.ContentBlock, 0, len(m.pendingAttachments))
			for _, a := range m.pendingAttachments {
				blocks = append(blocks, a.block)
			}
			m.a.AttachUserBlocks(blocks)
			echo = strings.TrimSpace(text + "  " + m.attachmentChips())
			m.pendingAttachments = nil
		}
		return m, m.startTurnEcho(text, echo)
	}

	// Mid-turn: enqueue the steer text, folding in any pending image
	// attachments so they ride this message rather than being stranded.
	if text != "" || len(m.pendingAttachments) > 0 {
		m.pendingSteer = append(m.pendingSteer, text)
		var blocks []agent.ContentBlock
		if len(m.pendingAttachments) > 0 {
			blocks = make([]agent.ContentBlock, 0, len(m.pendingAttachments))
			for _, a := range m.pendingAttachments {
				blocks = append(blocks, a.block)
			}
			m.pendingAttachments = nil
		}
		m.a.Inbox.EnqueueWithBlocks(text, blocks)
	}
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
			m.println(noticeStyle.Render("/init needs tools — restart with: octo chat --tools"))
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
		return m, m.startTurnEcho(inlineSkill(s, args), echo)
	}

	first := strings.Fields(text)[0]
	cmd := strings.ToLower(first)
	switch cmd {
	case "/exit", "/quit":
		m.quit = true
		return m, tea.Quit
	case "/conduct":
		return m.dispatchConduct(strings.TrimSpace(strings.TrimPrefix(text, first)))
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
			printMemory(&b, cfg.memDir)
		case "/mcp":
			printMCP(&b)
		}
		m.println(strings.TrimRight(b.String(), "\n"))
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
	m.pendingSteer = nil
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
	if m.echoPending != "" {
		h += strings.Count(m.echoPending, "\n") + 1
	}
	if m.partial.String() != "" {
		h++
	}
	if m.running != nil || (m.turnRunning && m.partial.Len() == 0) {
		h++
	}
	if n := len(m.pendingSteer); n > 0 {
		h += n // one line per pending steer message
	}
	if n := len(m.queue); n > 0 {
		h += 3 + n // panel border (2) + title (1) + n body lines
	}
	if bg := tools.RunningBackground(); len(bg) > 0 {
		h += 3 + len(bg) // panel border (2) + title (1) + body lines
	}
	if n := len(m.subAgentOrder); n > 0 {
		h += 3 + n // panel border (2) + title (1) + one line per sub-agent
	}
	h += m.completionHeight() // slash-completion menu (0 when closed)
	h += m.ta.Height()        // input box (textarea grows with content)
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

	// Deferred user-message echo: shown live above the activity area until the
	// turn's first output commits it to the scrollback (or Esc takes it back).
	if m.echoPending != "" {
		b.WriteString(m.echoPending)
		b.WriteByte('\n')
	}

	// Live partial assistant text
	if p := m.partial.String(); p != "" {
		b.WriteString(m.md.render(p, m.width))
		b.WriteByte('\n')
	}

	// Activity indicator
	if m.compacting {
		// Compaction runs between LLM calls; surface a dedicated spinner with a
		// live "generated ~N / max tokens" readout plus a streaming preview so
		// the user can tell the conversation is being summarized, not frozen.
		label := "Compacting conversation history…"
		if m.compactMaxTokens > 0 {
			label += fmt.Sprintf("  ~%s / ~%s tokens",
				humanTokens(m.compactTokens), humanTokens(m.compactMaxTokens))
		}
		b.WriteString(m.spinnerLine(label, m.compactStart))
		b.WriteByte('\n')
		if preview := strings.TrimSpace(strings.ReplaceAll(m.compactPreview, "\n", " ")); preview != "" {
			b.WriteString(hintStyle.Render("  … " + lastRunes(preview, 100)))
			b.WriteByte('\n')
		}
	} else if m.running != nil {
		b.WriteString(m.spinnerLine(m.running.verb+"("+m.running.target+")", m.running.start))
		b.WriteByte('\n')
	} else if m.turnRunning && m.partial.Len() == 0 {
		// Turn is running but nothing is on the activity line right now — no
		// live tool and no streaming text. That's the wait on the model
		// (initial prompt, or between steps after a tool result). Show the
		// thinking spinner so the user can tell the turn isn't idle. While
		// text is actively streaming (partial non-empty), the text itself is
		// the feedback, so the spinner stays out of the way.
		b.WriteString(m.spinnerLine(m.thinkingPhrase(), m.turnStart))
		b.WriteByte('\n')
	}

	// Live task list — attached under the activity area while a turn runs
	// (Claude Code style). Hidden when idle, and self-suppressing when there's
	// no outstanding work (see taskListView).
	if m.turnRunning {
		if tl := m.taskListView(); tl != "" {
			b.WriteString(tl)
			b.WriteByte('\n')
		}
	}

	// Queue panel
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

	// Background processes panel
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

	// Sub-agents panel — live tool-call chain of each running sub-agent
	// (Claude-Code style), mirroring the background-processes panel.
	if len(m.subAgentOrder) > 0 {
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		var lines strings.Builder
		for i, id := range m.subAgentOrder {
			sa := m.subAgents[id]
			if sa == nil {
				continue
			}
			if i > 0 {
				lines.WriteByte('\n')
			}
			label := id
			if sa.description != "" {
				label = fmt.Sprintf("%s (%s)", id, truncate1Line(sa.description))
			}
			chain := "starting…"
			if len(sa.recent) > 0 {
				chain = strings.Join(sa.recent, " · ")
			}
			fmt.Fprintf(&lines, "%c %s — %s  (%d tools, %s)",
				frame, label, chain, sa.toolCount, time.Since(sa.start).Round(time.Second))
		}
		b.WriteString(tui.Panel(fmt.Sprintf("sub-agents (%d running)", len(m.subAgentOrder)), lines.String()))
		b.WriteByte('\n')
	}

	// Pending steer — show what the user typed mid-turn, indented right above
	// the input box (Claude Code style: input上方，比普通消息多了一个indent).
	// Once the agent loop drains the inbox these messages are printed to the
	// scrollback in handleTurnFinished so they appear in the transcript like
	// regular user messages.
	if len(m.pendingSteer) > 0 {
		for _, s := range m.pendingSteer {
			b.WriteString(pendingSteerStyle.Render("  > ") + pendingSteerStyle.Render(s))
			b.WriteByte('\n')
		}
	}

	// Pending image attachments — chips above the input so the user knows an
	// image will ride their next message (Esc discards them).
	if len(m.pendingAttachments) > 0 {
		b.WriteString(pendingSteerStyle.Render("  " + m.attachmentChips()))
		b.WriteByte('\n')
	}

	// Slash-command completion menu, right above the input box (Claude Code style).
	b.WriteString(m.completionView())

	// Input box + status bar
	b.WriteString(m.renderInputBox())
	b.WriteByte('\n')
	b.WriteString(m.renderStatusBar())
	return b.String()
}

// updateTextAreaHeight sets the textarea height to match the number of lines
// in the current value, capped at a maximum so it doesn't take over the screen.
// When the height grows we also reset the viewport YOffset to 0 via reflection
// so that earlier lines remain visible instead of being scrolled out of view.
func (m *tuiModel) updateTextAreaHeight() tea.Cmd {
	lines := strings.Count(m.ta.Value(), "\n") + 1
	maxH := min(6, m.height/4)
	if maxH < 1 {
		maxH = 1
	}
	newH := min(lines, maxH)
	if m.ta.Height() == newH {
		return nil
	}
	m.ta.SetHeight(newH)
	// textarea.viewport is unexported; use unsafe to reset YOffset.
	v := reflect.ValueOf(&m.ta).Elem().FieldByName("viewport")
	if !v.IsValid() || v.IsNil() {
		return nil
	}
	vp := reflect.NewAt(v.Elem().Type(), unsafe.Pointer(v.Elem().UnsafeAddr())).Elem()
	if yOffset := vp.FieldByName("YOffset"); yOffset.IsValid() {
		yOffset.SetInt(0)
	}
	return nil
}

func (m *tuiModel) renderInputBox() string {
	return m.ta.View()
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
		esc := "Esc interrupt"
		if m.echoPending != "" {
			esc = "Esc take back" // no output yet — Esc returns the message to the input
		}
		hint = "Enter steer · Shift+Enter/Alt+Enter/Ctrl+J newline · Ctrl+Q queue · " + esc
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

// thinkingPhrases rotate (slowly) on the initial-wait placeholder so the
// pause feels alive, CC-style. Cycled by spinnerFrame.
var thinkingPhrases = []string{"Thinking", "Pondering", "Working", "Reasoning"}

func (m *tuiModel) thinkingPhrase() string {
	return thinkingPhrases[(m.spinnerFrame/16)%len(thinkingPhrases)]
}

// spinnerLine renders one animated activity line: a braille frame, a label,
// and elapsed seconds since the given start.
func (m *tuiModel) spinnerLine(label string, since time.Time) string {
	frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	return hintStyle.Render(fmt.Sprintf("%c %s (%s)", frame, label, time.Since(since).Round(time.Second)))
}
