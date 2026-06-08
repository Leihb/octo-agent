// Package tools provides built-in tool implementations for the octo agentic
// loop. Each tool implements agent.ToolExecutor and exposes a Definition()
// method that returns the agent.ToolDefinition the LLM sees.
package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// TerminalTimeout is the maximum time a single terminal command may run
// synchronously before it is automatically promoted to a background process.
var TerminalTimeout = 120 * time.Second

// BgPollNotice is the model-facing instruction appended to a background-launch
// tool result. It steers the model away from polling terminal_output and
// carries no information for the human, so the TUI strips it from result cards
// (see renderToolCard) rather than printing it to the scrollback.
const BgPollNotice = "DO NOT poll terminal_output. The system will automatically notify you when this process finishes, carrying its final output. While it runs, you may continue with other independent tasks. If you have no other task to do, report the launch to the user and stop — do not spin in a polling loop."

// ServiceModeNotice is the model-facing instruction appended to a background
// launch of a long-running service (servers, watchers, etc.). It tells the
// model to verify the service externally (curl, pgrep) rather than polling
// terminal_output.
const ServiceModeNotice = "After launching a long-running service, verify it with an external check (e.g., `curl http://localhost:PORT` or `pgrep`) rather than polling terminal_output. terminal_output is for inspecting startup logs or diagnosing issues — do not call it in a tight loop."

// TerminalOutputStopPolling is the model-facing instruction appended to a
// terminal_output result when the process is still running with no new output.
// Like BgPollNotice, it is noise for the human so the TUI strips it from cards.
const TerminalOutputStopPolling = "STOP POLLING. The system will automatically notify you when this background process finishes. Do NOT call terminal_output again unless you need to check progress mid-run."

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
// mgr, when non-nil, is the BackgroundManager used for run_in_background
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
// "run_in_background" flag.
func (TerminalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "terminal",
		Description: "Run a shell command in the system shell (POSIX sh on macOS/Linux, PowerShell on Windows — see the Shell line in the environment context) and return stdout+stderr. Use for file operations, running programs, etc. Prefer dedicated tools (read_file, write_file, edit_file, glob, grep) over raw shell commands when they exist.\n\nChoosing sync vs background:\n- Default (no run_in_background): runs synchronously with a 120s timeout. Use for fast commands whose output you need immediately (e.g. `ls`, `git status`, `grep`, short scripts).\n- run_in_background:true — ONE-SHOT tasks (compiling, testing, installing, building, linting, CI checks): detaches immediately, returns a process id. The system automatically notifies you on completion. DO NOT poll terminal_output.\n- run_in_background:true — LONG-RUNNING services (servers, watchers, docker compose up): detaches immediately, returns a process id. After launch, verify the service with an external check (e.g., `curl http://localhost:PORT`, `pgrep`) rather than polling terminal_output. Use terminal_output only to inspect startup logs or diagnose issues — do not call it in a tight loop.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
				},
				"run_in_background": map[string]any{
					"type":        "boolean",
					"description": "Run detached in the background (no 120s timeout, non-blocking). Returns a process id. Use for one-shot tasks that take more than a few seconds (compiling, testing, installing, building, CI checks) or for long-running services (servers, watchers). For one-shot tasks the system auto-notifies on completion. For long-running services, verify with an external check (e.g., curl, pgrep) rather than polling terminal_output.",
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
//
// Timeout promotion: if the command exceeds TerminalTimeout (120 s) the
// original process continues running in the background (no restart). The
// caller receives the output produced so far plus a background id and a
// clear instruction to wait for the completion notification.
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
	if bg, _ := input["run_in_background"].(bool); bg {
		id, err := t.manager().Start(command)
		if err != nil {
			return agent.ToolResult{Text: ""}, err
		}
		return agent.ToolResult{Text: fmt.Sprintf("Started background process %s.\n\n%s", id, BgPollNotice)}, nil
	}

	// Synchronous path: start as a background process so that if we hit the
	// timeout the original process simply keeps running — no kill, no restart.
	// We attach an onLine callback to collect output and stream to progress in
	// real time. The polling loop only checks status (exited/running).
	//
	// The collector is capped at maxBgOutputBytes: a command that floods stdout
	// faster than the 120s timeout (e.g. a runaway `yes`) would otherwise grow an
	// unbounded buffer and OOM the agent. Past the cap we keep the most recent
	// bytes and flag that earlier output was dropped — the same tail-retention
	// policy bgProcess.append uses for the background buffer.
	var (
		outMu   sync.Mutex
		out     []byte
		dropped bool
	)
	onLine := func(line string) {
		outMu.Lock()
		out = append(out, line...)
		if len(out) > maxBgOutputBytes {
			out = out[len(out)-maxBgOutputBytes:]
			dropped = true
		}
		outMu.Unlock()
		if progress != nil {
			progress(strings.TrimRight(line, "\n"))
		}
	}

	id, err := t.manager().Start(command, WithOnLine(onLine), WithVisible(false))
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	// snapshot returns the collected output so far, tab-expanded and with the
	// truncation marker prepended when the cap dropped earlier bytes.
	snapshot := func() string {
		outMu.Lock()
		body := strings.TrimRight(string(out), "\n")
		d := dropped
		outMu.Unlock()
		body = strings.ReplaceAll(body, "\t", "    ")
		if d {
			body = "[... earlier output truncated ...]\n" + body
		}
		return body
	}

	// Poll until the process exits or the timeout fires.
	timer := time.NewTimer(TerminalTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			// Timeout: promote the hidden process to visible so it shows up
			// in the TUI "background (N running)" panel, then return. NOT
			// reaped — it's now a real background task and its output must stay
			// readable via terminal_output.
			t.manager().Promote(id)
			body := MaybeSpillOutput(id, snapshot())
			return agent.ToolResult{Text: fmt.Sprintf("%s\n\n[timeout: command exceeded %s and continues as background process %s]\n\n%s", body, TerminalTimeout, id, BgPollNotice)}, nil
		case <-ctx.Done():
			// User cancelled (Esc / Ctrl-C): kill the hidden process and reap
			// it — the output is returned here and now, nothing will poll it.
			t.manager().Kill(id)
			body := snapshot()
			t.manager().Remove(id)
			return agent.ToolResult{Text: body + "\n[exit: signal: killed]"}, nil
		default:
		}

		_, status, _, _ := t.manager().Read(id)
		if strings.HasPrefix(status, "exited") {
			body := MaybeSpillOutput(id, snapshot())
			// Reap the hidden process: its output has been captured and
			// returned, so the bgProcess (and its retained buffer) can go.
			t.manager().Remove(id)
			if status != "exited: 0" {
				return agent.ToolResult{Text: body + "\n[exit: " + strings.TrimPrefix(status, "exited: ") + "]"}, nil
			}
			return agent.ToolResult{Text: body}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TerminalOutputTool reads new output (and status) from a background process
// started by TerminalTool with run_in_background:true — the counterpart that makes
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
		Description: "Read output produced since the last check from a background process (the id returned by terminal with run_in_background:true), along with its status (running / exited). To terminate the process, use the kill_shell tool.\n\nFor ONE-SHOT tasks (compiles, tests, builds): you do NOT need to poll this tool. The system automatically notifies you when the process finishes.\n\nFor LONG-RUNNING services (servers, watchers): you may call this tool occasionally to inspect startup logs or diagnose issues, but do not call it in a tight loop. Prefer verifying the service with an external check (e.g., curl, pgrep) instead.",
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
	out, status, found, blocked := t.manager().Read(id)
	if !found {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_output: no background process %q", id)
	}
	if blocked {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_output: polling blocked for %s — the process is still running with no new output. Wait for the automatic completion notification instead.", id)
	}
	header := "[status: " + status + "]"
	if out == "" {
		if status == "running" {
			return agent.ToolResult{Text: header + "\n(no new output)\n\n" + TerminalOutputStopPolling}, nil
		}
		return agent.ToolResult{Text: header + "\n(no new output)"}, nil
	}
	return agent.ToolResult{Text: header + "\n" + MaybeSpillOutput(id, out)}, nil
}

// TerminalInputTool sends text to the stdin of a running background process
// started by TerminalTool with run_in_background:true. Use to interact with
// long-running interactive applications (REPLs, configuration wizards, servers
// that accept commands via stdin).
type TerminalInputTool struct{ mgr *BackgroundManager }

func (t TerminalInputTool) manager() *BackgroundManager {
	if t.mgr != nil {
		return t.mgr
	}
	return defaultBg
}

// Definition describes the required "id" and "input".
func (TerminalInputTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "terminal_input",
		Description: "Send text input to the stdin of a running background process started by terminal with run_in_background:true. Use to interact with long-running interactive applications (e.g., REPLs, configuration wizards, servers that accept commands via stdin). The input is written verbatim — include a trailing newline (\\n) if the process expects line-based input.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The background process id (e.g. \"bg_1\").",
				},
				"input": map[string]any{
					"type":        "string",
					"description": "The text to send to the process's stdin. Include a trailing \\n if the process reads line-by-line.",
				},
			},
			"required": []string{"id", "input"},
		},
	}
}

// Execute writes input to the process's stdin. Unknown or exited id is an error.
func (t TerminalInputTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	id, _ := input["id"].(string)
	if id == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_input: id is required")
	}
	text, _ := input["input"].(string)
	if text == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_input: input is required")
	}
	if err := t.manager().WriteStdin(id, text); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("terminal_input: %w", err)
	}
	return agent.ToolResult{Text: fmt.Sprintf("Sent to %s.", id)}, nil
}

// KillShellTool terminates a background process started by TerminalTool with
// run_in_background:true and returns its final output — the counterpart to
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
		Description: "Terminate a background process started by terminal with run_in_background:true (the id it returned), and return its final output. Use to stop a server, watcher, or other long-running command you no longer need. To read output without stopping it, use terminal_output.\n\nFor long-running services (servers, watchers), prefer signal 'SIGTERM' for graceful shutdown so the process can clean up connections and release ports. Use 'SIGKILL' (default) for one-shot tasks or when SIGTERM fails.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The background process id to terminate (e.g. \"bg_1\").",
				},
				"signal": map[string]any{
					"type":        "string",
					"enum":        []string{"SIGTERM", "SIGKILL", "SIGINT"},
					"description": "Signal to send. Defaults to SIGKILL. Use SIGTERM for graceful shutdown of servers and long-running services.",
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
	sig, _ := input["signal"].(string)
	if sig == "" {
		sig = "SIGKILL"
	}
	mgr := t.manager()
	if !mgr.KillWithSignal(id, sig) {
		return agent.ToolResult{Text: ""}, fmt.Errorf("kill_shell: no background process %q", id)
	}
	// Give the process a moment to flush and the waiter to record exit.
	time.Sleep(50 * time.Millisecond)

	out, status, _, _ := mgr.Read(id) // found guaranteed: Kill succeeded
	header := "[killed] [status: " + status + "]"
	if out == "" {
		return agent.ToolResult{Text: header + "\n(no new output)"}, nil
	}
	return agent.ToolResult{Text: header + "\n" + MaybeSpillOutput(id, out)}, nil
}
