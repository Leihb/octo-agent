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

func TestEditFile_CurlyQuotes_NormalizedMatch(t *testing.T) {
	// File contains curly quotes; LLM supplies straight quotes.
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	writeTestFile(t, path, "He said \u201chello\u201d to me\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": `"hello"`,
		"new_string": `"hi there"`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	// Replacement should preserve the file's curly quote style
	want := "He said \u201chi there\u201d to me\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_CurlyQuotes_Single(t *testing.T) {
	// File contains curly single quotes; LLM supplies straight quotes.
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	writeTestFile(t, path, "It\u2019s a nice day\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "It's a nice",
		"new_string": "It's a great",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	// Apostrophe in contraction should become right single curly quote
	want := "It\u2019s a great day\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_TrailingWhitespace_Stripped(t *testing.T) {
	// LLM may add trailing whitespace to new_string; it should be stripped
	// (except for markdown files).
	dir := t.TempDir()
	path := filepath.Join(dir, "code.go")
	writeTestFile(t, path, "func main() {\n\tprintln(\"hello\")\n}\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "println(\"hello\")",
		"new_string": "println(\"hi\")   ", // trailing spaces
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	want := "func main() {\n\tprintln(\"hi\")\n}\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_TrailingWhitespace_MarkdownPreserved(t *testing.T) {
	// Markdown files: trailing double-space is a hard line break — preserve it.
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	writeTestFile(t, path, "Line one\nLine two\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "Line one",
		"new_string": "Line one  ", // trailing double-space (hard break)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	want := "Line one  \nLine two\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_CurlyQuotes_NotFound(t *testing.T) {
	// When even quote-normalized match fails, error should hint at it.
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	writeTestFile(t, path, "plain text\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "missing",
		"new_string": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestEditFile_CurlyQuotes_ReplaceAll(t *testing.T) {
	// replace_all with curly quotes in file.
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	writeTestFile(t, path, "\u201chello\u201d and \u201chello\u201d\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":        path,
		"old_string":  `"hello"`,
		"new_string":  `"hi"`,
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	want := "\u201chi\u201d and \u201chi\u201d\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_RefusesSecretInNewString(t *testing.T) {
	// edit_file must apply the same secret guard write_file does, so a
	// credential can't be injected through the edit path. The original file is
	// left untouched on refusal.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.go")
	original := "const token = \"REPLACE_ME\"\n"
	writeTestFile(t, path, original)

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "REPLACE_ME",
		"new_string": "ghp_0123456789abcdefghijklmnopqrstuvwxyz", // GitHub PAT shape (36 chars)
	})
	if err == nil || !strings.Contains(err.Error(), "GitHub token") {
		t.Fatalf("expected refusal naming the secret, got %v", err)
	}
	if got := readTestFile(t, path); got != original {
		t.Errorf("file should be unchanged on refusal, got %q", got)
	}
}

func TestStripTrailingWhitespace_EveryLine(t *testing.T) {
	// Trailing spaces/tabs must be stripped on every line, not just the last \u2014
	// a "\n" must not shield the whitespace in front of it.
	in := "a  \nb\t\nc   "
	want := "a\nb\nc"
	if got := stripTrailingWhitespace(in); got != want {
		t.Errorf("stripTrailingWhitespace(%q) = %q, want %q", in, got, want)
	}
}

func TestEditFile_TabIndent_NormalizedMatch(t *testing.T) {
	// File uses tabs for indentation; LLM supplies spaces.
	// The whitespace-normalized match should find it and map back
	// to the actual tab-indented text.
	dir := t.TempDir()
	path := filepath.Join(dir, "tabbed.go")
	writeTestFile(t, path, "\tcase \"chat\":\n\t\tchatHelp(w)\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "    case \"chat\":\n        chatHelp(w)", // spaces, not tabs
		"new_string": "    case \"chat\":\n        chatHelp(w)\n        // new",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	// File should keep its tab convention
	want := "\tcase \"chat\":\n\t\tchatHelp(w)\n\t\t// new\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_TabIndent_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.go")
	writeTestFile(t, path, "\tfoo\n\tfoo\n\tbar\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":        path,
		"old_string":  "    foo", // 4 spaces
		"new_string":  "    baz", // 4 spaces
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	// File keeps tabs, but all "foo" lines become "baz"
	want := "\tbaz\n\tbaz\n\tbar\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_TabIndent_NotFound_StillErrors(t *testing.T) {
	// When even whitespace normalization can't find the string, error.
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	writeTestFile(t, path, "\tpackage main\n\n\tfunc main() {}\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "nonexistent",
		"new_string": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestNormalizeLeadingWhitespace(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"\tcase:", "    case:"},
		{"\t\tchatHelp(w)", "        chatHelp(w)"},
		{"no indent", "no indent"},
		{"  spaces", "  spaces"},     // spaces unchanged
		{"\t  mixed", "      mixed"}, // tab + spaces
		{"a\n\tb\nc", "a\n    b\nc"}, // multi-line
	}
	for _, tc := range tests {
		got := normalizeLeadingWhitespace(tc.in, 4)
		if got != tc.want {
			t.Errorf("normalizeLeadingWhitespace(%q, 4) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFindWithIndentNorm(t *testing.T) {
	file := "\tcase \"chat\":\n\t\tchatHelp(w)\n"
	search := "    case \"chat\":\n        chatHelp(w)"

	actual := findWithIndentNorm(file, search, 4)
	// search has no trailing \n, so actual should also lack it
	want := "\tcase \"chat\":\n\t\tchatHelp(w)"
	if actual != want {
		t.Errorf("findWithIndentNorm = %q, want %q", actual, want)
	}
}

func TestFindWithIndentNorm_NotFound(t *testing.T) {
	file := "\tfoo\n\tbar\n"
	search := "        missing"

	actual := findWithIndentNorm(file, search, 4)
	if actual != "" {
		t.Errorf("expected empty for non-matching search, got %q", actual)
	}
}

func TestFindWithIndentNorm_TrailingNewline(t *testing.T) {
	// A search string ending with \n used to require the file's NEXT line to
	// be empty (line-split artifact). It must match any mid-file block whose
	// last line is newline-terminated.
	file := "\tfoo\n\tbar\n"
	search := "    foo\n"

	actual := findWithIndentNorm(file, search, 4)
	if actual != "\tfoo\n" {
		t.Errorf("findWithIndentNorm = %q, want %q", actual, "\tfoo\n")
	}
}

func TestFindWithIndentNorm_TrailingNewline_EOFWithoutNewline(t *testing.T) {
	// The search demands a trailing newline the file does not have —
	// returning the match anyway would produce an actualOldStr that is not a
	// substring of the file.
	file := "\tfoo"
	search := "    foo\n"

	actual := findWithIndentNorm(file, search, 4)
	if actual != "" {
		t.Errorf("expected empty when file lacks the trailing newline, got %q", actual)
	}
}

func TestEditFile_TabIndent_EightSpaceWidth(t *testing.T) {
	// Model assumed an 8-wide tab rendering. The width-4 pass misses,
	// the width-8 pass matches, and new_string converts back at 8.
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.go")
	writeTestFile(t, path, "\tif ok {\n\t\treturn\n\t}\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "        if ok {\n                return\n        }",
		"new_string": "        if ok {\n                return nil\n        }",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	want := "\tif ok {\n\t\treturn nil\n\t}\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestEditFile_TabIndent_TwoSpaceWidth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "narrow.go")
	writeTestFile(t, path, "\tfoo()\n\t\tbar()\n")

	_, err := EditFileTool{}.Execute(context.Background(), "edit_file", map[string]any{
		"path":       path,
		"old_string": "  foo()\n    bar()",
		"new_string": "  foo2()\n    bar()",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := readTestFile(t, path)
	want := "\tfoo2()\n\t\tbar()\n"
	if got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestPreserveIndentStyle_KeepsRemainderSpaces(t *testing.T) {
	// 6 spaces at width 4 = 1 tab + 2 alignment spaces. The old level
	// arithmetic truncated to 1 tab; 2 or 3 spaces collapsed to column zero.
	old := "\tindented"
	got := preserveIndentStyle(old, "      aligned\n  partial", 4)
	want := "\t  aligned\n  partial"
	if got != want {
		t.Errorf("preserveIndentStyle = %q, want %q", got, want)
	}
}

func TestPreserveIndentStyle_LeavesTabLinesAlone(t *testing.T) {
	// A line that already leads with a tab followed the file convention;
	// its alignment spaces must not be flattened.
	old := "\tindented"
	in := "\t\t  aligned continuation"
	if got := preserveIndentStyle(old, in, 4); got != in {
		t.Errorf("preserveIndentStyle = %q, want unchanged %q", got, in)
	}
}
