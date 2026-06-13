package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// sessionLiveState tracks in-progress agent state for replay to late subscribers.
type sessionLiveState struct {
	progress    *wsEventProgress
	stdoutLines []string

	// events buffers the turn's already-broadcast transcript events
	// (tool_call / tool_result / tool_error / steer history_user_message,
	// plus flushed deltas) so a tab that subscribes mid-turn — e.g. after a
	// page refresh — can replay what it missed. The session file only gains
	// the turn's messages at turn end, so until then this buffer is the only
	// source. Dropped with the live state once the turn persists.
	events []map[string]any

	// textBuf / thinkingBuf accumulate the in-flight LLM round's streamed
	// deltas. flushDeltas folds them into events when the round ends in a
	// tool call; anything still unflushed is replayed directly after events.
	textBuf     strings.Builder
	thinkingBuf strings.Builder

	// historyWatermark is how many persisted messages predate this turn
	// (including the turn's own user message). The turn's progress is saved
	// to disk incrementally, so without a cut-off a mid-turn history fetch
	// would reconstruct the same rounds the replay buffer resends — every
	// tool card twice. The history endpoint serves messages below the
	// watermark; the buffer owns everything above it. Mid-turn compaction
	// shifts it (see the EventCompactDone handler). 0 means unset (no cap):
	// a real watermark is always ≥ 1 because the user message saves first.
	historyWatermark int
}

// maxLiveTurnEvents caps the replay buffer; a turn that somehow exceeds it
// (hundreds of tool rounds) drops its oldest events rather than growing
// without bound. The cap stays under the 256-slot conn send buffer so a full
// replay can never overflow a fresh connection.
const maxLiveTurnEvents = 200

// appendEvent adds an already-broadcast turn event to the replay buffer.
// Caller holds liveStateMu.
func (ls *sessionLiveState) appendEvent(ev map[string]any) {
	ls.events = append(ls.events, ev)
	if n := len(ls.events) - maxLiveTurnEvents; n > 0 {
		ls.events = ls.events[n:]
	}
}

// flushDeltas folds the accumulated streaming deltas into the replay buffer
// as one synthetic event each, preserving their position relative to the
// tool call that ended the round. Caller holds liveStateMu.
func (ls *sessionLiveState) flushDeltas(sessionID string) {
	if ls.thinkingBuf.Len() > 0 {
		ls.appendEvent(map[string]any{
			"type":       "thinking_delta",
			"session_id": sessionID,
			"text":       ls.thinkingBuf.String(),
		})
		ls.thinkingBuf.Reset()
	}
	if ls.textBuf.Len() > 0 {
		ls.appendEvent(map[string]any{
			"type":       "text_delta",
			"session_id": sessionID,
			"text":       ls.textBuf.String(),
		})
		ls.textBuf.Reset()
	}
}

// ─── WS handler methods on Server ──────────────────────────────────────────

// listSessionsBrief returns a brief session list for the initial WS handshake.
func (s *Server) listSessionsBrief() []wsSessionInfo {
	sessions, err := agent.ListSessions(50)
	if err != nil {
		return nil
	}
	wd, pm, re, ctxUsage := s.sessionStatusFields()
	out := make([]wsSessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		source := sess.Source
		if source == "" {
			source = "manual"
		}
		out = append(out, wsSessionInfo{
			ID:              sess.ID,
			Name:            sess.DisplayTitle(),
			Status:          s.sessionStatus(sess.ID),
			CreatedAt:       sess.CreatedAt.UnixMilli(),
			Source:          source,
			Model:           sess.Model,
			TotalTurns:      sess.TurnCount(),
			WorkingDir:      wd,
			PermissionMode:  pm,
			ReasoningEffort: re,
			ContextUsage:    ctxUsage,
		})
	}
	return out
}

// SetSubscribed records the active session subscription for a connection.
func (s *Server) SetSubscribed(conn *wsConn, sessionID string) {}

// replayLiveState replays in-progress agent state (progress + stdout) to a
// newly-subscribing browser tab so it catches up with what it missed.
func (s *Server) replayLiveState(sessionID string, conn *wsConn) {
	// Bring the background-task badge up to date for this tab regardless of
	// whether a turn is in flight — background processes outlive turns. Always
	// sent (even with zero running) so switching sessions clears a stale badge.
	infos := tools.SessionBackgroundManager(sessionID).ListRunning()
	if b, err := json.Marshal(backgroundTasksUpdate(sessionID, infos, time.Now())); err == nil {
		conn.send <- b
	}

	// Snapshot the in-progress turn under the read lock: the event buffer,
	// any unflushed streaming deltas, the current progress, and buffered
	// stdout. Marshaling happens inside the lock because the builders and the
	// events slice are mutated by handleEvent under the write lock; sends
	// happen after release.
	var replay [][]byte
	s.liveStateMu.RLock()
	if state, ok := s.liveStates[sessionID]; ok && state.progress != nil {
		// Transcript events broadcast before this tab subscribed. Without
		// this a page refresh mid-turn loses every tool card (and any
		// streamed text) until the turn persists at its end.
		for _, ev := range state.events {
			if b, err := json.Marshal(ev); err == nil {
				replay = append(replay, b)
			}
		}
		// The in-flight round's deltas, not yet folded into events.
		if state.thinkingBuf.Len() > 0 {
			if b, err := json.Marshal(map[string]any{
				"type":       "thinking_delta",
				"session_id": sessionID,
				"text":       state.thinkingBuf.String(),
			}); err == nil {
				replay = append(replay, b)
			}
		}
		if state.textBuf.Len() > 0 {
			if b, err := json.Marshal(map[string]any{
				"type":       "text_delta",
				"session_id": sessionID,
				"text":       state.textBuf.String(),
			}); err == nil {
				replay = append(replay, b)
			}
		}
		p := state.progress
		if b, err := json.Marshal(map[string]any{
			"type":          "progress",
			"session_id":    sessionID,
			"message":       p.Message,
			"progress_type": p.ProgressType,
			"phase":         "active",
			"status":        "start",
			"started_at":    p.StartedAt,
		}); err == nil {
			replay = append(replay, b)
		}
		if len(state.stdoutLines) > 0 {
			if b, err := json.Marshal(map[string]any{
				"type":       "tool_stdout",
				"session_id": sessionID,
				"lines":      state.stdoutLines,
			}); err == nil {
				replay = append(replay, b)
			}
		}
	}
	s.liveStateMu.RUnlock()

	// Buffered transcript events go out before the pending prompt so the tab
	// rebuilds the transcript in broadcast order — the prompt was asked after
	// the tool calls that precede it. Non-blocking sends mirror the hub's
	// slow-consumer policy; a fresh connection's 256-slot buffer always fits
	// a full replay.
	for _, b := range replay {
		select {
		case conn.send <- b:
		default:
		}
	}

	// Replay an outstanding interactive prompt. Its original broadcast only
	// reached the tabs connected at the time; without this, a page refresh
	// during ask_user_question / a permission confirmation leaves the new tab
	// stuck on a spinner with no way to answer.
	s.pendingPromptMu.Lock()
	pendingQ, hasQ := s.pendingQuestions[sessionID]
	pendingC, hasC := s.pendingConfirms[sessionID]
	s.pendingPromptMu.Unlock()
	if hasQ {
		if b, err := json.Marshal(pendingQ); err == nil {
			conn.send <- b
		}
	}
	if hasC {
		if b, err := json.Marshal(pendingC); err == nil {
			conn.send <- b
		}
	}
}

// handleWSUserMessage processes a user message from the WebSocket.
// When a turn is already running the message is enqueued as steer and surfaced
// to the frontend as a pending ghost; the turn loop consumes it automatically.
func (s *Server) handleWSUserMessage(conn *wsConn, msg *wsMsgUserMessage) {
	sid := msg.SessionID
	if sid == "" {
		return
	}

	content := extractTextContent(msg.Content)
	att := parseUserFiles(msg.Files)
	if content == "" && len(att.blocks) == 0 && len(att.notes) == 0 {
		return
	}
	// Document attachments ride as path notes in the text so the model can
	// read_file them and the transcript keeps a visible record.
	if len(att.notes) > 0 {
		content = strings.TrimSpace(content + "\n\n" + strings.Join(att.notes, "\n"))
	}

	sess, err := agent.LoadSession(sid)
	if err != nil {
		s.wsHub.broadcast(sid, map[string]string{
			"type":    "error",
			"message": fmt.Sprintf("session not found: %s", sid),
		})
		return
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()

	if s.turnRunning[sess.ID] {
		mu.Unlock()
		// A mid-turn message has exactly one home: the running Agent's Inbox
		// when it is registered (the runLoop drains it between iterations —
		// attachment blocks and all), the steer queue otherwise (consumed by
		// runAgentTurnLoop as the next chained turn). Enqueueing into both,
		// as this branch once did, processed the same message twice.
		s.sessionAgentsMu.Lock()
		a := s.sessionAgents[sess.ID]
		if a != nil {
			a.Inbox.EnqueueWithBlocks(content, att.blocks)
		}
		s.sessionAgentsMu.Unlock()
		if a == nil {
			s.enqueueSteer(sess.ID, agent.InboxItem{Text: content, Blocks: att.blocks})
		}
		// The frontend already rendered a ghost bubble in _sendMessage;
		// history_user_message (broadcast when the turn drains steer) will
		// replace it.  No need for a separate pending_user_messages event.
		return
	}

	s.turnRunning[sess.ID] = true
	mu.Unlock()

	go func() {
		defer func() {
			mu.Lock()
			s.turnRunning[sess.ID] = false
			mu.Unlock()
		}()
		s.runAgentTurnLoop(sess, content, att.blocks, att.images)
	}()
}

// handleWSInterrupt sends an interrupt signal for a session and broadcasts
// the interrupted event so the frontend shows the cancellation to the user.
func (s *Server) handleWSInterrupt(sessionID string) {
	s.interruptMu.Lock()
	if cancel, ok := s.interrupts[sessionID]; ok {
		cancel()
		delete(s.interrupts, sessionID)
	}
	s.interruptMu.Unlock()

	s.wsHub.broadcast(sessionID, map[string]any{
		"type":       "interrupted",
		"session_id": sessionID,
	})
}

// lastVisibleUserIdx returns the index of the most recent user message that
// is a real prompt: not a tool_result carrier, with user-visible text (after
// stripping <system-reminder> spans) or an image attachment. -1 if none.
// Backwards-scanning for RoleUser alone would land on the tool_result carrier
// of an agentic turn and "retry" an empty message.
func lastVisibleUserIdx(msgs []agent.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != agent.RoleUser {
			continue
		}
		carrier, hasImage := false, false
		text := m.Content
		for _, b := range m.Blocks {
			switch b.Type {
			case "tool_result":
				carrier = true
			case "image":
				hasImage = true
			case "text":
				if text == "" {
					text = b.Text
				}
			}
		}
		if carrier {
			continue
		}
		if hasImage || strings.TrimSpace(agent.StripSystemReminders(text)) != "" {
			return i
		}
	}
	return -1
}

// broadcastRollback tells subscribed browsers the transcript tail was
// stripped, so they re-render from the API before any new events stream in.
func (s *Server) broadcastRollback(sessionID string) {
	s.wsHub.broadcast(sessionID, map[string]string{
		"type":       "history_rollback",
		"session_id": sessionID,
	})
}

// handleWSRetry re-runs the last turn by stripping the last assistant reply
// from the session and resending the last user message.
func (s *Server) handleWSRetry(conn *wsConn, sessionID string) {
	sess, err := agent.LoadSession(sessionID)
	if err != nil {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "session not found",
		})
		return
	}

	// Find the last real user prompt and strip everything from it on.
	lastUserIdx := lastVisibleUserIdx(sess.Messages)
	if lastUserIdx < 0 {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "no user message to retry",
		})
		return
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()

	if s.turnRunning[sess.ID] {
		mu.Unlock()
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "Cannot retry while a turn is running. Please interrupt first.",
		})
		return
	}

	userMsg := sess.Messages[lastUserIdx]
	sess.Messages = sess.Messages[:lastUserIdx]
	_ = sess.Save()
	s.broadcastRollback(sessionID)

	// A multipart user message (image attachments) keeps its text in a text
	// block; re-attach the image blocks (rehydrated by LoadSession) so the
	// retried turn re-sends them.
	content := userMsg.Content
	var blocks []agent.ContentBlock
	for _, b := range userMsg.Blocks {
		switch b.Type {
		case "text":
			if content == "" {
				content = b.Text
			}
		case "image":
			blocks = append(blocks, b)
		}
	}
	images := imageRefsFromBlocks(blocks)

	s.turnRunning[sess.ID] = true
	mu.Unlock()

	go func() {
		defer func() {
			mu.Lock()
			s.turnRunning[sess.ID] = false
			mu.Unlock()
		}()
		s.runAgentTurnLoop(sess, content, blocks, images)
	}()
}

// handleWSRollback strips the last turn — the last real user prompt and
// everything after it — without re-running. This is the edit-and-resend flow:
// the browser pulls the original text into the composer before sending
// rollback, the transcript re-renders without the turn, and the edited
// message arrives as a fresh user_message.
func (s *Server) handleWSRollback(conn *wsConn, sessionID string) {
	sess, err := agent.LoadSession(sessionID)
	if err != nil {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "session not found",
		})
		return
	}

	lastUserIdx := lastVisibleUserIdx(sess.Messages)
	if lastUserIdx < 0 {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "no user message to roll back",
		})
		return
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()
	defer mu.Unlock()

	if s.turnRunning[sess.ID] {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "Cannot edit while a turn is running. Please interrupt first.",
		})
		return
	}

	sess.Messages = sess.Messages[:lastUserIdx]
	_ = sess.Save()
	s.broadcastRollback(sessionID)
}

// handleWSRunTask triggers a scheduled task run immediately from the Web UI.
func (s *Server) handleWSRunTask(conn *wsConn, sessionID string) {
	s.initScheduler()
	if s.scheduler == nil {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": "scheduler not available",
		})
		return
	}
	if err := s.scheduler.RunNow(sessionID); err != nil {
		s.wsHub.broadcast(sessionID, map[string]string{
			"type":    "error",
			"message": err.Error(),
		})
	}
}

// handleWSConfirmation delivers a confirmation answer from the browser.
func (s *Server) handleWSConfirmation(confID, result string) {
	s.confirmMu.Lock()
	if ch, ok := s.confirmations[confID]; ok {
		ch <- result
	}
	s.confirmMu.Unlock()
}

// extractTextContent extracts plain text from content which may be a string
// or a multipart array (e.g. [{type:"text", text:"..."}, {type:"image_url", ...}]).
func extractTextContent(raw json.RawMessage) string {
	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try multipart array.
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if t, ok := p["type"].(string); ok && t == "text" {
			if txt, ok := p["text"].(string); ok {
				texts = append(texts, txt)
			}
		}
	}
	return joinNonEmpty(texts, "\n")
}

func joinNonEmpty(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			result += sep + parts[i]
		}
	}
	return result
}

// ─── runAgentTurnLoop / doAgentTurn ────────────────────────────────────────
//
// runAgentTurnLoop consumes steer messages queued mid-turn and chains turns
// until the queue is empty.  This mirrors the TUI's behaviour where inbox
// messages drained after a turn are automatically fed into the next one.
//
// doAgentTurn is the single-turn body shared by the loop and retry paths.

func (s *Server) runAgentTurnLoop(sess *agent.Session, initialContent string, blocks []agent.ContentBlock, images []string) {
	if err := s.drain.begin(); err != nil {
		// Restart drain in progress: surface a retryable error to the
		// browser instead of starting a turn the shutdown would cut short.
		if s.wsHub != nil {
			s.wsHub.broadcast(sess.ID, map[string]string{
				"type":       "error",
				"message":    err.Error(),
				"session_id": sess.ID,
			})
		}
		return
	}
	defer s.drain.end()

	content := initialContent
	for {
		s.doAgentTurn(sess, content, blocks, images)
		if s.drain.isDraining() {
			// Don't chain queued steer messages into fresh turns during a
			// drain — they'd start work the shutdown cuts at the timeout.
			// Tell the user to resend instead of eating the input silently.
			if items := s.drainSteer(sess.ID); len(items) > 0 && s.wsHub != nil {
				s.wsHub.broadcast(sess.ID, map[string]string{
					"type":       "error",
					"message":    errDraining.Error(),
					"session_id": sess.ID,
				})
			}
			break
		}
		steerItems := s.drainSteer(sess.ID)
		if len(steerItems) == 0 {
			break
		}
		// Fold the queued steer items into one chained turn: texts joined,
		// attachment blocks carried over, thumbnails re-derived.
		var texts []string
		blocks = nil
		for _, it := range steerItems {
			if strings.TrimSpace(it.Text) != "" {
				texts = append(texts, it.Text)
			}
			blocks = append(blocks, it.Blocks...)
		}
		content = strings.Join(texts, "\n\n")
		images = imageRefsFromBlocks(blocks)
	}
}

func (s *Server) enqueueSteer(sessionID string, items ...agent.InboxItem) {
	s.steerMu.Lock()
	s.steerQueues[sessionID] = append(s.steerQueues[sessionID], items...)
	s.steerMu.Unlock()
}

func (s *Server) drainSteer(sessionID string) []agent.InboxItem {
	s.steerMu.Lock()
	items := s.steerQueues[sessionID]
	s.steerQueues[sessionID] = nil
	s.steerMu.Unlock()
	return items
}

// crashRecoveryReminder is prepended (as model-facing context, stripped from
// every UI surface by StripSystemReminders) to the first user message of a
// turn whose session transcript still ends mid-turn — meaning the previous
// turn died with the server process. Its rounds were persisted incrementally,
// but the round in flight at the crash is gone, so executed tools may have
// changed state without their results being recorded.
const crashRecoveryReminder = `<system-reminder>The previous turn in this session ended abnormally (the server stopped mid-turn). Tool calls from that turn may have executed and changed state even if their results are missing from this conversation. Verify the current state before repeating or continuing potentially destructive actions.</system-reminder>`

func (s *Server) doAgentTurn(sess *agent.Session, content string, blocks []agent.ContentBlock, images []string) {
	// A transcript that still ends mid-turn here means the previous turn died
	// with the server — a finished or user-interrupted turn always ends on a
	// plain assistant message. Warn the model once: the reminder rides this
	// turn's user message, and the turn's own completion makes the tail clean
	// again, so it cannot re-fire.
	if sess.EndsMidTurn() {
		content = crashRecoveryReminder + "\n\n" + content
	}

	// Build the user message first: its CreatedAt is both the persisted
	// timestamp and the broadcast created_at below, so the live event and a
	// concurrent history fetch carry the SAME dedup key. (Mirror
	// appendUserInput's multipart shape — optional text block, then
	// attachments — so the persisted line matches what RunStream appends.)
	userMsg := agent.NewUserMessage(content)
	if len(blocks) > 0 {
		multi := make([]agent.ContentBlock, 0, len(blocks)+1)
		if content != "" {
			multi = append(multi, agent.NewTextBlock(content))
		}
		userMsg.Content = ""
		userMsg.Blocks = append(multi, blocks...)
	}

	// Confirm the user message immediately so the frontend can swap the
	// ghost (.msg-pending) bubble for the real one before streaming starts.
	// <system-reminder> spans are stripped from the bubble: a turn kicked by a
	// completion note (kickIdleSteerTurn) is pure reminder and renders nothing.
	visible := strings.TrimSpace(agent.StripSystemReminders(content))
	if visible != "" || len(images) > 0 {
		userEvent := map[string]any{
			"type":       "history_user_message",
			"session_id": sess.ID,
			"content":    visible,
			"created_at": userMsg.CreatedAt.UnixMilli(),
		}
		if len(images) > 0 {
			userEvent["images"] = images
		}
		s.wsHub.broadcast(sess.ID, userEvent)
	}

	// Persist the user message right away so a page refresh mid-turn doesn't
	// lose it.  We append it for Save(), then pop it back off so buildAgent
	// doesn't double-count it — RunStream will add the same message to
	// a.History via appendUserInput. The count including the user message is
	// the turn's history watermark: while the turn runs, the history endpoint
	// serves only messages below it and the WS replay buffer owns the rest.
	sess.Messages = append(sess.Messages, userMsg)
	_ = sess.Save()
	historyWatermark := len(sess.Messages)
	sess.Messages = sess.Messages[:len(sess.Messages)-1]

	sw := s.newWSStreamWriter(sess.ID)

	if err := s.ensureSender(); err != nil {
		sw.error(err.Error())
		return
	}

	// Set up progress tracking for this session, seeded with an initial
	// "thinking" phase that is broadcast immediately. Building the agent,
	// connecting to the provider, and the model's pre-text reasoning can take
	// several seconds during which no tool or text event fires. Without this
	// seed the frontend swaps the ghost bubble for the real one and then sits
	// with no indicator until the first delta — the session looks hung. The
	// real tool/text events adopt this progress element in place (the frontend
	// keys on started_at), so there is no flicker, and a late-subscribing tab
	// replays it via replayLiveState.
	startedAt := time.Now().UnixMilli()
	s.liveStateMu.Lock()
	s.liveStates[sess.ID] = &sessionLiveState{
		progress: &wsEventProgress{
			Type:         "progress",
			ProgressType: "thinking",
			Phase:        "active",
			StartedAt:    startedAt,
		},
		historyWatermark: historyWatermark,
	}
	s.liveStateMu.Unlock()
	s.wsHub.broadcast(sess.ID, map[string]any{
		"type":          "progress",
		"session_id":    sess.ID,
		"progress_type": "thinking",
		"phase":         "active",
		"status":        "start",
		"started_at":    startedAt,
	})

	defer func() {
		// Clear live state at the end of the turn.
		s.liveStateMu.Lock()
		delete(s.liveStates, sess.ID)
		s.liveStateMu.Unlock()
	}()

	runCtx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKeySessionID{}, sess.ID))
	s.registerInterrupt(sess.ID, cancel)
	defer func() {
		cancel()
		s.interruptMu.Lock()
		delete(s.interrupts, sess.ID)
		s.interruptMu.Unlock()
	}()

	// Tell the frontend the turn started: "running" is what shows the
	// interrupt button (the turn-end paths broadcast "idle" to hide it).
	s.wsHub.broadcast(sess.ID, map[string]any{
		"type":       "session_update",
		"session_id": sess.ID,
		"status":     "running",
	})

	a := s.buildAgent(sess)
	if len(blocks) > 0 {
		// Image attachments fold into the same user turn as the text when
		// RunStream appends the user input.
		a.AttachUserBlocks(blocks)
	}

	// Flush any steer messages that arrived before the Agent was built into
	// the Agent's Inbox so the runLoop can drain them between iterations.
	for _, it := range s.drainSteer(sess.ID) {
		a.Inbox.EnqueueWithBlocks(it.Text, it.Blocks)
	}

	// Register this Agent so concurrent mid-turn messages can reach its Inbox.
	s.sessionAgentsMu.Lock()
	s.sessionAgents[sess.ID] = a
	s.sessionAgentsMu.Unlock()
	defer func() {
		s.sessionAgentsMu.Lock()
		delete(s.sessionAgents, sess.ID)
		s.sessionAgentsMu.Unlock()
	}()
	// Messages still in the Inbox when the turn ends — they arrived during the
	// final LLM round, or the turn errored out before draining them — go back
	// to the steer queue so runAgentTurnLoop chains them into the next turn,
	// which answers, broadcasts, and persists them exactly once. (Runs before
	// the deregistration defer above, so no message slips past both homes.)
	defer func() {
		if items := a.Inbox.Drain(); len(items) > 0 {
			s.enqueueSteer(sess.ID, items...)
		}
	}()

	var toolDefs []agent.ToolDefinition
	var executor agent.ToolExecutor
	if s.cfg.Tools {
		var perr error
		// prepareToolTurn wires the session-scoped sub-agent manager's hooks
		// (live-panel events + completion notes to the model).
		runCtx, executor, _, perr = s.prepareToolTurn(runCtx, a)
		if perr != nil {
			sw.error(perr.Error())
			return
		}
		toolDefs = tools.DefaultToolsFor(a.Model)
		// Surface background-process completions (badge + chat notice).
		s.wireBackgroundTaskNotices(sess.ID)
	}

	// Persist the turn's progress incrementally: after any event that grew or
	// rewrote history, flush it to disk so a server crash mid-turn loses at
	// most the round in flight, not the whole turn. The length/dirty gate
	// makes the per-delta calls free — Save itself is also a no-op when
	// nothing changed. RunStream invokes the handler synchronously on this
	// goroutine, so sess needs no extra locking.
	lastSavedLen := -1
	persistTurnProgress := func() {
		if n := a.History.Len(); n != lastSavedLen || a.History.RewriteDirty() {
			sess.SyncFrom(a.History)
			if sess.Save() == nil {
				lastSavedLen = n
			}
		}
	}
	handler := func(ev agent.AgentEvent) {
		sw.handleEvent(ev)
		persistTurnProgress()
	}

	reply, err := a.RunStream(runCtx, content, toolDefs, executor, handler)

	// Save history even on interrupt — finishInterrupted repairs it so the
	// session stays well-formed for the next turn.
	sess.SyncFrom(a.History)
	_ = sess.Save()

	// The turn is persisted: drop the live state (and its replay buffer) now,
	// before any further broadcasts, so a tab subscribing from here on
	// rebuilds from history alone instead of also replaying buffered events
	// on top of it. (This is the primary cleanup point — the EventTurnDone
	// handler deliberately leaves the state alone; the deferred delete stays
	// as a backstop for panics.)
	s.liveStateMu.Lock()
	delete(s.liveStates, sess.ID)
	s.liveStateMu.Unlock()

	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Interrupted — finishInterrupted already emitted EventTurnDone,
			// so turn_done + assistant_message were broadcast by the handler.
			// Nothing more for the reply itself.
		} else {
			wd, pm, re, _ := s.sessionStatusFields()
			sw.error(err.Error())
			s.wsHub.broadcast(sess.ID, map[string]any{
				"type":             "session_update",
				"session_id":       sess.ID,
				"status":           "idle",
				"working_dir":      wd,
				"permission_mode":  pm,
				"reasoning_effort": re,
			})
			return
		}
	} else {
		// Normal completion: emit the final turn_done explicitly so the
		// frontend gets the aggregated reply even when the provider path
		// doesn't fire EventTurnDone (fallback buffered sender).
		rCopy := reply
		b, _ := json.Marshal(map[string]any{
			"type":       "turn_done",
			"session_id": sess.ID,
			"reply":      map[string]any{"content": rCopy.Content},
		})
		sw.sendRaw(b)
	}

	s.wsHub.broadcast(sess.ID, map[string]any{
		"type":       "complete",
		"session_id": sess.ID,
		"iterations": a.TurnIterations(),
	})

	used, window := a.ContextUsage()
	ctxPct := 0
	if window > 0 {
		ctxPct = used * 100 / window
		if ctxPct > 100 {
			ctxPct = 100
		}
	}
	wd, pm, re, _ := s.sessionStatusFields()
	s.wsHub.broadcast(sess.ID, map[string]any{
		"type":             "session_update",
		"session_id":       sess.ID,
		"status":           "idle",
		"context_usage":    ctxPct,
		"working_dir":      wd,
		"permission_mode":  pm,
		"reasoning_effort": re,
	})

	// Once per session, after its first successful turn: generate a sidebar
	// title (matches TUI titleCmd behaviour). Fire-and-forget; a failure is
	// silent and simply retried after a later turn.
	if err == nil && isAutoNamePlaceholder(sess.Title) && s.claimTitleGeneration(sess.ID) {
		sid := sess.ID
		go func() {
			defer s.releaseTitleGeneration(sid)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			t, terr := a.GenerateTitle(ctx, toolDefs)
			if terr != nil || strings.TrimSpace(t) == "" {
				return
			}
			// Apply the title to a freshly loaded Session, not the live one —
			// chained steer turns keep appending to sess on the loop
			// goroutine, and Session isn't goroutine-safe. A load that races
			// a concurrent append just errors out and a later turn retries.
			fresh, lerr := agent.LoadSession(sid)
			if lerr != nil || !isAutoNamePlaceholder(fresh.Title) {
				return
			}
			if fresh.SetTitle(t) != nil {
				return
			}
			// Global broadcast: the sidebar lists every session, so every
			// connected tab needs the rename, not just this session's
			// subscribers.
			s.wsHub.broadcast("", map[string]any{
				"type":       "session_renamed",
				"session_id": sid,
				"name":       t,
			})
		}()
	}

	// After-turn follow-up suggestion (matches TUI suggestCmd behaviour).
	// Fire-and-forget: the frontend shows it as ghost text; failures are silent.
	if err == nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			text, serr := a.Suggest(ctx, toolDefs)
			if serr != nil || strings.TrimSpace(text) == "" {
				return
			}
			s.wsHub.broadcast(sess.ID, map[string]any{
				"type":       "next_message_suggestion",
				"session_id": sess.ID,
				"text":       text,
			})
		}()
	}
}

// sessionPlaceholderRe matches the frontend's auto-assigned "Session N"
// default name on freshly created web sessions.
var sessionPlaceholderRe = regexp.MustCompile(`^Session \d+$`)

// isAutoNamePlaceholder reports whether a session title is absent or still the
// frontend's "Session N" placeholder — both get replaced by a generated title
// after the first completed turn. A name the user typed themselves is kept.
func isAutoNamePlaceholder(title string) bool {
	t := strings.TrimSpace(title)
	return t == "" || sessionPlaceholderRe.MatchString(t)
}

// claimTitleGeneration marks a title generation in flight for the session;
// it returns false when one is already running so concurrent turn ends don't
// spend duplicate provider calls. The map is lazily initialised because tests
// build minimal Servers by hand.
func (s *Server) claimTitleGeneration(sessionID string) bool {
	s.titleMu.Lock()
	defer s.titleMu.Unlock()
	if s.titlePending == nil {
		s.titlePending = make(map[string]bool)
	}
	if s.titlePending[sessionID] {
		return false
	}
	s.titlePending[sessionID] = true
	return true
}

func (s *Server) releaseTitleGeneration(sessionID string) {
	s.titleMu.Lock()
	delete(s.titlePending, sessionID)
	s.titleMu.Unlock()
}

// ─── wsStreamWriter ────────────────────────────────────────────────────────

type wsStreamWriter struct {
	sessionID string
	hub       *wsHub
	server    *Server // for live state tracking
	mu        sync.Mutex
}

func (s *Server) newWSStreamWriter(sessionID string) *wsStreamWriter {
	return &wsStreamWriter{
		sessionID: sessionID,
		hub:       s.wsHub,
		server:    s,
	}
}

func (w *wsStreamWriter) sendRaw(data []byte) {
	w.hub.broadcast(w.sessionID, json.RawMessage(data))
}

func (w *wsStreamWriter) error(msg string) {
	w.hub.broadcast(w.sessionID, map[string]string{
		"type":       "tool_error",
		"session_id": w.sessionID,
		"error":      msg,
	})
}

// bufferTurnEvent records an already-broadcast turn event in the session's
// live state so replayLiveState can resend it to a tab that subscribes
// mid-turn. Pending deltas are flushed first: anything accumulated by then
// was streamed before this event, so replay order matches broadcast order.
func (w *wsStreamWriter) bufferTurnEvent(ev map[string]any) {
	w.server.liveStateMu.Lock()
	if ls, ok := w.server.liveStates[w.sessionID]; ok {
		ls.flushDeltas(w.sessionID)
		ls.appendEvent(ev)
	}
	w.server.liveStateMu.Unlock()
}

// reseedThinkingProgress broadcasts a fresh "thinking" progress phase and
// records it as the session's live state. Called after a tool finishes (or
// errors): the next LLM round is already running but stays silent until its
// first delta, so without this the indicator the frontend cleared at
// tool_call never comes back between rounds.
func (w *wsStreamWriter) reseedThinkingProgress() {
	startedAt := time.Now().UnixMilli()
	w.server.liveStateMu.Lock()
	if ls, ok := w.server.liveStates[w.sessionID]; ok {
		ls.progress = &wsEventProgress{
			Type:         "progress",
			ProgressType: "thinking",
			Phase:        "active",
			StartedAt:    startedAt,
		}
	}
	w.server.liveStateMu.Unlock()
	w.hub.broadcast(w.sessionID, map[string]any{
		"type":          "progress",
		"session_id":    w.sessionID,
		"progress_type": "thinking",
		"phase":         "active",
		"status":        "start",
		"started_at":    startedAt,
	})
}

// handleEvent converts agent.AgentEvent to WS JSON events and broadcasts them.
// It also updates the server's live state for late-subscriber replay.
func (w *wsStreamWriter) handleEvent(ev agent.AgentEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()

	switch ev.Kind {
	case agent.EventTextDelta:
		w.hub.broadcast(w.sessionID, map[string]string{
			"type":       "text_delta",
			"session_id": w.sessionID,
			"text":       ev.Text,
		})
		w.server.liveStateMu.Lock()
		if ls, ok := w.server.liveStates[w.sessionID]; ok {
			ls.textBuf.WriteString(ev.Text)
		}
		w.server.liveStateMu.Unlock()

	case agent.EventToolStarted:
		evt := map[string]any{
			"type":       "tool_call",
			"session_id": w.sessionID,
			"name":       ev.ToolName,
			"args":       ev.Input,
			"tool_id":    ev.ToolID,
		}
		w.hub.broadcast(w.sessionID, evt)

		// Track as live state for replay: fold the finished round's deltas
		// into the buffer first so replay keeps text before its tool call.
		w.server.liveStateMu.Lock()
		if ls, ok := w.server.liveStates[w.sessionID]; ok {
			ls.flushDeltas(w.sessionID)
			ls.appendEvent(evt)
			ls.progress = &wsEventProgress{
				Type:         "progress",
				Message:      ev.ToolName,
				ProgressType: "tool",
				Phase:        "active",
				StartedAt:    time.Now().UnixMilli(),
			}
		}
		w.server.liveStateMu.Unlock()

	case agent.EventToolDone:
		toolResult := map[string]any{
			"type":       "tool_result",
			"session_id": w.sessionID,
			"result":     ev.Output,
			"tool_id":    ev.ToolID,
		}
		if ev.UI != nil {
			toolResult["ui_payload"] = ev.UI
		}
		w.hub.broadcast(w.sessionID, toolResult)
		w.bufferTurnEvent(toolResult)
		// The agent immediately starts the next LLM round after a tool result,
		// and that round emits no event until its first delta (or the next
		// tool_call). Re-seed the "thinking" indicator so the UI animates
		// across the gap instead of the next tool popping out of dead air —
		// same rationale as the turn-start seed in doAgentTurn. The frontend
		// clears it on the next tool_call / assistant_message / complete.
		// (No sub_agent_done broadcast here: in async mode the tool returns
		// while agents still run — the manager's per-agent "done" event is
		// the completion signal for the live panel.)
		w.reseedThinkingProgress()
		// A tool call is the only place a turn starts or kills a background
		// process — refresh the badge.
		w.server.broadcastBackgroundTasks(w.sessionID)

	case agent.EventToolError:
		evt := map[string]any{
			"type":       "tool_error",
			"session_id": w.sessionID,
			"error":      ev.Err,
			"tool_id":    ev.ToolID,
		}
		w.hub.broadcast(w.sessionID, evt)
		w.bufferTurnEvent(evt)
		// A tool error does not end the turn — the error result goes back to
		// the model, which keeps running. Re-seed the indicator just like
		// EventToolDone (deleting the live state here would also blank the
		// progress replay for late-subscribing tabs mid-turn).
		w.reseedThinkingProgress()
		w.server.broadcastBackgroundTasks(w.sessionID)

	case agent.EventThinkingDelta:
		w.hub.broadcast(w.sessionID, map[string]string{
			"type":       "thinking_delta",
			"session_id": w.sessionID,
			"text":       ev.Text,
		})
		w.server.liveStateMu.Lock()
		if ls, ok := w.server.liveStates[w.sessionID]; ok {
			ls.thinkingBuf.WriteString(ev.Text)
		}
		w.server.liveStateMu.Unlock()

	case agent.EventCompactDone:
		// Compaction folded the FoldedMsgs oldest messages into one summary
		// message, shifting every later index down — including the turn's
		// history watermark. Skip no-op compactions (summarization failed or
		// returned nothing; history untouched), signalled by before == after.
		if c := ev.Compact; c != nil && c.FoldedMsgs > 0 && c.BeforeTokens != c.AfterTokens {
			w.server.liveStateMu.Lock()
			if ls, ok := w.server.liveStates[w.sessionID]; ok && ls.historyWatermark > 0 {
				if nw := ls.historyWatermark - c.FoldedMsgs + 1; nw > 1 {
					ls.historyWatermark = nw
				} else {
					// Compaction reached into the current turn: only the
					// summary message itself predates the turn now.
					ls.historyWatermark = 1
				}
			}
			w.server.liveStateMu.Unlock()
		}

	case agent.EventSteerInjected:
		// Prefer the full inbox items (text + attachment blocks) so a steer
		// message's image thumbnails reach the bubble; Messages is the
		// text-only fallback for events from older emitters.
		items := ev.Steer
		if len(items) == 0 {
			for _, msg := range ev.Messages {
				items = append(items, agent.InboxItem{Text: msg})
			}
		}
		for _, it := range items {
			// <system-reminder> blocks (background-process completion notes,
			// recalled memories) are model-facing context, not user speech —
			// the TUI skips them and the web transcript must too.
			text := strings.TrimSpace(agent.StripSystemReminders(it.Text))
			imgs := imageRefsFromBlocks(it.Blocks)
			if text == "" && len(imgs) == 0 {
				continue
			}
			evt := map[string]any{
				"type":       "history_user_message",
				"session_id": w.sessionID,
				"content":    text,
				"created_at": time.Now().UnixMilli(),
			}
			if len(imgs) > 0 {
				evt["images"] = imgs
			}
			w.hub.broadcast(w.sessionID, evt)
			// Steer messages persist only at turn end like everything else
			// in the turn — buffer them so a refresh keeps the bubble.
			w.bufferTurnEvent(evt)
		}

	case agent.EventTurnDone:
		if ev.Reply != nil {
			w.hub.broadcast(w.sessionID, map[string]any{
				"type":       "turn_done",
				"session_id": w.sessionID,
				"reply":      map[string]any{"content": ev.Reply.Content},
			})
			// Frontend expects a complete assistant_message event rather than
			// streaming text_delta fragments. Emit it once the turn is done.
			w.hub.broadcast(w.sessionID, map[string]any{
				"type":       "assistant_message",
				"session_id": w.sessionID,
				"content":    ev.Reply.Content,
				"thinking":   extractThinking(ev.Reply),
			})
		}
		// Live state is NOT cleared here: this event fires inside RunStream,
		// before doAgentTurn persists the session, and a tab subscribing in
		// that gap needs the replay buffer (history doesn't have the turn
		// yet). doAgentTurn drops the state right after Save.
	}
}

// ─── Server fields for WS ──────────────────────────────────────────────────

// ws fields added to Server.
func (s *Server) initWS() {
	s.wsHub = newWSHub()
	s.wsHub.init(s)
	s.interrupts = make(map[string]context.CancelFunc)
	s.confirmations = make(map[string]chan string)
	s.liveStates = make(map[string]*sessionLiveState)
}

// toolDefsFor returns tool definitions for the given model.
func (s *Server) toolDefsFor(model string) []agent.ToolDefinition {
	// Import cycle avoidance — tools.DefaultToolsFor is called from handlers.go
	return getDefaultToolsFor(model)
}

// Interrupt cancellation tracking.
func (s *Server) registerInterrupt(sessionID string, cancel context.CancelFunc) {
	s.interruptMu.Lock()
	s.interrupts[sessionID] = cancel
	s.interruptMu.Unlock()
}

// sessionStatus returns the status string the frontend keys on ("running"
// shows the interrupt button; anything else hides it). A registered interrupt
// cancel func exists exactly for the duration of a turn — read via its own
// mutex, unlike the turnRunning map which is guarded by per-session turnLocks.
func (s *Server) sessionStatus(sessionID string) string {
	s.interruptMu.Lock()
	defer s.interruptMu.Unlock()
	if _, ok := s.interrupts[sessionID]; ok {
		return "running"
	}
	return "idle"
}

// Request confirmation from user (blocks until user responds in browser or ctx
// is cancelled).
func (s *Server) requestConfirmation(ctx context.Context, sessionID, message, kind string) (string, error) {
	confID := fmt.Sprintf("conf_%d", time.Now().UnixNano())
	ch := make(chan string, 1)

	s.confirmMu.Lock()
	s.confirmations[confID] = ch
	s.confirmMu.Unlock()

	ev := wsEventRequestConfirmation{
		Type:      "request_confirmation",
		SessionID: sessionID,
		ConfID:    confID,
		Message:   message,
		Kind:      kind,
	}

	// Record the outstanding confirmation so a tab that (re)subscribes
	// mid-ask — e.g. after a page refresh — gets it replayed.
	s.pendingPromptMu.Lock()
	s.pendingConfirms[sessionID] = ev
	s.pendingPromptMu.Unlock()

	cleanup := func() {
		s.confirmMu.Lock()
		delete(s.confirmations, confID)
		s.confirmMu.Unlock()
		s.pendingPromptMu.Lock()
		delete(s.pendingConfirms, sessionID)
		s.pendingPromptMu.Unlock()
	}

	s.wsHub.broadcast(sessionID, ev)

	// Wait for response, timeout, or cancellation.
	select {
	case result := <-ch:
		cleanup()
		return result, nil
	case <-ctx.Done():
		cleanup()
		return "", fmt.Errorf("confirmation cancelled")
	case <-time.After(5 * time.Minute):
		cleanup()
		return "", fmt.Errorf("confirmation timed out")
	}
}

// extractThinking pulls a reasoning/thinking trace from a Reply's Blocks so the
// web UI can render it alongside the final assistant message. Anthropic models
// return it as a standalone "thinking" block; OpenAI models stash it on the
// first "tool_use" block (Reasoning field). Empty string when none is present.
func extractThinking(reply *agent.Reply) string {
	if reply == nil {
		return ""
	}
	for _, b := range reply.Blocks {
		if b.Type == "thinking" && b.Thinking != "" {
			return b.Thinking
		}
		if b.Type == "tool_use" && b.Reasoning != "" {
			return b.Reasoning
		}
	}
	return ""
}
