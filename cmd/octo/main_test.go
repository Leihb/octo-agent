package main

import (
	"bytes"
	"strings"
	"testing"
)

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

func TestPrintUsage_ListsInit(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	if !strings.Contains(buf.String(), "init") {
		t.Errorf("usage should list the init command; got: %q", buf.String())
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

func TestRun_PositionalMessage_RoutesToChat(t *testing.T) {
	// A first arg that isn't a named subcommand is a chat prompt now, not an
	// "unknown command" error. With no API key configured the chat path fails
	// with the missing-key message — proof routing reached runChat, offline.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	var stdout, stderr bytes.Buffer
	code := run([]string{"summarise the README"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("stderr should mention the missing key; got: %q", stderr.String())
	}
}

func TestRun_TopLevelFlags_RouteToChat(t *testing.T) {
	// Session flags work without a subcommand. Flag validation is offline and
	// deterministic, so a bad --permission-mode proves the routing.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--permission-mode", "bogus", "hi"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission-mode") {
		t.Errorf("stderr should explain the bad permission-mode; got: %q", stderr.String())
	}
}

func TestRun_Sessions_Empty(t *testing.T) {
	// `octo sessions` replaced --list-sessions. With an isolated HOME there's
	// nothing saved, so it reports that and exits 0 — no provider needed.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	var stdout, stderr bytes.Buffer
	code := run([]string{"sessions"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No saved sessions.") {
		t.Errorf("stdout should report no sessions; got: %q", stdout.String())
	}
}

func TestRun_Sessions_RejectsArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sessions", "extra"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: octo sessions") {
		t.Errorf("stderr should print usage; got: %q", stderr.String())
	}
}

func TestNormalizeBareContinue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"bare -c at end", []string{"-c"}, []string{"-c", pickSessionSentinel}},
		{"bare --continue at end", []string{"--continue"}, []string{"--continue", pickSessionSentinel}},
		{"-c followed by a flag", []string{"-c", "--no-tools"}, []string{"-c", pickSessionSentinel, "--no-tools"}},
		{"-c with an ID stays put", []string{"-c", "last"}, []string{"-c", "last"}},
		{"--continue=id form untouched", []string{"--continue=abc"}, []string{"--continue=abc"}},
		{"unrelated args untouched", []string{"--no-tools", "hello"}, []string{"--no-tools", "hello"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeBareContinue(tc.in); !sliceEq(got, tc.want) {
				t.Errorf("normalizeBareContinue(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRun_BareContinue_NonTTY_Errors(t *testing.T) {
	// The picker needs a terminal; over a pipe a bare -c errors with a
	// pointer at `octo sessions` instead of hanging on a list nobody sees.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-c"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "needs a terminal") {
		t.Errorf("stderr should explain the TTY requirement; got: %q", stderr.String())
	}
}

func TestRun_HelpWithSubcommand_PrintsRichHelp(t *testing.T) {
	cases := []struct {
		cmd      string
		wantHits []string
	}{
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
