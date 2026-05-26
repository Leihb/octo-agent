// Package tools provides built-in tool implementations for the octo agentic
// loop. Each tool implements agent.ToolExecutor and exposes a Definition()
// method that returns the agent.ToolDefinition the LLM sees.
package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// TerminalTimeout is the maximum time a single terminal command may run.
const TerminalTimeout = 30 * time.Second

// TerminalTool is an agent.ToolExecutor that runs shell commands via `sh -c`.
// Stdout and stderr are combined and returned as the tool result. Non-zero
// exit codes are reported as extra metadata in the result text rather than
// as a tool error, so the LLM can see the failure output and adapt.
//
// The LLM-facing tool name is "terminal" — calling it "bash" would imply a
// hard /bin/bash dependency, but the executor actually shells out via
// `sh -c`.
type TerminalTool struct{}

// Definition returns the agent.ToolDefinition the LLM receives in the tools
// list. The JSON Schema describes a single required "command" string parameter.
func (TerminalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "terminal",
		Description: "Run a shell command (via `sh -c`) and return stdout+stderr. Use for file operations, running programs, searching code, etc.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
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
func (t TerminalTool) Execute(ctx context.Context, name string, input map[string]any) (string, error) {
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
func (TerminalTool) ExecuteStream(
	ctx context.Context,
	_ string,
	input map[string]any,
	progress func(chunk string),
) (string, error) {
	command, _ := input["command"].(string)
	if command == "" {
		return "", fmt.Errorf("terminal: command is required")
	}

	ctx, cancel := context.WithTimeout(ctx, TerminalTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	// Merge stdout + stderr through a single pipe so the reader sees a
	// chronological stream. Doing `cmd.Stderr = cmd.Stdout` after StdoutPipe
	// doesn't work in all Go versions; the io.Pipe pattern is portable.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", fmt.Errorf("terminal: start: %w", err)
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

	waitErr := cmd.Wait()
	_ = pw.Close() // unblocks the scanner's Read by EOF
	<-readDone     // ensure goroutine has flushed before reading `out`

	body := strings.TrimRight(out.String(), "\n")
	if waitErr != nil {
		// Match the original Execute contract: non-zero exit is surfaced as
		// result text, not as a Go error, so the LLM can read and adapt.
		return body + "\n[exit: " + waitErr.Error() + "]", nil
	}
	return body, nil
}
