package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/upgrade"
	"github.com/Leihb/octo-agent/internal/version"
)

// Latest-version cache TTLs: successes are fresh for an hour, failures
// back off for ten minutes. The /api/version endpoint is unauthenticated,
// so the cache is also what keeps a request flood from becoming an
// outbound-request flood.
const (
	versionCheckTTL     = time.Hour
	versionCheckBackoff = 10 * time.Minute
	versionCheckTimeout = 3 * time.Second
)

// ─── GET /api/version ────────────────────────────────────────────────────────

// handleVersion reports the running version plus update availability.
// `version` is the original field; `current`/`latest`/`needs_update`/
// `cli_command` are what the web badge reads. cli_command is a constant —
// the endpoint is unauthenticated, so the executable's filesystem path
// must not leak here.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	current := strings.TrimPrefix(version.Version, "v")
	latest, needs := s.latestVersion()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      version.Version,
		"current":      current,
		"latest":       latest,
		"needs_update": needs,
		"cli_command":  "octo",
	})
}

// latestVersion resolves the latest released version through the cache.
// The update check is opt-in via Config.UpdateCheck (set only by `octo
// serve`), so every other Server constructor — the test suite included —
// performs no outbound calls and degrades to "current is latest".
func (s *Server) latestVersion() (string, bool) {
	current := strings.TrimPrefix(version.Version, "v")
	if !s.cfg.UpdateCheck {
		return current, false
	}

	// The mutex doubles as single-flight: under a flood, one caller does
	// the network round-trip and the rest line up to read its cache entry.
	s.versionCheckMu.Lock()
	defer s.versionCheckMu.Unlock()

	now := time.Now()
	if s.versionLatest != "" && now.Sub(s.versionCheckedAt) < versionCheckTTL {
		return s.versionLatest, needsUpdate(current, s.versionLatest)
	}
	if !s.versionFailedAt.IsZero() && now.Sub(s.versionFailedAt) < versionCheckBackoff {
		return current, false
	}

	// Background-derived, not the request context: a client navigating
	// away mid-check would otherwise record a failure and degrade the
	// badge for every client for the whole backoff window.
	cctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
	defer cancel()
	latest, err := upgrade.Check(cctx)
	if err != nil {
		s.versionFailedAt = now
		return current, false
	}
	s.versionLatest, s.versionCheckedAt, s.versionFailedAt = latest, now, time.Time{}
	return latest, needsUpdate(current, latest)
}

// needsUpdate is true only for an upgradeable build that is actually
// behind — dev builds never grow the badge, matching upgrade.Run's own
// eligibility refusal.
func needsUpdate(current, latest string) bool {
	if upgrade.Eligible() != nil {
		return false
	}
	return upgrade.CompareVersions(current, latest) < 0
}

// ─── POST /api/version/upgrade ──────────────────────────────────────────────

// handleVersionUpgrade starts a binary upgrade in the background,
// single-flight. Progress streams over WS as upgrade_log lines; every
// outcome — including refusals past this point — ends in an
// upgrade_complete broadcast, because the frontend ignores this response's
// status code and only a completion event unwedges its popover.
func (s *Server) handleVersionUpgrade(w http.ResponseWriter, r *http.Request) {
	s.upgradeMu.Lock()
	if s.upgradeRunning {
		s.upgradeMu.Unlock()
		writeError(w, http.StatusConflict, "an upgrade is already in progress")
		return
	}
	s.upgradeRunning = true
	s.upgradeMu.Unlock()

	go s.runBinaryUpgrade()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": "upgrade started"})
}

// runBinaryUpgrade downloads outside the restart drain gate and holds the
// gate only for the swap: a restart requested mid-install drains behind
// the renames instead of exiting between them — which would leave no file
// at the target path and make the supervisor respawn fail outright.
func (s *Server) runBinaryUpgrade() {
	defer func() {
		s.upgradeMu.Lock()
		s.upgradeRunning = false
		s.upgradeMu.Unlock()
	}()

	logf := func(line string) {
		s.broadcastGlobal(map[string]any{"type": "upgrade_log", "line": line})
	}
	complete := func(ok bool) {
		s.broadcastGlobal(map[string]any{"type": "upgrade_complete", "success": ok})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// No Force over HTTP: a dev-build server refuses inside Prepare, and
	// the refusal (with its --force CLI hint) reaches the user as a log
	// line. The slow part (download/verify/extract) runs OUTSIDE the
	// drain gate — Restart only waits 30s for gate holders before
	// proceeding, so holding it across a download would reopen the
	// exit-between-renames window the gate exists to close.
	p, err := upgrade.Prepare(ctx, upgrade.Options{Log: logf})
	switch {
	case errors.Is(err, upgrade.ErrUpToDate):
		logf("already up to date")
		complete(false)
		return
	case err != nil:
		logf("upgrade failed: " + err.Error())
		complete(false)
		return
	}
	defer p.Close()

	// The swap itself is local renames — seconds, comfortably inside the
	// restart drain bound.
	if err := s.drain.begin(); err != nil {
		logf("the server is restarting — upgrade aborted before install; try again once it is back")
		complete(false)
		return
	}
	err = p.Install()
	s.drain.end()
	if err != nil {
		logf("upgrade failed: " + err.Error())
		complete(false)
		return
	}
	complete(true)
}

// broadcastGlobal sends an event to every WS connection. Nil-safe so
// Server constructions without a WS hub (tests) don't block on a nil
// channel.
func (s *Server) broadcastGlobal(event any) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.broadcast("", event)
}
