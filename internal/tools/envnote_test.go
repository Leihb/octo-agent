package tools

import (
	"runtime"
	"strings"
	"testing"
)

// The notes are runtime-gated, so assert the constants directly — the
// guidance must keep its load-bearing elements regardless of test platform.
func TestShellEnvNoteContent(t *testing.T) {
	for _, want := range []string{
		"PowerShell",
		"GetEnvironmentVariable('Path','Machine')", // in-command PATH refresh
		"restart octo",
		"UAC",
		"npm.cmd", // execution-policy workaround
	} {
		if !strings.Contains(shellEnvNoteWindows, want) {
			t.Errorf("windows note missing %q", want)
		}
	}
	for _, want := range []string{
		"sudo",              // non-interactive shell hangs on the password prompt
		"/opt/homebrew/bin", // Apple Silicon PATH trap
		"Xcode Command Line Tools",
		"restart octo",
	} {
		if !strings.Contains(shellEnvNoteDarwin, want) {
			t.Errorf("darwin note missing %q", want)
		}
	}
}

func TestShellEnvNote_PlatformGate(t *testing.T) {
	got := ShellEnvNote()
	switch runtime.GOOS {
	case "windows":
		if got != shellEnvNoteWindows {
			t.Error("windows should get the windows note")
		}
	case "darwin":
		if got != shellEnvNoteDarwin {
			t.Error("darwin should get the darwin note")
		}
	default:
		if got != "" {
			t.Errorf("non-windows/darwin should get no note, got %q", got)
		}
	}
}
