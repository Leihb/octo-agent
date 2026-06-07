package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// benchmarkResponse is the JSON shape the Web UI expects.
type benchmarkResponse struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error,omitempty"`
	Results []benchmarkResult `json:"results,omitempty"`
}

type benchmarkResult struct {
	ModelID string `json:"model_id"`
	OK      bool   `json:"ok"`
	TTFTMs  int    `json:"ttft_ms"`
	Error   string `json:"error,omitempty"`
}

// ─── POST /api/sessions/{id}/benchmark ──────────────────────────────────────

func (s *Server) handleBenchmark(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureSender(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	// Cap the benchmark so a stalled provider doesn't hang the UI.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Resolve the model to benchmark: the session's current model if we can
	// load it, otherwise the server's default.
	model := s.model
	if sess, err := agent.LoadSession(sessionID); err == nil && sess.Model != "" {
		model = sess.Model
	}

	// Build a minimal message.  We intentionally do NOT use the session's
	// full history — this is a cold-start latency probe, not a real turn.
	msgs := []agent.Message{{Role: agent.RoleUser, Content: "hi"}}

	// Try the streaming path so we can measure true TTFT.
	streamingSender, ok := s.sender.(agent.StreamingSender)
	if !ok {
		// Non-streaming fallback: measure total round-trip.
		t0 := time.Now()
		_, err := s.sender.SendMessages(ctx, model, s.system, msgs, 10)
		elapsed := int(time.Since(t0).Milliseconds())
		res := benchmarkResult{
			ModelID: model,
			OK:      err == nil,
			TTFTMs:  elapsed,
		}
		if err != nil {
			res.Error = err.Error()
		}
		writeJSON(w, http.StatusOK, benchmarkResponse{OK: true, Results: []benchmarkResult{res}})
		return
	}

	t0 := time.Now()
	var firstChunk time.Time
	var once sync.Once

	_, err := streamingSender.StreamMessages(ctx, model, s.system, msgs, 10,
		func(textDelta string) {
			once.Do(func() { firstChunk = time.Now() })
		},
		nil, // no thinking delta
	)

	res := benchmarkResult{ModelID: model}
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		// If we got at least one chunk before the error, report the TTFT
		// up to that point — it's still useful signal.
		if !firstChunk.IsZero() {
			res.TTFTMs = int(firstChunk.Sub(t0).Milliseconds())
			res.OK = true // partial success: we measured TTFT
		}
	} else if firstChunk.IsZero() {
		// Stream succeeded but produced zero tokens (unlikely with "hi").
		res.OK = true
		res.TTFTMs = int(time.Since(t0).Milliseconds())
	} else {
		res.OK = true
		res.TTFTMs = int(firstChunk.Sub(t0).Milliseconds())
	}

	writeJSON(w, http.StatusOK, benchmarkResponse{OK: true, Results: []benchmarkResult{res}})
}
