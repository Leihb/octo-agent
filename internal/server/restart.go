package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// ExitRestart is the process exit code the serve worker uses to ask its
// supervisor for a respawn. The supervisor (cmd/octo serve, or an external
// one via e.g. systemd RestartForceExitStatus) re-spawns the worker from the
// on-disk binary path, so a replaced binary takes effect on restart.
const ExitRestart = 42

// ErrRestartRequested is returned by ListenAndServe when the server stopped
// because a restart was requested (API or restart_server tool), as opposed
// to a plain shutdown. Callers map it to ExitRestart.
var ErrRestartRequested = errors.New("server restart requested")

// shutdownTimeout bounds the http.Shutdown wait during a restart.
const shutdownTimeout = 10 * time.Second

// defaultDrainTimeout is the hard bound on waiting for in-flight turns
// before a restart proceeds anyway. Round-granularity session persistence
// plus the SSE replay buffer cap the damage of a forced cut at one round.
const defaultDrainTimeout = 30 * time.Second

// Restart requests a graceful restart. It is idempotent — only the first
// call wins — and returns immediately; the drain and shutdown run in the
// background so the caller (an HTTP handler or a tool executing inside a
// turn, whose own turn the drain waits for) is not blocked. reason is
// logged for the operator.
func (s *Server) Restart(reason string) {
	if !s.restartPending.CompareAndSwap(false, true) {
		return
	}
	slog.Info("restart requested", "reason", reason)
	go func() {
		timeout := s.drainTimeout
		if timeout <= 0 {
			timeout = defaultDrainTimeout
		}
		if clean := s.drain.drain(timeout); !clean {
			slog.Warn("drain timed out; restarting with turns in flight", "timeout", timeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()
}

// handleRestart serves POST /api/restart. It acknowledges with 202 before
// the shutdown begins; the caller observes the actual restart as a dropped
// connection followed by the new worker accepting again.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.Restart("api")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
}
