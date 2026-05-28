package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/memoryd"
)

// fakeHomeForMemoryd points $HOME at a tempdir so memoryd lifecycle
// commands write their PID file there.
func fakeHomeForMemoryd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func TestRunMemoryd_NoArgsShowsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runMemoryd(nil, nil, &out, &errBuf); code != 2 {
		t.Errorf("no args exit = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "octo memoryd") {
		t.Errorf("usage should be printed:\n%s", out.String())
	}
}

func TestRunMemoryd_UnknownSubcommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runMemoryd([]string{"bogus"}, nil, &out, &errBuf); code != 2 {
		t.Errorf("unknown subcommand exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' notice:\n%s", errBuf.String())
	}
}

func TestRunMemoryd_HelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		var out, errBuf bytes.Buffer
		if code := runMemoryd([]string{arg}, nil, &out, &errBuf); code != 0 {
			t.Errorf("%s exit = %d, want 0", arg, code)
		}
	}
}

func TestRunMemorydStatus_NoDaemon(t *testing.T) {
	fakeHomeForMemoryd(t)
	var out bytes.Buffer
	if code := runMemoryd([]string{"status"}, nil, &out, &out); code != 0 {
		t.Errorf("status with no daemon exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not running") {
		t.Errorf("status should say 'not running':\n%s", out.String())
	}
}

func TestRunMemorydStatus_StalePIDFile(t *testing.T) {
	dir := fakeHomeForMemoryd(t)
	// Drop a PID file pointing at a long-dead PID.
	pidPath := filepath.Join(dir, ".octo", "memoryd.pid")
	if err := memoryd.WritePIDFile(pidPath, 2_000_000_000); err != nil {
		t.Fatal(err)
	}
	if memoryd.IsRunning(2_000_000_000) {
		t.Skip("PID 2_000_000_000 happens to be alive on this system")
	}
	var out bytes.Buffer
	if code := runMemoryd([]string{"status"}, nil, &out, &out); code != 0 {
		t.Errorf("status with stale pid exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("stale-pid status should be flagged:\n%s", out.String())
	}
}

func TestRunMemorydStatus_AliveDaemon(t *testing.T) {
	if !memoryd.SupportedOnThisOS() {
		t.Skip("daemon model not supported on this OS")
	}
	fakeHomeForMemoryd(t)
	pidPath, _ := memoryd.PIDFile()
	if err := memoryd.WritePIDFile(pidPath, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runMemoryd([]string{"status"}, nil, &out, &out); code != 0 {
		t.Errorf("status with alive pid exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "running") {
		t.Errorf("alive-daemon status should be flagged:\n%s", out.String())
	}
}

func TestRunMemorydStop_NoDaemon(t *testing.T) {
	fakeHomeForMemoryd(t)
	var out, errBuf bytes.Buffer
	if !memoryd.SupportedOnThisOS() {
		t.Skip("memoryd not supported on this OS")
	}
	if code := runMemoryd([]string{"stop"}, nil, &out, &errBuf); code != 0 {
		t.Errorf("stop with no daemon exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no daemon") {
		t.Errorf("stop with no daemon should say so:\n%s", out.String())
	}
}

func TestRunMemorydStop_StalePIDClearsFile(t *testing.T) {
	if !memoryd.SupportedOnThisOS() {
		t.Skip("memoryd not supported on this OS")
	}
	fakeHomeForMemoryd(t)
	pidPath, _ := memoryd.PIDFile()
	if err := memoryd.WritePIDFile(pidPath, 2_000_000_000); err != nil {
		t.Fatal(err)
	}
	if memoryd.IsRunning(2_000_000_000) {
		t.Skip("PID 2_000_000_000 alive — can't test stale path")
	}
	var out bytes.Buffer
	if code := runMemoryd([]string{"stop"}, nil, &out, &out); code != 0 {
		t.Errorf("stop with stale pid exit = %d, want 0", code)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale PID file should be cleared, got %v", err)
	}
}

func TestRunMemorydStart_RefusesIfAlreadyRunning(t *testing.T) {
	if !memoryd.SupportedOnThisOS() {
		t.Skip("memoryd not supported on this OS")
	}
	fakeHomeForMemoryd(t)
	// Provider check runs first; satisfy it so we reach the PID-file
	// check this test actually cares about.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("OPENAI_API_KEY", "")
	// Pretend a daemon is already up (our own PID).
	pidPath, _ := memoryd.PIDFile()
	if err := memoryd.WritePIDFile(pidPath, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runMemoryd([]string{"start"}, nil, &out, &errBuf); code != 1 {
		t.Errorf("start with alive daemon exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "already running") {
		t.Errorf("start should refuse with 'already running':\n%s", errBuf.String())
	}
}

func TestRunMemorydStart_RefusesWithoutAPIKey(t *testing.T) {
	if !memoryd.SupportedOnThisOS() {
		t.Skip("memoryd not supported on this OS")
	}
	fakeHomeForMemoryd(t)
	t.Setenv("OCTO_MEMORYD_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	var out, errBuf bytes.Buffer
	if code := runMemoryd([]string{"start"}, nil, &out, &errBuf); code != 1 {
		t.Errorf("start without keys exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("start should name the missing env var:\n%s", errBuf.String())
	}
	// Critically: no PID file should have been written.
	pidPath, _ := memoryd.PIDFile()
	if _, err := os.Stat(pidPath); err == nil {
		t.Errorf("PID file should NOT exist after a config-error refusal")
	}
}
