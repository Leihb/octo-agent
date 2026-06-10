package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/Leihb/octo-agent/internal/server"
)

// serveWorkerEnv marks a process as the serve worker. The supervisor sets it
// on the child it spawns; setting it externally (systemd, launchd, a shell)
// skips the built-in supervisor so an external one can own the respawn via
// exit code server.ExitRestart.
const serveWorkerEnv = "OCTO_SERVE_WORKER"

// superviseLoop drives the supervisor: spawn a worker, wait, and dispatch on
// its exit code — server.ExitRestart respawns (picking up a replaced binary),
// anything else propagates. A signal received while a worker runs is
// forwarded and permanently stops respawning: the user asked the whole serve
// to quit, and on POSIX the worker usually got the same terminal signal
// directly anyway.
//
// spawn starts one worker and returns a wait function (blocks until exit,
// returns the exit code) and a signal-forwarding function. Split out from
// the exec.Cmd mechanics so the loop is testable with fakes.
func superviseLoop(spawn func() (func() int, func(os.Signal), error), sigCh <-chan os.Signal, stderr io.Writer) int {
	quitting := false
	for {
		wait, forward, err := spawn()
		if err != nil {
			fmt.Fprintf(stderr, "octo serve: supervisor: %v\n", err)
			return 1
		}

		done := make(chan int, 1)
		go func() { done <- wait() }()

		var code int
	waitLoop:
		for {
			select {
			case sig := <-sigCh:
				quitting = true
				forward(sig)
			case code = <-done:
				break waitLoop
			}
		}

		if code == server.ExitRestart && !quitting {
			fmt.Fprintln(stderr, "octo serve: restarting worker")
			continue
		}
		return code
	}
}

// spawnServeWorker returns the production spawn function for superviseLoop:
// re-exec the current binary with `serve <args>` and the worker marker env.
// The binary path is resolved once at supervisor start and re-opened by the
// OS on every spawn, so a binary replaced on disk takes effect on the next
// restart without the supervisor itself changing.
func spawnServeWorker(args []string, stdout, stderr io.Writer) func() (func() int, func(os.Signal), error) {
	exe, exeErr := os.Executable()
	return func() (func() int, func(os.Signal), error) {
		if exeErr != nil {
			return nil, nil, exeErr
		}
		cmd := exec.Command(exe, append([]string{"serve"}, args...)...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Env = append(os.Environ(), serveWorkerEnv+"=1")
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}

		wait := func() int {
			err := cmd.Wait()
			if err == nil {
				return 0
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			// Wait failed without an exit code (I/O error, killed before
			// start completed); treat as a crash.
			return 1
		}
		forward := func(sig os.Signal) {
			if cmd.Process == nil {
				return
			}
			// Windows has no Unix signals; the graceful path there is the
			// console Ctrl-C event the worker already received. Forwarding
			// means terminating the process.
			if runtime.GOOS == "windows" {
				_ = cmd.Process.Kill()
				return
			}
			_ = cmd.Process.Signal(sig)
		}
		return wait, forward, nil
	}
}
