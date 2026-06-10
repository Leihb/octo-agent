package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIHead(t *testing.T) {
	cases := []struct {
		in       string
		maxLines int
		maxBytes int
		want     string
	}{
		{"", 5, 100, ""},
		{"a\nb\nc\n", 5, 100, "a\nb\nc"},
		{"a\nb\nc", 2, 100, "a\nb"},
		{"aaaa\nbbbb\ncccc", 5, 9, "aaaa"}, // byte cap cuts at line boundary
		{"aaaaaaaaaa", 5, 4, "aaaa"},       // single long line: hard byte cut
	}
	for _, c := range cases {
		if got := uiHead(c.in, c.maxLines, c.maxBytes); got != c.want {
			t.Errorf("uiHead(%q,%d,%d) = %q, want %q", c.in, c.maxLines, c.maxBytes, got, c.want)
		}
	}
}

func TestUITail(t *testing.T) {
	cases := []struct {
		in       string
		maxLines int
		maxBytes int
		want     string
	}{
		{"", 5, 100, ""},
		{"a\nb\nc\n", 5, 100, "a\nb\nc"},
		{"a\nb\nc", 2, 100, "b\nc"},
		{"aaaa\nbbbb\ncccc", 5, 9, "cccc"}, // byte cap keeps whole last lines
		{"aaaaaaaaaa", 5, 4, "aaaa"},       // single long line: hard byte cut
	}
	for _, c := range cases {
		if got := uiTail(c.in, c.maxLines, c.maxBytes); got != c.want {
			t.Errorf("uiTail(%q,%d,%d) = %q, want %q", c.in, c.maxLines, c.maxBytes, got, c.want)
		}
	}
}

// uiOf extracts the map payload from a ToolResult, failing the test when
// missing — shared by the per-tool payload tests below.
func uiOf(t *testing.T, res any) map[string]any {
	t.Helper()
	ui, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("UI payload = %T (%v), want map", res, res)
	}
	return ui
}

func TestReadFile_UIPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "file_read" || ui["lines_read"] != 3 || ui["total_lines"] != 3 || ui["truncated"] != false {
		t.Errorf("payload = %+v", ui)
	}
	preview, _ := ui["content_preview"].(string)
	if !strings.Contains(preview, "two") || strings.Contains(preview, "[end of file") {
		t.Errorf("preview = %q, want file content without footers", preview)
	}
}

func TestGlob_UIPayload(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := GlobTool{}.Execute(context.Background(), "glob", map[string]any{
		"pattern": "*.go", "path": dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "file_list" || ui["total"] != 2 {
		t.Errorf("payload = %+v", ui)
	}
	entries, _ := ui["entries"].([]map[string]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	// Entries are root-relative, not absolute.
	if name, _ := entries[0]["name"].(string); filepath.IsAbs(name) || !strings.HasSuffix(name, ".go") {
		t.Errorf("entry name = %q, want relative *.go", name)
	}
}

func TestGrep_UIPayload_ContentMode(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hit here\nmiss\nanother hit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "hit", "path": dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "search" || ui["total_matches"] != 2 || ui["files_with_matches"] != 1 {
		t.Errorf("payload = %+v", ui)
	}
	matches, _ := ui["matches"].([]map[string]any)
	if len(matches) != 2 || matches[0]["line_no"] != 1 {
		t.Errorf("matches = %+v", matches)
	}
}

func TestGrep_UIPayload_SkippedForContextRuns(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hit\nctx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := GrepTool{}.Execute(context.Background(), "grep", map[string]any{
		"pattern": "hit", "path": dir, "context_lines": 1,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.UI != nil {
		t.Errorf("context run should carry no UI payload, got %+v", res.UI)
	}
}

func TestTerminal_UIPayload(t *testing.T) {
	res, err := TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "terminal" || ui["status"] != "success" || ui["command"] != "echo hello" {
		t.Errorf("payload = %+v", ui)
	}

	res, err = TerminalTool{}.Execute(context.Background(), "terminal", map[string]any{
		"command": "echo oops; exit 1",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui = uiOf(t, res.UI)
	if ui["status"] != "failed" {
		t.Errorf("payload = %+v, want status failed", ui)
	}
	if preview, _ := ui["output_preview"].(string); !strings.Contains(preview, "oops") {
		t.Errorf("preview = %q, want stdout tail", preview)
	}
}

func TestEditFile_UIPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello old world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path": path, "old_string": "old world", "new_string": "new world",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "edit" || ui["occurrences"] != 1 {
		t.Errorf("payload = %+v", ui)
	}
	diff, _ := ui["diff"].(string)
	if !strings.Contains(diff, "- old world") || !strings.Contains(diff, "+ new world") {
		t.Errorf("diff = %q, want -old/+new lines", diff)
	}
}

func TestWriteFile_UIPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	res, err := WriteFileTool{}.Execute(context.Background(), "write_file", map[string]any{
		"path": path, "content": "hello\n",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "write" || ui["size_bytes"] != 6 {
		t.Errorf("payload = %+v", ui)
	}
}

func TestTasks_UIPayload(t *testing.T) {
	useTaskStore(t)
	res, err := TaskCreateTool{}.Execute(context.Background(), "task_create", map[string]any{
		"subject": "Do the thing",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ui := uiOf(t, res.UI)
	if ui["type"] != "todo" || ui["action"] != "Created #1" || ui["progress"] != "0/1 done" {
		t.Errorf("payload = %+v", ui)
	}
	todos, _ := ui["todos"].([]map[string]any)
	if len(todos) != 1 || todos[0]["task"] != "Do the thing" || todos[0]["status"] != "pending" {
		t.Errorf("todos = %+v", todos)
	}
}
