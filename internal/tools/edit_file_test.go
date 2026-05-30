package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditFile_UniqueReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.go")
	writeTestFile(t, path, "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")

	out, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": `println("hello")`,
		"new_string": `println("hi there")`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "Replaced 1 occurrence") {
		t.Errorf("status = %q", out.Text)
	}
	if !strings.Contains(readTestFile(t, path), `"hi there"`) {
		t.Error("replacement did not land in file")
	}
}

func TestEditFile_NonUniqueWithoutReplaceAll_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.txt")
	writeTestFile(t, path, "foo\nfoo\nbar\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "foo",
		"new_string": "baz",
	})
	if err == nil || !strings.Contains(err.Error(), "matches 2 times") {
		t.Errorf("expected non-unique error, got %v", err)
	}
	// File must be untouched on error.
	if got := readTestFile(t, path); got != "foo\nfoo\nbar\n" {
		t.Errorf("file was modified despite error: %q", got)
	}
}

func TestEditFile_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.txt")
	writeTestFile(t, path, "foo\nfoo\nbar\nfoo\n")

	out, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":        path,
		"old_string":  "foo",
		"new_string":  "baz",
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "Replaced 3 occurrence(s)") {
		t.Errorf("status = %q", out.Text)
	}
	if got := readTestFile(t, path); got != "baz\nbaz\nbar\nbaz\n" {
		t.Errorf("file = %q", got)
	}
}

func TestEditFile_NotFound_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	writeTestFile(t, path, "hello world")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "missing",
		"new_string": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestEditFile_FileMissing(t *testing.T) {
	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       "/nope/nope/nope.txt",
		"old_string": "a",
		"new_string": "b",
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditFile_IdenticalRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	writeTestFile(t, path, "hello")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "hello",
		"new_string": "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Errorf("expected identical-strings error, got %v", err)
	}
}

func TestEditFile_EmptyOldString_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	writeTestFile(t, path, "hi")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "",
		"new_string": "x",
	})
	if err == nil {
		t.Fatal("empty old_string should be rejected")
	}
}

func TestEditFile_DeleteByEmptyNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	writeTestFile(t, path, "hello world")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": " world",
		"new_string": "",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := readTestFile(t, path); got != "hello" {
		t.Errorf("file = %q", got)
	}
}

func TestEditFile_CRLFNormalization_MatchAndPreserve(t *testing.T) {
	// Windows-style file: \r\n line endings on disk. LLM supplies
	// old_string with plain \n (what read_file shows). The edit must
	// (a) match, and (b) write the result back with \r\n preserved.
	dir := t.TempDir()
	path := filepath.Join(dir, "win.txt")
	writeTestFile(t, path, "line one\r\nline two\r\nline three\r\n")

	out, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "line two", // LF-style (what read_file would show)
		"new_string": "LINE TWO",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "Replaced 1 occurrence") {
		t.Errorf("status = %q", out.Text)
	}
	got := readTestFile(t, path)
	want := "line one\r\nLINE TWO\r\nline three\r\n"
	if got != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestEditFile_LFFilesStayLF(t *testing.T) {
	// Files that were LF on disk must NOT be coerced to CRLF — the
	// normalization round-trip only kicks in when \r\n was present.
	dir := t.TempDir()
	path := filepath.Join(dir, "unix.txt")
	writeTestFile(t, path, "a\nb\nc\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "b",
		"new_string": "B",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	if got != "a\nB\nc\n" {
		t.Errorf("LF file should stay LF; got %q", got)
	}
	if strings.Contains(got, "\r\n") {
		t.Errorf("CRLF leaked into LF-only file: %q", got)
	}
}
