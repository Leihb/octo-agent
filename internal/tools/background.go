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

	mu       sync.Mutex
	buf      []byte // most recent <= maxBgOutputBytes of combined stdout+stderr
	produced int64  // total bytes ever written to the logical stream
	readOff  int64  // absolute offset already returned by Read
	done     bool
	exitErr  error

	// Anti-polling: time-window based. Within a 30-second window, 3 or more
	// empty reads on a running process trigger a block. This allows occasional
	// status checks on long-running services without penalising the model.
	firstEmptyPoll time.Time
	emptyPollCount int

	onLine func(string) // optional real-time callback for sync-mode streaming

	// stdin is the write end of the pipe attached to the process's stdin.
	// Set after Start for background processes that need interactive input.
	stdin io.WriteCloser

	// visible controls whether the process appears in ListRunning /
	// RunningBackground. Sync-started processes are hidden until they time
	// out and become true background tasks, so the TUI "background (N)"
	// panel doesn't flicker during a normal synchronous call.
	visible bool
}

func (p *bgProcess) append(b []byte) {
	p.mu.Lock()
	p.produced += int64(len(b))
	p.buf = append(p.buf, b...)
	if len(p.buf) > maxBgOutputBytes {
		p.buf = p.buf[len(p.buf)-maxBgOutputBytes:]
	}
	onLine := p.onLine
	p.mu.Unlock()
	if onLine != nil {
		onLine(string(b))
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

	// Anti-polling: time-window based. Within a 30-second window, 3 or more
	// empty reads on a running process trigger a block. This is lenient enough
	// for occasional status checks on long-running services while still stopping
	// tight polling loops on one-shot tasks.
	const pollWindow = 30 * time.Second
	const maxEmptyPolls = 3
	blocked := false
	if !p.done && len(out) == 0 {
		now := time.Now()
		if p.emptyPollCount == 0 || now.Sub(p.firstEmptyPoll) > pollWindow {
			// Start a new window.
			p.firstEmptyPoll = now
			p.emptyPollCount = 1
		} else {
			p.emptyPollCount++
			if p.emptyPollCount >= maxEmptyPolls {
				blocked = true
			}
		}
	} else {
		p.emptyPollCount = 0
		p.firstEmptyPoll = time.Time{}
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

// StartOption is a functional option for BackgroundManager.Start.
type StartOption func(*bgProcess)

// WithOnLine registers a callback that receives each line of output as it is
// produced. Used by the synchronous path to forward output to the progress
// callback in real time.
func WithOnLine(fn func(string)) StartOption {
	return func(p *bgProcess) { p.onLine = fn }
}

// WithVisible sets the process visibility in ListRunning. Sync-started
// processes start hidden (visible=false) and are promoted to visible=true
// when they time out.
func WithVisible(v bool) StartOption {
	return func(p *bgProcess) { p.visible = v }
}

// Start launches command detached (via `sh -c`), with no timeout, and returns
// its background id. Output streams into a capped buffer; the process is killed
// if its context is cancelled (Kill / KillAll).
func (m *BackgroundManager) Start(command string, opts ...StartOption) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := shellCommand(ctx, command)
	if err != nil {
		cancel()
		return "", err
	}

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	// Stdin pipe: allows the agent to send input to interactive background
	// processes (REPLs, configuration wizards, etc.) via terminal_input.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		cancel()
		_ = pw.Close()
		return "", fmt.Errorf("terminal: create stdin pipe: %w", err)
	}
	cmd.Stdin = stdinR

	if err := cmd.Start(); err != nil {
		cancel()
		_ = pw.Close()
		_ = stdinW.Close()
		return "", fmt.Errorf("terminal: start background: %w", err)
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bg_%d", m.seq)
	p := &bgProcess{id: id, command: command, cancel: cancel, proc: cmd.Process, start: time.Now(), visible: true, stdin: stdinW}
	for _, opt := range opts {
		opt(p)
	}
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
		_ = stdinW.Close() // close stdin so any blocked reads on the process side get EOF
		<-readerDone       // ensures reader flushed all pipe data
		p.finish(err)

		m.mu.Lock()
		hook := m.onExit
		m.mu.Unlock()
		if hook != nil {
			p.mu.Lock()
			visible := p.visible
			p.mu.Unlock()
			// Only notify for processes that are visible as background tasks.
			// Sync-started processes start invisible (visible=false); they are
			// promoted to visible=true when they time out. If a sync process
			// finishes before the timeout, it was never a background task and
			// its result was already returned synchronously — no notification.
			if visible {
				out, status, _ := p.readNew()
				hook(BgExit{ID: p.id, Command: p.command, Status: status, NewOutput: out})
			}
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

// ListRunning returns the visible processes that haven't exited yet, oldest first.
// Processes started invisibly (e.g. sync mode) are excluded until promoted.
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
		visible := p.visible
		p.mu.Unlock()
		if !done && visible {
			out = append(out, BgInfo{ID: p.id, Command: p.command, Start: p.start})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Promote makes a background process visible in ListRunning. Used when a
// sync-started process times out and becomes a true background task.
func (m *BackgroundManager) Promote(id string) bool {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return false
	}
	p.mu.Lock()
	p.visible = true
	p.mu.Unlock()
	return true
}

// Kill terminates the process for id with SIGKILL. Returns false when id is unknown.
func (m *BackgroundManager) Kill(id string) bool {
	return m.KillWithSignal(id, "SIGKILL")
}

// KillWithSignal terminates the process for id with the named signal.
// Supported signals: SIGKILL, SIGTERM, SIGINT. Returns false when id is unknown.
func (m *BackgroundManager) KillWithSignal(id string, sigName string) bool {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return false
	}
	terminate(p, sigName)
	return true
}

// terminate is the single chokepoint for taking a background process down — the
// one place that knows how to do it correctly, so Kill / KillAll / Remove can't
// drift apart (every past orphan/leak bug came from one of them forgetting a
// step). Two rules live here and nowhere else:
//   - Always signal the whole process GROUP (POSIX kill(-pid); Windows
//     taskkill /T), not just the direct child — otherwise the shell wrapper
//     (sh -c / pwsh) dies and the real process it spawned is orphaned.
//   - Only cancel the context on SIGKILL (exec.CommandContext fires its own
//     SIGKILL as a backstop). For SIGTERM/SIGINT we must NOT cancel, or exec
//     would race in with an automatic SIGKILL and defeat the graceful stop.
func terminate(p *bgProcess, sigName string) {
	if p == nil {
		return
	}
	if sigName == "SIGKILL" {
		p.cancel()
	}
	if p.proc != nil {
		_ = killProcessGroup(p.proc, sigName)
	}
}

// WriteStdin sends text to the stdin of a running background process.
// Returns an error if the process is unknown or has already exited.
func (m *BackgroundManager) WriteStdin(id string, input string) error {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no background process %q", id)
	}
	p.mu.Lock()
	done := p.done
	stdin := p.stdin
	p.mu.Unlock()
	if done {
		return fmt.Errorf("background process %q has already exited", id)
	}
	if stdin == nil {
		return fmt.Errorf("background process %q does not accept input", id)
	}
	_, err := stdin.Write([]byte(input))
	return err
}

// Command returns the original command string for a background process id,
// or ("", false) if the id is unknown or has been removed.
func (m *BackgroundManager) Command(id string) (string, bool) {
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		return "", false
	}
	return p.command, true
}

// Remove drops a process from the tracking map, releasing its retained output
// buffer. Used by the synchronous terminal path to reap a hidden command once
// it has exited and its output has been returned to the caller — otherwise
// every synchronous command would leak a bgProcess (up to maxBgOutputBytes
// each) for the life of the session. Visible background tasks are NOT reaped
// this way: their output stays readable via terminal_output after they exit.
func (m *BackgroundManager) Remove(id string) {
	m.mu.Lock()
	p, ok := m.procs[id]
	delete(m.procs, id)
	m.mu.Unlock()
	// Belt-and-suspenders: if Remove is ever called on a still-running id, take
	// it (and its process group) down so it can't outlive its map entry. A no-op
	// on an already-exited process — the common reap path — where the group is
	// already gone.
	if ok {
		terminate(p, "SIGKILL")
	}
}

// KillAll terminates every tracked process. Called on session shutdown so no
// background command is orphaned.
func (m *BackgroundManager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		terminate(p, "SIGKILL")
	}
}

// defaultBg is the process-wide manager used by the built-in tools when no
// manager is injected. A single-process CLI shares one set of background
// processes across the session.
var defaultBg = NewBackgroundManager()

// KillAllBackground terminates every tracked background process — the default
// manager AND every per-session manager. Wire it into session/REPL/daemon
// shutdown to avoid orphans regardless of which session launched a process.
func KillAllBackground() {
	for _, m := range allBackgroundManagers() {
		m.KillAll()
	}
}

// SetBackgroundOnExit registers the completion hook on the default manager (the
// one the built-in terminal tool uses). The REPL wires this to push a
// "background finished" notice into the conversation + UI. Pass nil to clear.
func SetBackgroundOnExit(fn func(BgExit)) { defaultBg.SetOnExit(fn) }

// RunningBackground lists the still-running processes on the default manager,
// for the TUI's live "background (N running)" panel.
func RunningBackground() []BgInfo { return defaultBg.ListRunning() }

// BgCommand returns the original command string for a background process id
// on the default manager, or ("", false) if unknown.
func BgCommand(id string) (string, bool) { return defaultBg.Command(id) }
