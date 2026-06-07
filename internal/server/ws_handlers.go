package server

import (
	"context"
	"encoding/json"
	"fmt"
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
	out := make([]wsSessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		source := "manual"
		out = append(out, wsSessionInfo{
			ID:         sess.ID,
			Name:       sess.DisplayTitle(),
			Status:     "idle",
			CreatedAt:  sess.CreatedAt.UnixMilli(),
			Source:     source,
			Model:      sess.Model,
			TotalTurns: sess.TurnCount(),
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
func (s *Server) handleWSUserMessage(conn *wsConn, msg *wsMsgUserMessage) {
	sid := msg.SessionID
	if sid == "" {
		return
	}

	content := extractTextContent(msg.Content)
	if content == "" {
		return
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

	s.runAgentTurn(sess, content)
	mu.Unlock()
}

// handleWSInterrupt sends an interrupt signal for a session.
func (s *Server) handleWSInterrupt(sessionID string) {
	s.interruptMu.Lock()
	if cancel, ok := s.interrupts[sessionID]; ok {
		cancel()
		delete(s.interrupts, sessionID)
	}
	s.interruptMu.Unlock()
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

	userMsg := sess.Messages[lastUserIdx]
	sess.Messages = sess.Messages[:lastUserIdx]
	_ = sess.Save()

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()

	s.runAgentTurn(sess, userMsg.Content)
	mu.Unlock()
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

// ─── runAgentTurn ───────────────────────────────────────────────────────────
//
// Shared by handleWSUserMessage and handleWSRetry. Runs the agent turn,
// streams events to WS, saves the session, and cleans up live state.

func (s *Server) runAgentTurn(sess *agent.Session, content string) {
	sw := s.newWSStreamWriter(sess.ID)

	if err := s.ensureSender(); err != nil {
		sw.error(err.Error())
		return
	}

	// Set up progress tracking for this session.
	s.liveStateMu.Lock()
	s.liveStates[sess.ID] = &sessionLiveState{}
	s.liveStateMu.Unlock()

	defer func() {
		// Clear live state at the end of the turn.
		s.liveStateMu.Lock()
		delete(s.liveStates, sess.ID)
		s.liveStateMu.Unlock()
	}()

	runCtx := context.Background()
	a := s.buildAgent(sess)

	var toolDefs []agent.ToolDefinition
	var executor agent.ToolExecutor
	if s.cfg.Tools {
		var perr error
		runCtx, executor, perr = s.prepareToolTurn(runCtx, a)
		if perr != nil {
			sw.error(perr.Error())
			return
		}
		toolDefs = tools.DefaultToolsFor(a.Model)
	}

	reply, err := a.RunStream(runCtx, content, toolDefs, executor, sw.handleEvent)
	if err != nil {
		sw.error(err.Error())
		return
	}

	sess.SyncFrom(a.History)
	_ = sess.Save()

	rCopy := reply
	b, _ := json.Marshal(map[string]any{
		"type":       "turn_done",
		"session_id": sess.ID,
		"reply":      map[string]any{"content": rCopy.Content},
	})
	sw.sendRaw(b)

	s.wsHub.broadcast(sess.ID, map[string]any{
		"type":       "complete",
		"session_id": sess.ID,
		"iterations": 1,
	})

	s.wsHub.broadcast(sess.ID, map[string]any{
		"type":       "session_update",
		"session_id": sess.ID,
		"status":     "idle",
	})
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
		inputJSON := ""
		if ev.Input != nil {
			b, _ := json.Marshal(ev.Input)
			inputJSON = string(b)
		}
		tc := wsEventToolCall{
			Type:    "tool_call",
			Name:    ev.ToolName,
			Args:    ev.Input,
			Summary: fmt.Sprintf("🔧 %s %s", ev.ToolName, inputJSON),
		}
		w.hub.broadcast(w.sessionID, map[string]any{
			"type":       tc.Type,
			"session_id": w.sessionID,
			"name":       tc.Name,
			"args":       tc.Args,
			"summary":    tc.Summary,
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

// Request confirmation from user (blocks until user responds in browser).
func (s *Server) requestConfirmation(sessionID, message, kind string) (string, error) {
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

	// Wait for response or timeout.
	select {
	case result := <-ch:
		s.confirmMu.Lock()
		delete(s.confirmations, confID)
		s.confirmMu.Unlock()
		return result, nil
	case <-time.After(5 * time.Minute):
		s.confirmMu.Lock()
		delete(s.confirmations, confID)
		s.confirmMu.Unlock()
		return "", fmt.Errorf("confirmation timed out")
	}
}
