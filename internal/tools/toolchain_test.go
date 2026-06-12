package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeExe writes an executable-named file into dir. On Windows an entry needs a
// PATHEXT extension to be found by exec.LookPath; elsewhere it needs the
// execute bit.
func fakeExe(t *testing.T, dir, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// withIsolatedPath points PATH at a single temp dir for the duration of the
// test, so detection sees only the fakes we plant — not the host's real tools.
func withIsolatedPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	return dir
}

func TestDetectToolchain_PresentAndMissing(t *testing.T) {
	dir := withIsolatedPath(t)
	fakeExe(t, dir, "git")
	fakeExe(t, dir, "node")
	fakeExe(t, dir, "python3") // satisfies the "python" probe via its variant

	present, missing := DetectToolchain()

	wantPresent := map[string]bool{"git": true, "node": true, "python": true}
	for _, p := range present {
		if !wantPresent[p] {
			t.Errorf("unexpected present tool %q", p)
		}
		delete(wantPresent, p)
	}
	if len(wantPresent) != 0 {
		t.Errorf("missing expected-present tools: %v", wantPresent)
	}

	for _, m := range missing {
		if m == "git" || m == "node" || m == "python" {
			t.Errorf("%q reported missing but was planted", m)
		}
	}
}

func TestDetectToolchain_PythonVariantCollapses(t *testing.T) {
	dir := withIsolatedPath(t)
	fakeExe(t, dir, "python") // the non-3 variant alone must satisfy "python"

	present, _ := DetectToolchain()
	found := false
	for _, p := range present {
		if p == "python" {
			found = true
		}
	}
	if !found {
		t.Error("bare `python` on PATH did not satisfy the python probe")
	}
}

func TestToolchainNote_FormatsBothSections(t *testing.T) {
	dir := withIsolatedPath(t)
	fakeExe(t, dir, "go")

	note := ToolchainNote()
	if !strings.Contains(note, "Detected tools on PATH:") || !strings.Contains(note, "go") {
		t.Errorf("note missing detected section: %q", note)
	}
	if !strings.Contains(note, "Not found") {
		t.Errorf("note missing not-found section: %q", note)
	}
}
