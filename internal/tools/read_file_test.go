package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	writeTestFile(t, path, "line1\nline2\nline3\n")

	out, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"     1\tline1", "     2\tline2", "     3\tline3"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("output missing %q\n%s", want, out.Text)
		}
	}
}

func TestReadFile_OffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.txt")
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("line")
		b.WriteByte(byte('0' + (i % 10)))
		b.WriteByte('\n')
	}
	writeTestFile(t, path, b.String())

	out, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{
		"path":   path,
		"offset": 3,
		"limit":  2,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "     3\tline3") || !strings.Contains(out.Text, "     4\tline4") {
		t.Errorf("offset/limit slice wrong:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "     2\tline2") || strings.Contains(out.Text, "     5\tline5") {
		t.Errorf("offset/limit returned unwanted lines:\n%s", out.Text)
	}
}

func TestReadFile_PastEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.txt")
	writeTestFile(t, path, "only\n")

	_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{
		"path":   path,
		"offset": 100,
	})
	if err == nil || !strings.Contains(err.Error(), "past end") {
		t.Errorf("expected past-end error, got %v", err)
	}
}

func TestReadFile_Missing(t *testing.T) {
	_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{
		"path": "/definitely/not/a/real/path/12345",
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	writeTestFile(t, path, "")

	out, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "empty") {
		t.Errorf("expected empty-file marker, got %q", out.Text)
	}
}

func TestReadFile_RequiresPath(t *testing.T) {
	_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestReadFile_RejectsBinaryExtensions(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		ext     string
		wantSub string
	}{
		{"exe", ".exe", "Windows executable"},
		{"zip", ".zip", "ZIP archive"},
		{"pdf", ".pdf", "PDF document"},
		{"sqlite", ".sqlite", "SQLite database"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, "file"+c.ext)
			writeTestFile(t, path, "bogus content")
			_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": path})
			if err == nil {
				t.Fatalf("%s extension should be refused", c.ext)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error should mention %q; got: %v", c.wantSub, err)
			}
		})
	}
}

func TestReadFile_ReadsImage(t *testing.T) {
	// A valid PNG header: 89 50 4E 47 0D 0A 1A 0A
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.png")
	if err := os.WriteFile(path, pngHeader, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "image/png") {
		t.Errorf("expected image description with MIME type, got: %q", res.Text)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Type != "image" {
		t.Errorf("expected 1 image block, got %+v", res.Blocks)
	}
	if res.Blocks[0].Image == nil || res.Blocks[0].Image.MIMEType != "image/png" {
		t.Errorf("image block MIME type wrong: %+v", res.Blocks[0].Image)
	}
}

func TestReadFile_ImageTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.png")
	// Write a file larger than ReadFileImageMaxBytes
	big := make([]byte, ReadFileImageMaxBytes+1)
	big[0] = 0x89 // PNG magic byte so it passes the extension check
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": path})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected size-limit error, got: %v", err)
	}
}

func TestReadFile_AllowsTextExtensions(t *testing.T) {
	// Sanity check that the binary list isn't too aggressive — common
	// source-code extensions must still be readable.
	dir := t.TempDir()
	cases := []string{".go", ".ts", ".tsx", ".py", ".rb", ".md", ".json", ".yml", ".html", ".css", ".sql", ".rs"}
	for _, ext := range cases {
		t.Run(strings.TrimPrefix(ext, "."), func(t *testing.T) {
			path := filepath.Join(dir, "x"+ext)
			writeTestFile(t, path, "hello\n")
			if _, err := (ReadFileTool{}).Execute(context.Background(), "read_file", map[string]any{"path": path}); err != nil {
				t.Errorf("%s should be readable as text; got %v", ext, err)
			}
		})
	}
}

func TestReadFile_RejectsBlockedDevicePaths(t *testing.T) {
	// On non-Unix, these paths simply don't exist; the test is meaningful
	// only where they would otherwise be readable. Skip if /dev itself
	// isn't there.
	if _, err := os.Stat("/dev"); err != nil {
		t.Skip("/dev not present; skipping device-file test")
	}
	for _, p := range []string{"/dev/random", "/dev/urandom", "/dev/zero", "/dev/tty"} {
		t.Run(strings.TrimPrefix(p, "/dev/"), func(t *testing.T) {
			_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": p})
			if err == nil {
				t.Fatalf("%s should be refused", p)
			}
			if !strings.Contains(err.Error(), "device file") {
				t.Errorf("error should mention device file; got %v", err)
			}
		})
	}
}

func TestReadFile_AllowsDevNull(t *testing.T) {
	// /dev/null returns EOF immediately; it's not on the blocklist.
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("/dev/null not present; skipping")
	}
	out, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": "/dev/null"})
	if err != nil {
		t.Fatalf("/dev/null should be readable (returns EOF): %v", err)
	}
	if !strings.Contains(out.Text, "empty") {
		t.Errorf("expected empty-file marker, got %q", out.Text)
	}
}

func TestReadFile_RefusesUNCPath(t *testing.T) {
	for _, p := range []string{`\\evil-host\share\file`, "//evil-host/share/file"} {
		_, err := ReadFileTool{}.Execute(context.Background(), "read_file", map[string]any{"path": p})
		if err == nil || !strings.Contains(err.Error(), "UNC") {
			t.Errorf("read_file should refuse UNC path %q, got %v", p, err)
		}
	}
}

func TestIsUNCPath(t *testing.T) {
	unc := []string{`\\server\share`, "//server/share"}
	for _, p := range unc {
		if !isUNCPath(p) {
			t.Errorf("isUNCPath(%q) = false, want true", p)
		}
	}
	notUNC := []string{"/etc/passwd", "relative/path", "~/file", `C:\Users\me`, "/single/slash"}
	for _, p := range notUNC {
		if isUNCPath(p) {
			t.Errorf("isUNCPath(%q) = true, want false", p)
		}
	}
}
