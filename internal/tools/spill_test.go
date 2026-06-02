package tools

import (
	"os"
	"strings"
	"testing"
)

// spillHome points ~/.octo at a temp dir so spill files don't touch the real
// home, and cleans up after the test.
func spillHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestMaybeSpillOutput_SmallPassesThrough(t *testing.T) {
	spillHome(t)
	body := "short output\nwith a few lines\n"
	if got := MaybeSpillOutput("bg_1", body); got != body {
		t.Errorf("small output should pass through unchanged, got:\n%s", got)
	}
}

func TestMaybeSpillOutput_LargeSpillsToFile(t *testing.T) {
	spillHome(t)
	// Many short lines well over the byte threshold and the line preview.
	var b strings.Builder
	total := 2000
	for i := 0; i < total; i++ {
		b.WriteString("this is a line of build output number ")
		b.WriteByte(byte('0' + i%10))
		b.WriteByte('\n')
	}
	body := b.String()
	if len(body) <= TerminalSpillBytes {
		t.Fatalf("test setup: body should exceed threshold (%d)", len(body))
	}

	out := MaybeSpillOutput("bg_42", body)

	// Preview must mention the spill, the total line count, and a path.
	if !strings.Contains(out, "output too long") {
		t.Errorf("expected spill marker, got:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "term-bg_42-") {
		t.Errorf("expected spill file path in preview, got:\n%s", out)
	}
	// Inline preview must be far smaller than the full body.
	if len(out) >= len(body) {
		t.Errorf("preview (%d) should be smaller than body (%d)", len(out), len(body))
	}

	// The path named in the preview must exist and hold the full body.
	path := extractSpillPath(t, out)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file %q unreadable: %v", path, err)
	}
	if string(data) != body {
		t.Errorf("spill file should hold the full body verbatim (%d vs %d bytes)", len(data), len(body))
	}

	// CleanSpillFiles must remove this process's spill files.
	CleanSpillFiles()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("CleanSpillFiles should have removed %q (err=%v)", path, err)
	}
}

func TestMaybeSpillOutput_FewLongLinesPassThrough(t *testing.T) {
	spillHome(t)
	// Over the byte threshold but only a handful of (very long) lines — a
	// line-based preview would hide nothing, so we return as-is.
	body := strings.Repeat("x", TerminalSpillBytes+100) + "\n" + strings.Repeat("y", 100)
	if got := MaybeSpillOutput("bg_7", body); got != body {
		t.Errorf("few long lines should pass through unchanged")
	}
}

// extractSpillPath pulls the temp-file path out of the preview marker.
func extractSpillPath(t *testing.T, preview string) string {
	t.Helper()
	for _, line := range strings.Split(preview, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "term-bg_") && strings.HasSuffix(line, ".log") {
			return line
		}
	}
	t.Fatalf("no spill path found in preview:\n%s", preview)
	return ""
}
