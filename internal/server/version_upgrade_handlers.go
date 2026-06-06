package server

import (
	"net/http"
	"os"
	"os/exec"
	"runtime"

	"github.com/Leihb/octo-agent/internal/version"
)

// ─── POST /api/version/upgrade ──────────────────────────────────────────────

func (s *Server) handleVersionUpgrade(w http.ResponseWriter, r *http.Request) {
	// The Go rewrite is a single binary — upgrading it is outside the scope
	// of the server process. Return a stub that tells the user to use their
	// package manager or download a new release.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      false,
		"message": "Upgrade is not supported in the Go rewrite. Please install the latest release manually.",
	})
}

// ─── POST /api/restart ──────────────────────────────────────────────────────

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	// Best-effort self-restart: exec the same binary with the same args.
	// The HTTP response is sent before the process exits so the client
	// gets a clean 200.
	go func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		// Give the new process a moment to start, then exit.
		if runtime.GOOS != "windows" {
			_ = cmd.Process.Release()
		}
		os.Exit(0)
	}()

	writeJSON(w, http.StatusOK, map[string]any{"restarting": true})
}

// ─── GET /api/version (extended) ────────────────────────────────────────────

func (s *Server) handleVersionExtended(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        version.Version,
		"needs_update":   false,
		"latest_version": version.Version,
	})
}
