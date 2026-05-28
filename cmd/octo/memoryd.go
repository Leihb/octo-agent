package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Leihb/octo-agent/internal/memoryd"
)

// runMemoryd handles `octo memoryd <subcommand>`. The C9 Phase 2 daemon
// (see internal/memoryd) takes over the async extract + consolidate
// passes the chat path runs at startup; with the daemon alive, chat
// startup becomes effectively instant and memory work happens in the
// background. v1 is manual-start only (no launchd / systemd integration);
// the user runs `octo memoryd start` in a spare terminal or backgrounds
// it via shell.
//
// PR1 (this file) ships start / stop / status. PR2 will plug the actual
// work into the daemon. PR3 will teach `octo chat` to skip the Phase 1
// startup path when the daemon is alive.
func runMemoryd(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printMemorydUsage(stdout)
		return 2
	}
	switch args[0] {
	case "start":
		return runMemorydStart(stdout, stderr)
	case "stop":
		return runMemorydStop(stdout, stderr)
	case "status":
		return runMemorydStatus(stdout, stderr)
	case "help", "--help", "-h":
		printMemorydUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "octo memoryd: unknown subcommand %q\n", args[0])
		printMemorydUsage(stderr)
		return 2
	}
}

func printMemorydUsage(w io.Writer) {
	fmt.Fprintln(w, "octo memoryd — C9 Phase 2 cross-session memory daemon")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: octo memoryd <subcommand>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  start    Run the daemon in the foreground (Ctrl-C / SIGTERM to stop).")
	fmt.Fprintln(w, "  stop     SIGTERM the running daemon (reads ~/.octo/memoryd.pid).")
	fmt.Fprintln(w, "  status   Report whether a daemon is running and where its PID file is.")
}

// runMemorydStart writes the PID file, hooks signal handlers, and runs
// the daemon loop in the foreground. Returns 0 on graceful shutdown, 1
// on configuration / setup errors.
func runMemorydStart(stdout, stderr io.Writer) int {
	if !memoryd.SupportedOnThisOS() {
		fmt.Fprintln(stderr, "octo memoryd: not supported on this OS (Windows falls back to Phase 1 startup memory).")
		return 1
	}

	// Build the work function BEFORE claiming the PID file so a config
	// error (missing API key) fails fast and doesn't leave a stale
	// "started then exited" pidfile to confuse the next start.
	idleThreshold := memoryd.DefaultIdleThreshold
	work, err := buildMemorydWork(stderr, idleThreshold)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	pidPath, err := memoryd.ReserveStart()
	if err != nil {
		if memoryd.IsAlreadyRunning(err) {
			fmt.Fprintf(stderr, "octo memoryd: %v\n", err)
			fmt.Fprintln(stderr, "If you're sure no daemon is alive, remove ~/.octo/memoryd.pid manually.")
			return 1
		}
		fmt.Fprintf(stderr, "octo memoryd: %v\n", err)
		return 1
	}
	// Remove the PID file on exit so the next `start` doesn't refuse.
	defer func() { _ = memoryd.RemovePIDFile(pidPath) }()

	fmt.Fprintf(stdout, "octo memoryd: started (pid %d, pidfile %s)\n", os.Getpid(), pidPath)
	fmt.Fprintln(stdout, "Use `octo memoryd stop` from another terminal, or send SIGTERM/SIGINT.")

	ctx, cancel := signalContext(context.Background())
	defer cancel()

	d := &memoryd.Daemon{Out: stdout, IdleThreshold: idleThreshold, Work: work}
	err = d.Run(ctx)
	// ctx.Canceled is the graceful path (SIGTERM/SIGINT delivered →
	// signalContext cancelled the ctx → Run returned ctx.Err()).
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "octo memoryd: %v\n", err)
		return 1
	}
	return 0
}

// runMemorydStop reads the PID file and sends SIGTERM to the recorded
// PID. Doesn't wait for the process to exit — the daemon's signal
// handler is responsible for finishing the in-flight tick and cleaning
// up the PID file. Returns 0 if signal delivery succeeded (or the
// daemon was already gone), 1 otherwise.
func runMemorydStop(stdout, stderr io.Writer) int {
	if !memoryd.SupportedOnThisOS() {
		fmt.Fprintln(stderr, "octo memoryd: not supported on this OS.")
		return 1
	}

	status, err := memoryd.CheckStatus()
	if err != nil {
		fmt.Fprintf(stderr, "octo memoryd: %v\n", err)
		return 1
	}
	if status.PID == 0 {
		fmt.Fprintln(stdout, "octo memoryd: no daemon running (no PID file).")
		return 0
	}
	if !status.Running {
		fmt.Fprintf(stdout, "octo memoryd: PID %d is not alive; clearing stale pidfile.\n", status.PID)
		_ = memoryd.RemovePIDFile(status.PIDFile)
		return 0
	}
	proc, err := os.FindProcess(status.PID)
	if err != nil {
		fmt.Fprintf(stderr, "octo memoryd: find pid %d: %v\n", status.PID, err)
		return 1
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(stderr, "octo memoryd: SIGTERM pid %d: %v\n", status.PID, err)
		return 1
	}
	fmt.Fprintf(stdout, "octo memoryd: sent SIGTERM to pid %d.\n", status.PID)
	return 0
}

// runMemorydStatus prints whether a daemon is currently running. Plays
// the same role as `systemctl status` — informational, never exits with
// an error just because no daemon is up.
func runMemorydStatus(stdout, stderr io.Writer) int {
	status, err := memoryd.CheckStatus()
	if err != nil {
		fmt.Fprintf(stderr, "octo memoryd: %v\n", err)
		return 1
	}
	switch {
	case status.PID == 0:
		fmt.Fprintln(stdout, "octo memoryd: not running.")
	case !status.Running:
		fmt.Fprintf(stdout, "octo memoryd: stale (pidfile %s claims pid %d, not alive).\n", status.PIDFile, status.PID)
	default:
		uptime := time.Since(status.StartedAt).Round(time.Second)
		fmt.Fprintf(stdout, "octo memoryd: running (pid %d, uptime %s, pidfile %s).\n",
			status.PID, uptime, status.PIDFile)
	}
	return 0
}

// signalContext returns a context that cancels on SIGTERM or SIGINT, so
// the daemon's Run loop unwinds gracefully when the user runs
// `octo memoryd stop` (which sends SIGTERM) or Ctrl-C's the foreground
// process.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}
