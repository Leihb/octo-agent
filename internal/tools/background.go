package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// maxBgOutputBytes caps how much output is retained per background process.
// Beyond this we keep only the most recent bytes (a long-running server's tail
// is what matters); Read reports when earlier output was dropped.
const maxBgOutputBytes = 1 << 20 // 1 MiB

// bgProcess tracks one detached command launched via BackgroundManager.
type bgProcess struct {
	id      string
	command string
	cancel  context.CancelFunc
	proc    *os.Process // set after Start; used for hard-kill
	start   time.Time

	mu        sync.Mutex
	buf       []byte // most recent <= maxBgOutputBytes of combined stdout+stderr
	produced  int64  // total bytes ever written to the logical stream
	readOff   int64  // absolute offset already returned by Read
	done      bool
	exitErr   error
	pollCount int // consecutive empty reads while running
}

func (p *bgProcess) append(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.produced += int64(len(b))
	p.buf = append(p.buf, b...)
	if len(p.buf) > maxBgOutputBytes {
		p.buf = p.buf[len(p.buf)-maxBgOutputBytes:]
	}
}

func (p *bgProcess) finish(err error) {
	p.mu.Lock()
	p.done = true
	p.exitErr = err
	p.mu.Unlock()
}

// readNew returns output produced since the last Read and the current status,
// advancing the read cursor. When retained output was dropped (buffer cap), the
// returned text is prefixed with a truncation marker.
func (p *bgProcess) readNew() (string, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	bufStart := p.produced - int64(len(p.buf)) // absolute offset of buf[0]
	var out []byte
	if p.readOff < bufStart {
		out = append(out, "[... earlier output truncated ...]\n"...)
		p.readOff = bufStart
	}
	out = append(out, p.buf[p.readOff-bufStart:]...)
	p.readOff = p.produced

	status := "running"
	if p.done {
		if p.exitErr != nil {
			status = "exited: " + p.exitErr.Error()
		} else {
			status = "exited: 0"
		}
	}

	// Anti-polling: if running and no new output, increment poll count.
	// After 2 consecutive empty polls, report that polling is blocked.
	blocked := false
	if !p.done && len(out) == 0 {
		p.pollCount++
		if p.pollCount >= 2 {
			blocked = true
		}
	} else {
		p.pollCount = 0
	}
	return string(out), status, blocked
}

// BgExit is delivered to a BackgroundManager's onExit hook when a detached
// process finishes. NewOutput is whatever hadn't been consumed by Read yet at
// exit (so a push notification and a later terminal_output poll don't
// double-report the same bytes — the readNew cursor advances either way).
type BgExit struct {
	ID        string
	Command   string
	Status    string // "exited: 0" / "exited: <err>" — same shape readNew returns
	NewOutput string
}

// BackgroundManager owns the set of detached background processes for a
// session. Methods are safe for concurrent use.
type BackgroundManager struct {
	mu     sync.Mutex
	procs  map[string]*bgProcess
	seq    int
	onExit func(BgExit) // optional; fired from the waiter goroutine on completion
}

// NewBackgroundManager returns an empty manager.
func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{procs: map[string]*bgProcess{}}
}

// SetOnExit registers a completion hook fired once per process when it exits,
// carrying its final status and any output not yet read. Pass nil to clear.
// The hook runs on the process's waiter goroutine (not under the manager lock),
// so it may call back into the manager (e.g. Read) without deadlocking. The CLI
// uses it to push a "background finished" notice into the conversation + UI;
// the default (nil) keeps the original poll-only behaviour.
func (m *BackgroundManager) SetOnExit(fn func(BgExit)) {
	m.mu.Lock()
	m.onExit = fn
	m.mu.Unlock()
}

// Start launches command detached (via `sh -c`), with no timeout, and returns
// its background id. Output streams into a capped buffer; the process is killed
// if its context is cancelled (Kill / KillAll).
func (m *BackgroundManager) Start(command string) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := shellCommand(ctx, command)
	if err != nil {
		cancel()
		return "", err
	}

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		cancel()
		_ = pw.Close()
		return "", fmt.Errorf("terminal: start background: %w", err)
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bg_%d", m.seq)
	p := &bgProcess{id: id, command: command, cancel: cancel, proc: cmd.Process, start: time.Now()}
	m.procs[id] = p
	m.mu.Unlock()

	// readerDone is closed by the reader goroutine when it finishes
	// draining the pipe. The waiter MUST wait on this before firing
	// onExit — otherwise a fast-exiting process triggers the hook before
	// the reader has flushed remaining pipe data, losing output.
	readerDone := make(chan struct{})

	// Reader: forward combined output into the capped buffer.
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			// Tabs break the TUI's cursor-position math; replace with spaces.
			line = bytes.ReplaceAll(line, []byte{'\t'}, []byte("    "))
			p.append(append(line, '\n')) // append copies
		}
	}()
	// Waiter: wait for the process, close pipe so the reader sees EOF,
	// then wait for the reader to drain before firing onExit.
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		<-readerDone // ensures reader flushed all pipe data
		p.finish(err)

		m.mu.Lock()
		hook := m.onExit
		m.mu.Unlock()
		if hook != nil {
			out, status, _ := p.readNew()
			hook(BgExit{ID: p.id, Command: p.command, Status: status, NewOutput: out})
		}
	}()

	return id, nil
}

// Read returns output produced since the last call for id, plus a status
// string. found is false when id is unknown. blocked is true when the caller
// has polled too many times without new output while the process is still
// running — this forces the LLM to stop polling.
func (m *BackgroundManager) Read(id string) (output, status string, found bool, blocked bool) {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return "", "", false, false
	}
	out, st, blk := p.readNew()
	return out, st, true, blk
}

// BgInfo is a snapshot of a still-running background process, for a live
// "background (N running)" panel in the TUI.
type BgInfo struct {
	ID      string
	Command string
	Start   time.Time
}

// ListRunning returns the processes that haven't exited yet, oldest first.
func (m *BackgroundManager) ListRunning() []BgInfo {
	m.mu.Lock()
	procs := make([]*bgProcess, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.Unlock()

	var out []BgInfo
	for _, p := range procs {
		p.mu.Lock()
		done := p.done
		p.mu.Unlock()
		if !done {
			out = append(out, BgInfo{ID: p.id, Command: p.command, Start: p.start})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Kill terminates the process for id. Returns false when id is unknown.
func (m *BackgroundManager) Kill(id string) bool {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return false
	}
	p.cancel()
	if p.proc != nil {
		_ = killProcessGroup(p.proc)
	}
	return true
}

// KillAll terminates every tracked process. Called on session shutdown so no
// background command is orphaned.
func (m *BackgroundManager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		p.cancel()
	}
}

// defaultBg is the process-wide manager used by the built-in tools when no
// manager is injected. A single-process CLI shares one set of background
// processes across the session.
var defaultBg = NewBackgroundManager()

// KillAllBackground terminates all background processes started via the default
// manager. Wire it into session/REPL shutdown to avoid orphans.
func KillAllBackground() { defaultBg.KillAll() }

// SetBackgroundOnExit registers the completion hook on the default manager (the
// one the built-in terminal tool uses). The REPL wires this to push a
// "background finished" notice into the conversation + UI. Pass nil to clear.
func SetBackgroundOnExit(fn func(BgExit)) { defaultBg.SetOnExit(fn) }

// RunningBackground lists the still-running processes on the default manager,
// for the TUI's live "background (N running)" panel.
func RunningBackground() []BgInfo { return defaultBg.ListRunning() }
