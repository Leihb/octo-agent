// Package tools provides built-in tool implementations for the octo agentic
// loop. Each tool implements agent.ToolExecutor and exposes a Definition()
// method that returns the agent.ToolDefinition the LLM sees.
package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// TerminalTimeout is the maximum time a single terminal command may run.
const TerminalTimeout = 30 * time.Second

// TerminalTool is an agent.ToolExecutor that runs shell commands through the
// system shell (`sh -c` on macOS/Linux, PowerShell on Windows; see
// shellCommand). Stdout and stderr are combined and returned as the tool
// result. Non-zero exit codes are reported as extra metadata in the result
// text rather than as a tool error, so the LLM can see the failure output and
// adapt.
//
// The LLM-facing tool name is "terminal" — calling it "bash" would imply a
// hard /bin/bash dependency, but the executor shells out via the platform
// shell (the model is told which one via the environment context).
//
// mgr, when non-nil, is the BackgroundManager used for background:true
// launches; nil falls back to the process-wide default manager. The field
// exists so tests can inject an isolated manager.
type TerminalTool struct{ mgr *BackgroundManager }

func (t TerminalTool) manager() *BackgroundManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultBg
}

// Definition returns the agent.ToolDefinition the LLM receives in the tools
// list. The JSON Schema describes a required "command" string and an optional
// "background" flag.
func (TerminalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "terminal",
		Description: "Run a shell command in the system shell (POSIX sh on macOS/Linux, PowerShell on Windows — see the Shell line in the environment context) and return stdout+stderr. Use for file operations, running programs, searching code, etc.\n\nIMPORTANT — background mode:\n- ALWAYS set background:true for commands that may take more than a few seconds (compiling, testing, installing dependencies, linting, building, watching, servers).\n- Common examples that MUST use background:true: `go test ./...`, `npm install`, `make build`, `gh pr checks --watch`, `docker compose up`, any server or watcher.\n- background:true returns immediately with a process id (no 30s timeout).\n- After launching a background command, DO NOT poll terminal_output. The system will automatically notify you when the process finishes, carrying its final output.\n- While the background command runs, you can continue with other tasks (read files, run other commands, etc.).\n- Use terminal_output only if you explicitly want to check progress mid-run, or use kill_shell to stop the process early.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
				},
				"background": map[string]any{
					"type":        "boolean",
					"description": "Run detached in the background (no 30s timeout, non-blocking). Returns a process id. The system will automatically notify you when the process completes — you do not need to poll terminal_output unless you want mid-run progress.",
				},
			},
			"required": []string{"command"},
		},
	}
}

// Execute runs the command and returns combined output. A non-zero exit code
// is appended to the output as `[exit: <error>]` rather than being surfaced
// as an error, giving the LLM visibility into what went wrong.
//
// Internally this delegates to ExecuteStream with a nil progress callback so
// both code paths share the same exec/scanner pipeline — only the streaming
// behavior changes.
func (t TerminalTool) Execute(ctx context.Context, name string, input map[string]any) (agent.ToolResult, error) {
	return t.ExecuteStream(ctx, name, input, nil)
}

// ExecuteStream runs the command and forwards each output line to progress
// as it arrives, returning the full aggregated stdout+stderr at the end.
// progress may be nil — in that case the behaviour is identical to Execute.
//
// stdout and stderr are merged into a single stream so the LLM sees them in
// chronological order (the same way they'd appear in an interactive terminal).
// Scanner buffer cap is 1 MiB per line — commands that emit a single 10MB-
// long line will get their final line truncated, but the more usual case of
// many short lines is unaffected.
func (t TerminalTool) ExecuteStream(
	ctx context.Context,
	_ string,
	input map[string]any,
	progress func(chunk string),
) (agent.ToolResult, error) {
	command, _ := input["command"].(string)
	if command == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal: command is required")
	}
	if err := guardCommand(command); err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	// Background launch: detach, no timeout, return the id immediately. The
	// guard above still applies, so dangerous commands are blocked either way.
	if bg, _ := input["background"].(bool); bg {
		id, err := t.manager().Start(command)
		if err != nil {
			return agent.ToolResult{Text: ""}, err
		}
		return agent.ToolResult{Text: fmt.Sprintf("Started background process %s.\n\nDO NOT poll terminal_output. The system will automatically notify you when this process finishes, carrying its full output. You can continue with other tasks while it runs.", id)}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, TerminalTimeout)
	defer cancel()

	cmd, err := shellCommand(ctx, command)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	// Merge stdout + stderr through a single pipe so the reader sees a
	// chronological stream. Doing `cmd.Stderr = cmd.Stdout` after StdoutPipe
	// doesn't work in all Go versions; the io.Pipe pattern is portable.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal: start: %w", err)
	}

	// Reader goroutine forwards each line to progress and accumulates the
	// full buffer for the eventual return value.
	var (
		out      strings.Builder
		readDone = make(chan struct{})
	)
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			out.WriteString(line)
			out.WriteByte('\n')
			if progress != nil {
				progress(line)
			}
		}
		// Drop scanner.Err here: the most common cause is a single
		// over-cap line, which we recover from by simply not forwarding
		// the rest. The Wait() error below is the canonical signal.
	}()

	// Kill the subprocess (and any children it spawned) when the context is
	// cancelled (e.g. user pressed Esc in the TUI). Without this, cmd.Wait()
	// blocks until the child exits on its own, so an interactive command like
	// `gh pr checks --watch` appears to ignore the interrupt.
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = killProcessGroup(cmd.Process)
		}
	}()

	waitErr := cmd.Wait()
	_ = pw.Close() // unblocks the scanner's Read by EOF
	<-readDone     // ensure goroutine has flushed before reading `out`

	body := strings.TrimRight(out.String(), "\n")
	// Tabs confuse the TUI's cursor-position math (bubbletea can't predict
	// where a tab stop lands). Replace them with spaces so inline rendering
	// stays aligned with the terminal's actual cursor.
	body = strings.ReplaceAll(body, "\t", "    ")
	if waitErr != nil {
		// Match the original Execute contract: non-zero exit is surfaced as
		// result text, not as a Go error, so the LLM can read and adapt.
		return agent.ToolResult{Text: body + "\n[exit: " + waitErr.Error() + "]"}, nil
	}
	return agent.ToolResult{Text: body}, nil
}

// TerminalOutputTool reads new output (and status) from a background process
// started by TerminalTool with background:true — the counterpart that makes
// detached commands useful. It can also kill the process.
type TerminalOutputTool struct{ mgr *BackgroundManager }

func (t TerminalOutputTool) manager() *BackgroundManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultBg
}

// Definition describes the required "id". Reading is non-destructive; to stop a
// process use the kill_shell tool.
func (TerminalOutputTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "terminal_output",
		Description: "Read output produced since the last check from a background process (the id returned by terminal with background:true), along with its status (running / exited). To terminate the process, use the kill_shell tool.\n\nIMPORTANT: You do NOT need to poll this tool. When a background process finishes, the system automatically sends you a notification with its final output. Only call terminal_output if you want to check progress while the process is still running.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The background process id (e.g. \"bg_1\").",
				},
			},
			"required": []string{"id"},
		},
	}
}

// Execute returns the new output plus a status line. Read-only — it never
// terminates the process (that's kill_shell).
func (t TerminalOutputTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	id, _ := input["id"].(string)
	if id == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_output: id is required")
	}
	out, status, found := t.manager().Read(id)
	if !found {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_output: no background process %q", id)
	}
	header := "[status: " + status + "]"
	if out == "" {
		if status == "running" {
			return agent.ToolResult{Text: header + "\n(no new output)\n\nSTOP POLLING. The system will automatically notify you when this background process finishes. Do NOT call terminal_output again unless you need to check progress mid-run."}, nil
		}
		return agent.ToolResult{Text: header + "\n(no new output)"}, nil
	}
	return agent.ToolResult{Text: header + "\n" + out}, nil
}

// KillShellTool terminates a background process started by TerminalTool with
// background:true and returns its final output — the counterpart to
// terminal_output, which only reads. Split out from terminal_output's old
// kill:true flag so "stop this process" is a first-class, obvious action.
type KillShellTool struct{ mgr *BackgroundManager }

func (t KillShellTool) manager() *BackgroundManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultBg
}

// Definition describes the required "id".
func (KillShellTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "kill_shell",
		Description: "Terminate a background process started by terminal with background:true (the id it returned), and return its final output. Use to stop a server, watcher, or other long-running command you no longer need. To read output without stopping it, use terminal_output.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The background process id to terminate (e.g. \"bg_1\").",
				},
			},
			"required": []string{"id"},
		},
	}
}

// Execute kills the process, then returns its final remaining output. An
// unknown id is an error (Kill reports it); an already-exited process is a
// no-op kill and still returns its last output.
func (t KillShellTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	id, _ := input["id"].(string)
	if id == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_shell: id is required")
	}
	mgr := t.manager()
	if !mgr.Kill(id) {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_shell: no background process %q", id)
	}
	// Give the process a moment to flush and the waiter to record exit.
	time.Sleep(50 * time.Millisecond)

	out, status, _ := mgr.Read(id) // found guaranteed: Kill succeeded
	header := "[killed] [status: " + status + "]"
	if out == "" {
		return agent.ToolResult{Text: header + "\n(no new output)"}, nil
	}
	return agent.ToolResult{Text: header + "\n" + out}, nil
}
