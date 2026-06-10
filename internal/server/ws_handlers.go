package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	toolCall    *wsEventToolCall
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
		source := "manual"
		out = append(out, wsSessionInfo{
			ID:              sess.ID,
			Name:            sess.DisplayTitle(),
			Status:          "idle",
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
	s.liveStateMu.RLock()
	state, ok := s.liveStates[sessionID]
	s.liveStateMu.RUnlock()
	if !ok || state.progress == nil {
		return
	}

	// Replay progress.
	p := state.progress
	b, _ := json.Marshal(map[string]any{
		"type":          "progress",
		"session_id":    sessionID,
		"message":       p.Message,
		"progress_type": p.ProgressType,
		"phase":         "active",
		"status":        "start",
		"started_at":    p.StartedAt,
	})
	conn.send <- b

	// Replay buffered stdout.
	if len(state.stdoutLines) > 0 {
		b, _ := json.Marshal(map[string]any{
			"type":       "tool_stdout",
			"session_id": sessionID,
			"lines":      state.stdoutLines,
		})
		conn.send <- b
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

	// Find the last user message and strip everything after it.
	lastUserIdx := -1
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		if sess.Messages[i].Role == agent.RoleUser {
			lastUserIdx = i
			break
		}
	}
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
	_, err := s.scheduler.RunNow(context.Background(), sessionID)
	if err != nil {
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
	content := initialContent
	for {
		s.doAgentTurn(sess, content, blocks, images)
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

func (s *Server) doAgentTurn(sess *agent.Session, content string, blocks []agent.ContentBlock, images []string) {
	// Confirm the user message immediately so the frontend can swap the
	// ghost (.msg-pending) bubble for the real one before streaming starts.
	userEvent := map[string]any{
		"type":       "history_user_message",
		"session_id": sess.ID,
		"content":    content,
		"created_at": time.Now().UnixMilli(),
	}
	if len(images) > 0 {
		userEvent["images"] = images
	}
	s.wsHub.broadcast(sess.ID, userEvent)

	// Persist the user message right away so a page refresh mid-turn doesn't
	// lose it.  We append it for Save(), then pop it back off so buildAgent
	// doesn't double-count it — RunStream will add the same message to
	// a.History via appendUserInput. Mirror appendUserInput's multipart shape
	// (optional text block, then attachments) so the persisted line matches.
	userMsg := agent.NewUserMessage(content)
	if len(blocks) > 0 {
		multi := make([]agent.ContentBlock, 0, len(blocks)+1)
		if content != "" {
			multi = append(multi, agent.NewTextBlock(content))
		}
		userMsg.Content = ""
		userMsg.Blocks = append(multi, blocks...)
	}
	sess.Messages = append(sess.Messages, userMsg)
	_ = sess.Save()
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
	var subMgr *tools.SubAgentManager
	if s.cfg.Tools {
		var perr error
		runCtx, executor, subMgr, perr = s.prepareToolTurn(runCtx, a)
		if perr != nil {
			sw.error(perr.Error())
			return
		}
		toolDefs = tools.DefaultToolsFor(a.Model)
		// Wire sub-agent live-panel events into the WebSocket stream.
		if subMgr != nil {
			subMgr.SetOnEvent(func(ev tools.SubAgentEvent) {
				s.wsHub.broadcast(sess.ID, map[string]any{
					"type":        "sub_agent_event",
					"session_id":  sess.ID,
					"agent_id":    ev.AgentID,
					"description": ev.Description,
					"kind":        ev.Kind,
					"tool_name":   ev.ToolName,
				})
			})
		}
	}

	reply, err := a.RunStream(runCtx, content, toolDefs, executor, sw.handleEvent)

	// Save history even on interrupt — finishInterrupted repairs it so the
	// session stays well-formed for the next turn.
	sess.SyncFrom(a.History)
	_ = sess.Save()

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
		"iterations": 1,
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

	case agent.EventToolStarted:
		tc := wsEventToolCall{
			Type: "tool_call",
			Name: ev.ToolName,
			Args: ev.Input,
		}
		w.hub.broadcast(w.sessionID, map[string]any{
			"type":       tc.Type,
			"session_id": w.sessionID,
			"name":       tc.Name,
			"args":       tc.Args,
		})

		// Track as live state for replay.
		w.server.liveStateMu.Lock()
		if ls, ok := w.server.liveStates[w.sessionID]; ok {
			ls.toolCall = &tc
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
		w.hub.broadcast(w.sessionID, map[string]any{
			"type":       "tool_result",
			"session_id": w.sessionID,
			"result":     ev.Output,
		})
		// Signal sub-agent completion so the frontend can clear the live panel.
		if ev.ToolName == "sub_agent" {
			w.hub.broadcast(w.sessionID, map[string]any{
				"type":       "sub_agent_done",
				"session_id": w.sessionID,
			})
		}
		// Clear live state on tool done.
		w.server.liveStateMu.Lock()
		if ls, ok := w.server.liveStates[w.sessionID]; ok {
			ls.progress = nil
			ls.toolCall = nil
		}
		w.server.liveStateMu.Unlock()

	case agent.EventToolError:
		w.hub.broadcast(w.sessionID, map[string]any{
			"type":       "tool_error",
			"session_id": w.sessionID,
			"error":      ev.Err,
		})
		w.server.liveStateMu.Lock()
		delete(w.server.liveStates, w.sessionID)
		w.server.liveStateMu.Unlock()

	case agent.EventThinkingDelta:
		w.hub.broadcast(w.sessionID, map[string]string{
			"type":       "thinking_delta",
			"session_id": w.sessionID,
			"text":       ev.Text,
		})

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
			evt := map[string]any{
				"type":       "history_user_message",
				"session_id": w.sessionID,
				"content":    it.Text,
				"created_at": time.Now().UnixMilli(),
			}
			if imgs := imageRefsFromBlocks(it.Blocks); len(imgs) > 0 {
				evt["images"] = imgs
			}
			w.hub.broadcast(w.sessionID, evt)
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
		// Clear live state.
		w.server.liveStateMu.Lock()
		delete(w.server.liveStates, w.sessionID)
		w.server.liveStateMu.Unlock()
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

// Request confirmation from user (blocks until user responds in browser or ctx
// is cancelled).
func (s *Server) requestConfirmation(ctx context.Context, sessionID, message, kind string) (string, error) {
	confID := fmt.Sprintf("conf_%d", time.Now().UnixNano())
	ch := make(chan string, 1)

	s.confirmMu.Lock()
	s.confirmations[confID] = ch
	s.confirmMu.Unlock()

	s.wsHub.broadcast(sessionID, wsEventRequestConfirmation{
		Type:    "request_confirmation",
		ConfID:  confID,
		Message: message,
		Kind:    kind,
	})

	// Wait for response, timeout, or cancellation.
	select {
	case result := <-ch:
		s.confirmMu.Lock()
		delete(s.confirmations, confID)
		s.confirmMu.Unlock()
		return result, nil
	case <-ctx.Done():
		s.confirmMu.Lock()
		delete(s.confirmations, confID)
		s.confirmMu.Unlock()
		return "", fmt.Errorf("confirmation cancelled")
	case <-time.After(5 * time.Minute):
		s.confirmMu.Lock()
		delete(s.confirmations, confID)
		s.confirmMu.Unlock()
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
