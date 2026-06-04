package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// sseEvent writes one SSE data line.
func sseEvent(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// handleTurnSSE handles POST /api/chat/:id/turn with Accept: text/event-stream.
// It streams AgentEvents as SSE data lines.
func (s *Server) handleTurnSSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	var req turnRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	sess, err := agent.LoadSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	mu := s.sessionTurnLock(sess.ID)
	mu.Lock()
	defer mu.Unlock()

	// Set up SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	a := s.buildAgent(sess)

	runCtx := r.Context()
	var toolDefs []agent.ToolDefinition
	var executor agent.ToolExecutor
	if s.cfg.Tools {
		var perr error
		runCtx, executor, perr = s.prepareToolTurn(runCtx, a)
		if perr != nil {
			ev := agent.AgentEvent{Kind: agent.EventToolError, Err: perr.Error()}
			b, _ := json.Marshal(ev)
			sseEvent(w, string(b))
			return
		}
		toolDefs = tools.DefaultToolsFor(a.Model)
	}

	eventHandler := func(ev agent.AgentEvent) {
		b, _ := json.Marshal(ev)
		sseEvent(w, string(b))
	}

	reply, err := a.RunStream(runCtx, req.Message, toolDefs, executor, eventHandler)
	if err != nil {
		ev := agent.AgentEvent{Kind: agent.EventToolError, Err: err.Error()}
		b, _ := json.Marshal(ev)
		sseEvent(w, string(b))
		return
	}

	sess.SyncFrom(a.History)
	_ = sess.Save()

	// Final turn_done event.
	rCopy := reply
	b, _ := json.Marshal(agent.AgentEvent{Kind: agent.EventTurnDone, Reply: &rCopy})
	sseEvent(w, string(b))
}
