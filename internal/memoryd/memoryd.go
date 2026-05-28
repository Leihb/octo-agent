// Package memoryd is the C9 Phase 2 cross-session memory daemon. It owns
// the async extraction + consolidation passes that Phase 1 runs at REPL
// startup, moving them off the user's hot path. When the daemon is
// offline, the chat path falls back to Phase 1 — memory still works,
// just synchronously.
//
// v1 scope (per c9-memory-design.md §7):
//   - Manual `octo memoryd start` lifecycle (no auto-spawn).
//   - Foreground process (the user runs it in a separate terminal or
//     backgrounds via shell). No fork/setsid daemonization.
//   - PID file at ~/.octo/memoryd.pid for start/stop/status coordination.
//   - SIGTERM / SIGINT graceful shutdown (finishes the in-flight tick
//     before exiting).
//   - Windows degrades: `octo memoryd start` on Windows refuses with a
//     clear notice and exits non-zero, mirroring the sandbox's Windows
//     fail-soft strategy.
//
// PR1 (this file) wires the lifecycle plumbing without the actual work
// loop — Tick is a placeholder that just logs and sleeps. PR2 will
// implement the extract + consolidate logic against memory.Store +
// agent.ExtractMemory.
package memoryd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultTick is how often the daemon wakes up to check for work. 15s is
// short enough that a freshly-ended chat session sees its memory extracted
// promptly, but long enough that idle CPU cost is negligible.
const DefaultTick = 15 * time.Second

// DefaultIdleThreshold is how long a session must be quiet (no writes)
// before the daemon considers it eligible for boundary extraction. 15
// minutes matches the design doc's chosen default.
const DefaultIdleThreshold = 15 * time.Minute

// PIDFile returns ~/.octo/memoryd.pid. The single-user PID file is
// sufficient for the v1 manual-start model — there's only ever one
// memoryd per user.
func PIDFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("memoryd: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, ".octo", "memoryd.pid"), nil
}

// WritePIDFile atomically writes pid to path. Creates parent dirs as
// needed. Uses the temp-file + rename pattern so a crash mid-write never
// leaves a partially-written PID a peer would read as garbage.
func WritePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadPIDFile reads the integer PID from path. Returns the canonical
// errors so callers can distinguish "no daemon ever started" from
// "stale/corrupt file" — both mean "no live daemon" but the user-facing
// status command can phrase them differently.
func ReadPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("memoryd: pid file %s is malformed: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("memoryd: pid file %s has invalid pid %d", path, pid)
	}
	return pid, nil
}

// RemovePIDFile deletes path. Idempotent — a missing file is not an
// error (cleanup ran twice, e.g. graceful shutdown + defer).
func RemovePIDFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsRunning probes whether a process with the given PID exists. On Unix
// this uses `kill -0` (signal 0 = just check, don't deliver). On Windows
// it always returns false — the daemon is unsupported there (see
// SupportedOnThisOS), so the answer is structurally "no daemon", and
// every code path that consults IsRunning gracefully degrades to the
// Phase 1 fallback.
//
// False positives are possible on Unix if the OS recycled the PID to an
// unrelated process — in that case `octo memoryd start` will detect the
// conflict when it tries to write its own PID, and `octo memoryd stop`
// will SIGTERM the unrelated process. Acceptable for v1; a real watchdog
// would verify the binary by reading /proc/<pid>/comm.
//
// Implemented per-OS via isRunning so build-tagged behavior is local to
// supported_*.go.
func IsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	return isRunning(pid)
}

// Status describes a daemon's lifecycle state for the CLI to render.
type Status struct {
	PID       int       // 0 when no daemon is running
	Running   bool      // true when the PID is alive
	PIDFile   string    // resolved path (always populated for messaging)
	StartedAt time.Time // file mtime of the PID file (best-effort)
}

// CheckStatus is the read-side of the lifecycle: look up the PID file,
// see if it exists, see if the PID is alive. Used by `octo memoryd
// status` AND by the chat side to decide whether to skip startup
// extraction (PR3).
func CheckStatus() (Status, error) {
	path, err := PIDFile()
	if err != nil {
		return Status{}, err
	}
	s := Status{PIDFile: path}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // no PID file → no daemon (not an error)
		}
		return s, err
	}
	s.StartedAt = fi.ModTime()
	pid, err := ReadPIDFile(path)
	if err != nil {
		return s, err
	}
	s.PID = pid
	s.Running = IsRunning(pid)
	return s, nil
}

// WorkFn is the per-tick callback the Daemon invokes. Implementations
// live outside this package (cmd/octo wires the real one against
// internal/agent + internal/memory) so memoryd stays free of agent /
// memory / tools dependencies — same dependency discipline as the M11
// scheduler's Executor interface.
//
// A WorkFn should be short-lived per call (one tick should complete in
// well under the daemon's tick interval). The daemon doesn't enforce a
// timeout; the ctx the work receives is the daemon's lifecycle ctx, so
// a SIGTERM / SIGINT propagates and the work should honor it promptly.
//
// Errors are logged via Daemon.Out but don't stop the loop — a
// transient extract failure shouldn't take down the daemon for the
// rest of the session.
type WorkFn func(ctx context.Context) error

// Daemon is the long-running worker. The lifecycle (PID file, signal
// handler, tick loop) lives here; the actual work — extracting memories
// from finished sessions, consolidating when due — comes in via Work.
type Daemon struct {
	// Tick is the work-loop period. Zero → DefaultTick.
	Tick time.Duration
	// IdleThreshold is how long a session must be quiet before its
	// memories are eligible for extraction. Zero → DefaultIdleThreshold.
	// Pass-through value — the daemon doesn't consume it directly, but
	// it's the natural place to keep the config so WorkFn can read it
	// via the surrounding closure if needed.
	IdleThreshold time.Duration
	// Out receives lifecycle + heartbeat lines (start, stop, tick errors)
	// when non-nil. nil silences the daemon (useful for tests).
	Out io.Writer
	// Work is the per-tick callback. If nil, the daemon emits a heartbeat
	// only — useful for the PR1-era smoke tests that just exercise the
	// lifecycle. cmd/octo wires the real extract + consolidate function
	// in production.
	Work WorkFn
}

// Run starts the work loop. Blocks until ctx is done (typically the CLI
// hooks a signal-driven context to SIGTERM / SIGINT). The PID file is
// the caller's responsibility — Run doesn't touch it so tests can
// exercise the loop without polluting the real ~/.octo/memoryd.pid.
//
// The first tick fires immediately so a freshly-started daemon catches
// up on any quiet-and-unextracted sessions without making the user wait
// a full Tick interval.
func (d *Daemon) Run(ctx context.Context) error {
	tick := d.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	d.logf("memoryd: started (tick=%s, idle_threshold=%s)\n", tick, d.idleThreshold())

	d.doTick(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logf("memoryd: stopping (%v)\n", ctx.Err())
			return ctx.Err()
		case <-t.C:
			d.doTick(ctx)
		}
	}
}

// doTick runs one cycle. When Work is nil, just emits a heartbeat (the
// pre-PR2 behavior, kept for the lifecycle smoke tests). When Work is
// set, dispatches to it and logs any non-nil error without aborting.
func (d *Daemon) doTick(ctx context.Context) {
	if d.Work == nil {
		d.logf("memoryd: tick @ %s\n", time.Now().UTC().Format(time.RFC3339))
		return
	}
	if err := d.Work(ctx); err != nil && ctx.Err() == nil {
		d.logf("memoryd: tick error: %v\n", err)
	}
}

func (d *Daemon) idleThreshold() time.Duration {
	if d.IdleThreshold > 0 {
		return d.IdleThreshold
	}
	return DefaultIdleThreshold
}

func (d *Daemon) logf(format string, args ...any) {
	if d.Out == nil {
		return
	}
	fmt.Fprintf(d.Out, format, args...)
}

// SupportedOnThisOS reports whether the current platform supports the
// daemon. Windows is unsupported in v1 (see package doc); on those
// platforms the CLI surface refuses with a clear notice.
var SupportedOnThisOS = supportedOnThisOS

// errPidFileExists is the sentinel ReserveStart returns when another
// daemon is already running. CLI maps it to a friendly message.
var errPidFileExists = errors.New("memoryd: another daemon is already running")

// IsAlreadyRunning reports whether err came from ReserveStart and means
// "PID file claims a live daemon".
func IsAlreadyRunning(err error) bool { return errors.Is(err, errPidFileExists) }

// ReserveStart atomically claims the PID file for the current process.
// If the file already exists AND its PID is alive, returns
// errPidFileExists (use IsAlreadyRunning to detect). Stale PID files
// (the recorded PID is dead) are overwritten — convenient for the
// common case where the daemon crashed without cleanup.
func ReserveStart() (path string, err error) {
	path, err = PIDFile()
	if err != nil {
		return "", err
	}
	if pid, err := ReadPIDFile(path); err == nil {
		if IsRunning(pid) {
			return "", fmt.Errorf("%w (pid %d)", errPidFileExists, pid)
		}
		// Stale → fall through and overwrite.
	}
	if err := WritePIDFile(path, os.Getpid()); err != nil {
		return "", err
	}
	return path, nil
}
