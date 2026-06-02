package conductor

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
)

// CmdVerifier is the objective gate as a sequence of shell commands run in
// the unit's working directory. All must exit 0 for a green verdict; the
// first failing command short-circuits and its output tail becomes the
// summary fed back to the worker. For a Go project the default is
// `go build ./... && go vet ./... && go test ./...`.
type CmdVerifier struct {
	// Commands are run in order, each via the platform shell, in the target
	// workdir. A non-zero exit stops the sequence and yields a red verdict.
	Commands []string
	// MaxSummaryBytes caps the failure summary handed back to the worker so a
	// huge test log doesn't blow the prompt. <=0 → 4000.
	MaxSummaryBytes int
}

// DefaultGoCommands is the standard Go verification gate.
var DefaultGoCommands = []string{
	"go build ./...",
	"go vet ./...",
	"go test ./...",
}

// NewGoVerifier returns a CmdVerifier running the standard Go gate.
func NewGoVerifier() *CmdVerifier { return &CmdVerifier{Commands: DefaultGoCommands} }

// Verify runs each command in target.Workdir ("" = process cwd). It returns a
// green verdict only if every command exits 0. A command that fails to start
// (e.g. shell missing) returns a non-nil error; a command that runs but exits
// non-zero is a red verdict, not an error. Only target.Workdir is used — the
// shell gate is objective and doesn't reason about the goal/result.
func (v *CmdVerifier) Verify(ctx context.Context, target VerifyTarget) (Verdict, error) {
	if len(v.Commands) == 0 {
		return Verdict{Green: true}, nil
	}
	for _, cmdStr := range v.Commands {
		out, exitOK, runErr := runShell(ctx, target.Workdir, cmdStr)
		if runErr != nil {
			return Verdict{}, runErr
		}
		if !exitOK {
			return Verdict{
				Green:   false,
				Summary: "`" + cmdStr + "` failed:\n" + v.tail(out),
			}, nil
		}
	}
	return Verdict{Green: true}, nil
}

func (v *CmdVerifier) tail(s string) string {
	max := v.MaxSummaryBytes
	if max <= 0 {
		max = 4000
	}
	s = strings.TrimRight(s, "\n")
	if len(s) <= max {
		return s
	}
	return "…[earlier output truncated]\n" + s[len(s)-max:]
}

// runShell runs one command via the platform shell in dir, returning combined
// output, whether it exited 0, and a non-nil error only if it couldn't run at
// all. Mirrors internal/tools' shell selection (sh -c / PowerShell).
func runShell(ctx context.Context, dir, command string) (out string, exitOK bool, err error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shell := "powershell"
		if _, e := exec.LookPath("pwsh"); e == nil {
			shell = "pwsh"
		}
		cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	if runErr == nil {
		return buf.String(), true, nil
	}
	if _, ok := runErr.(*exec.ExitError); ok {
		return buf.String(), false, nil // ran, exited non-zero → red, not error
	}
	return buf.String(), false, runErr // couldn't start
}
