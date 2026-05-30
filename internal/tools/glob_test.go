package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGlob_FlatStarPattern(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a.go"), "")
	writeTestFile(t, filepath.Join(dir, "b.go"), "")
	writeTestFile(t, filepath.Join(dir, "c.txt"), "")

	out, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{
		"pattern": "*.go",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "a.go") || !strings.Contains(out.Text, "b.go") {
		t.Errorf("expected a.go and b.go in output:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "c.txt") {
		t.Errorf(".txt should not match *.go:\n%s", out.Text)
	}
}

func TestGlob_DoubleStarRecursive(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "top.go"), "")
	writeTestFile(t, filepath.Join(dir, "sub", "mid.go"), "")
	writeTestFile(t, filepath.Join(dir, "sub", "deeper", "leaf.go"), "")
	writeTestFile(t, filepath.Join(dir, "sub", "deeper", "other.txt"), "")

	out, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{
		"pattern": "**/*.go",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"top.go", "mid.go", "leaf.go"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("expected %s in output:\n%s", want, out.Text)
		}
	}
	if strings.Contains(out.Text, "other.txt") {
		t.Errorf("other.txt should not match")
	}
}

func TestGlob_SortedByMtimeDescending(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.txt")
	newer := filepath.Join(dir, "newer.txt")
	writeTestFile(t, older, "")
	// Sleep just enough to give different mtimes on coarse-grained filesystems.
	time.Sleep(20 * time.Millisecond)
	writeTestFile(t, newer, "")

	out, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{
		"pattern": "*.txt",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// newer.txt should appear before older.txt in the output.
	idxNew := strings.Index(out.Text, "newer.txt")
	idxOld := strings.Index(out.Text, "older.txt")
	if idxNew < 0 || idxOld < 0 || idxNew > idxOld {
		t.Errorf("expected newer.txt before older.txt:\n%s", out.Text)
	}
}

func TestGlob_SkipsGitAndNodeModules(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "src", "real.go"), "")
	writeTestFile(t, filepath.Join(dir, ".git", "objects", "ignored.go"), "")
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "ignored.go"), "")

	out, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{
		"pattern": "**/*.go",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "real.go") {
		t.Errorf("real.go missing:\n%s", out.Text)
	}
	if strings.Contains(out.Text, ".git/") || strings.Contains(out.Text, "node_modules/") {
		t.Errorf("noise directories should be skipped:\n%s", out.Text)
	}
}

func TestGlob_NoMatches(t *testing.T) {
	dir := t.TempDir()
	out, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{
		"pattern": "*.nonsense",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "no matches") {
		t.Errorf("expected 'no matches' message: %q", out.Text)
	}
}

func TestGlob_RequiresPattern(t *testing.T) {
	_, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{})
	if err == nil {
		t.Fatal("missing pattern should error")
	}
}
