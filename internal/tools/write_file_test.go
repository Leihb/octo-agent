package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	out, err := WriteFileTool{}.Execute(context.Background(), "write_file", map[string]any{
		"path":    path,
		"content": "hello\nworld\n",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "12 bytes") || !strings.Contains(out.Text, "2 lines") {
		t.Errorf("status message wrong: %q", out.Text)
	}
	if got := readTestFile(t, path); got != "hello\nworld\n" {
		t.Errorf("file content = %q", got)
	}
}

func TestWriteFile_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "tree", "file.txt")

	_, err := WriteFileTool{}.Execute(context.Background(), "write_file", map[string]any{
		"path":    path,
		"content": "ok",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist: %v", err)
	}
}

func TestWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	writeTestFile(t, path, "old content")

	_, err := WriteFileTool{}.Execute(context.Background(), "write_file", map[string]any{
		"path":    path,
		"content": "new",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := readTestFile(t, path); got != "new" {
		t.Errorf("file content = %q, want 'new'", got)
	}
}

func TestWriteFile_EmptyContentAllowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	_, err := WriteFileTool{}.Execute(context.Background(), "write_file", map[string]any{
		"path":    path,
		"content": "",
	})
	if err != nil {
		t.Fatalf("empty content should be allowed: %v", err)
	}
	if got := readTestFile(t, path); got != "" {
		t.Errorf("file should be empty, got %q", got)
	}
}

func TestWriteFile_ValidatesInputs(t *testing.T) {
	w := WriteFileTool{}
	if _, err := w.Execute(context.Background(), "write_file", map[string]any{}); err == nil {
		t.Error("missing path should error")
	}
	if _, err := w.Execute(context.Background(), "write_file", map[string]any{"path": "/tmp/x"}); err == nil {
		t.Error("missing content (nil) should error")
	}
}
