package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_NoArgs_PrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage: octo") {
		t.Errorf("stdout missing usage banner; got: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty; got: %q", stderr.String())
	}
}

func TestRun_Version(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{arg}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if !strings.HasPrefix(stdout.String(), "octo ") {
				t.Errorf("stdout should start with 'octo '; got: %q", stdout.String())
			}
		})
	}
}

func TestRun_Usage_ListsInit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run(nil, strings.NewReader(""), &stdout, &stderr)
	if !strings.Contains(stdout.String(), "init") {
		t.Errorf("usage should list the init command; got: %q", stdout.String())
	}
}

func TestRunInit_InvalidPermissionMode(t *testing.T) {
	// Validation happens before any provider/network work, so this is
	// deterministic and offline.
	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--permission-mode", "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "permission-mode") {
		t.Errorf("stderr should explain the bad permission-mode; got: %q", stderr.String())
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("stderr should mention 'unknown command'; got: %q", stderr.String())
	}
}

func TestRun_HelpWithSubcommand_PrintsRichHelp(t *testing.T) {
	cases := []struct {
		cmd      string
		wantHits []string
	}{
		{"chat", []string{"octo chat", "Examples:", "ANTHROPIC_API_KEY", "octo chat -c last"}},
		{"goal", []string{"octo goal", "Examples:", "ID shortcuts", "octo goal start"}},
		{"memory", []string{"octo memory", "octo memory list"}},
		{"init", []string{"octo init", ".octorules"}},
		{"mcp", []string{"octo mcp", "mcp.json", "mcp__"}},
		{"completion", []string{"octo completion", "shell-completion"}},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"help", tc.cmd}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
			}
			for _, want := range tc.wantHits {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("help %s missing %q in output:\n%s", tc.cmd, want, stdout.String())
				}
			}
		})
	}
}

func TestRun_HelpWithUnknownSubcommand_Exits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"help", "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "no help available") {
		t.Errorf("stderr should explain missing help; got %q", stderr.String())
	}
}

func TestRun_TopLevelHelp_PointsToPerCommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"help"}, strings.NewReader(""), &stdout, &stderr)
	// Verify the footer advertises the new `octo help <command>` form so users
	// can discover the rich per-command help.
	if !strings.Contains(stdout.String(), "octo help <command>") {
		t.Errorf("top-level help missing 'octo help <command>' pointer:\n%s", stdout.String())
	}
}
