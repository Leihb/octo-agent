package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/server"
)

// fakeSpawner scripts a sequence of worker lifetimes for superviseLoop. Each
// entry is the exit code the fake worker reports; workers exit immediately
// unless holdForSignal is set, in which case they block until signalled.
type fakeSpawner struct {
	codes         []int
	spawned       int
	holdForSignal bool
	sigReceived   chan os.Signal
}

func (f *fakeSpawner) spawn() (func() int, func(os.Signal), error) {
	if f.spawned >= len(f.codes) {
		panic("fakeSpawner: spawned more workers than scripted")
	}
	code := f.codes[f.spawned]
	f.spawned++

	release := make(chan struct{})
	if !f.holdForSignal {
		close(release)
	}
	wait := func() int {
		<-release
		return code
	}
	sigFn := func(sig os.Signal) {
		if f.sigReceived != nil {
			f.sigReceived <- sig
		}
		select {
		case <-release:
		default:
			close(release)
		}
	}
	return wait, sigFn, nil
}

func TestSuperviseLoop_CleanExitNoRespawn(t *testing.T) {
	f := &fakeSpawner{codes: []int{0}}
	var errBuf bytes.Buffer

	code := superviseLoop(f.spawn, make(chan os.Signal), &errBuf)

	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if f.spawned != 1 {
		t.Errorf("spawned = %d, want 1", f.spawned)
	}
}

func TestSuperviseLoop_RestartCodeRespawns(t *testing.T) {
	f := &fakeSpawner{codes: []int{server.ExitRestart, server.ExitRestart, 0}}
	var errBuf bytes.Buffer

	code := superviseLoop(f.spawn, make(chan os.Signal), &errBuf)

	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if f.spawned != 3 {
		t.Errorf("spawned = %d, want 3 (two respawns then clean exit)", f.spawned)
	}
}

func TestSuperviseLoop_CrashCodePropagates(t *testing.T) {
	f := &fakeSpawner{codes: []int{server.ExitRestart, 7}}
	var errBuf bytes.Buffer

	code := superviseLoop(f.spawn, make(chan os.Signal), &errBuf)

	if code != 7 {
		t.Errorf("code = %d, want 7 (crash propagates, no respawn)", code)
	}
	if f.spawned != 2 {
		t.Errorf("spawned = %d, want 2", f.spawned)
	}
}

// TestSuperviseLoop_SignalStopsRespawn covers the Ctrl-C path: the parent
// forwards the signal to the worker and must NOT respawn even when the
// worker's exit code is ExitRestart (a restart drain interrupted mid-flight).
func TestSuperviseLoop_SignalStopsRespawn(t *testing.T) {
	f := &fakeSpawner{
		codes:         []int{server.ExitRestart},
		holdForSignal: true,
		sigReceived:   make(chan os.Signal, 1),
	}
	sigCh := make(chan os.Signal, 1)
	var errBuf bytes.Buffer

	done := make(chan int, 1)
	go func() { done <- superviseLoop(f.spawn, sigCh, &errBuf) }()

	sigCh <- syscall.SIGTERM

	select {
	case sig := <-f.sigReceived:
		if sig != syscall.SIGTERM {
			t.Errorf("forwarded signal = %v, want SIGTERM", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("signal was not forwarded to the worker")
	}

	select {
	case code := <-done:
		if code != server.ExitRestart {
			t.Errorf("code = %d, want %d (worker's code propagates)", code, server.ExitRestart)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("superviseLoop did not return after signalled worker exit")
	}
	if f.spawned != 1 {
		t.Errorf("spawned = %d, want 1 (no respawn after signal)", f.spawned)
	}
}

func TestSuperviseLoop_SpawnErrorReturns1(t *testing.T) {
	spawn := func() (func() int, func(os.Signal), error) {
		return nil, nil, os.ErrNotExist
	}
	var errBuf bytes.Buffer

	code := superviseLoop(spawn, make(chan os.Signal), &errBuf)

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if errBuf.Len() == 0 {
		t.Error("expected an error message on stderr")
	}
}

func TestShouldSupervise(t *testing.T) {
	cases := []struct {
		noSupervisor bool
		workerEnv    string
		want         bool
	}{
		{false, "", true},   // default: supervise
		{false, "1", false}, // spawned worker, or external supervisor
		{true, "", false},   // explicit opt-out
		{true, "1", false},
	}
	for _, c := range cases {
		if got := shouldSupervise(c.noSupervisor, c.workerEnv); got != c.want {
			t.Errorf("shouldSupervise(%v, %q) = %v, want %v", c.noSupervisor, c.workerEnv, got, c.want)
		}
	}
}

func TestServeExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{server.ErrRestartRequested, server.ExitRestart},
		{os.ErrClosed, 1},
	}
	for _, c := range cases {
		if got := serveExitCode(c.err); got != c.want {
			t.Errorf("serveExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

// TestSuperviseLoop_QueuedSignalSkipsRespawn: a signal that lands together
// with an ExitRestart exit must not spawn a doomed replacement worker.
func TestSuperviseLoop_QueuedSignalSkipsRespawn(t *testing.T) {
	f := &fakeSpawner{codes: []int{server.ExitRestart}}
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGINT
	var errBuf bytes.Buffer

	code := superviseLoop(f.spawn, sigCh, &errBuf)

	if code != server.ExitRestart {
		t.Errorf("code = %d, want %d", code, server.ExitRestart)
	}
	if f.spawned != 1 {
		t.Errorf("spawned = %d, want 1 (queued signal must suppress respawn)", f.spawned)
	}
}
