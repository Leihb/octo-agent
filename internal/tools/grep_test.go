package tools

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireRg skips the test if ripgrep isn't installed. We intentionally
// don't reimplement rg in Go for tests — if a contributor has rg they get
// real coverage; if not, the build still passes.
func requireRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed; skipping grep test")
	}
}

func TestGrep_ContentMode(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.go"), "package main\n\nfunc Hello() {}\n")
	writeTestFile(t, filepath.Join(dir, "b.go"), "package main\n\nfunc World() {}\n")

	out, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "func ",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "func Hello") || !strings.Contains(out.Text, "func World") {
		t.Errorf("expected both functions in output:\n%s", out.Text)
	}
}

func TestGrep_FilesWithMatches(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "needle\n")
	writeTestFile(t, filepath.Join(dir, "b.txt"), "haystack\n")

	out, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "needle",
		"path":    dir,
		"mode":    "files_with_matches",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "a.txt") {
		t.Errorf("expected a.txt in output:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "b.txt") {
		t.Errorf("b.txt has no match — shouldn't appear:\n%s", out.Text)
	}
}

func TestGrep_NoMatches(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "hello\n")

	out, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "definitely-not-there-xyz",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute should not error on zero matches: %v", err)
	}
	if !strings.Contains(out.Text, "no matches") {
		t.Errorf("expected 'no matches' message: %q", out.Text)
	}
}

func TestGrep_CaseInsensitive(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.txt"), "HELLO world\n")

	out, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern":          "hello",
		"path":             dir,
		"case_insensitive": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "HELLO") {
		t.Errorf("case-insensitive should match: %q", out.Text)
	}
}

func TestGrep_IncludeGlob(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.go"), "match here\n")
	writeTestFile(t, filepath.Join(dir, "b.txt"), "match here\n")

	out, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "match",
		"path":    dir,
		"include": "*.go",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "a.go") {
		t.Errorf("a.go should match:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "b.txt") {
		t.Errorf("b.txt should be excluded by include glob:\n%s", out.Text)
	}
}

func TestGrep_UnknownMode_Errors(t *testing.T) {
	requireRg(t)
	_, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "x",
		"mode":    "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("expected unknown-mode error, got %v", err)
	}
}

func TestGrep_RequiresPattern(t *testing.T) {
	requireRg(t)
	_, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{})
	if err == nil {
		t.Fatal("missing pattern should error")
	}
}

func TestGrep_TruncatesLongMatchingLines(t *testing.T) {
	requireRg(t)
	// Simulate a minified-bundle hit: one line of 1000 a's containing
	// the pattern. With --max-columns 500, rg should suppress the body
	// and emit the "[Omitted long matching line]" marker instead.
	dir := t.TempDir()
	longLine := strings.Repeat("a", 1000) + "NEEDLE" + strings.Repeat("b", 1000)
	writeTestFile(t, filepath.Join(dir, "minified.js"), longLine+"\n")

	out, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "NEEDLE",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The literal long run of 'a' must NOT appear in output.
	if strings.Contains(out.Text, strings.Repeat("a", 200)) {
		sample := out.Text
		if len(sample) > 200 {
			sample = sample[:200]
		}
		t.Errorf("long line leaked through; head of output:\n%s", sample)
	}
	if !strings.Contains(out.Text, "Omitted long matching line") {
		t.Errorf("expected omission marker; output:\n%s", out.Text)
	}
}
