package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMicroCompact_ShortUnchanged(t *testing.T) {
	s := "small output"
	if got := microCompact(s); got != s {
		t.Errorf("short output should be unchanged, got %q", got)
	}
	// Exactly at the cap → unchanged.
	atCap := strings.Repeat("x", ToolResultMaxBytes)
	if got := microCompact(atCap); got != atCap {
		t.Errorf("output at the cap should be unchanged (len %d)", len(got))
	}
}

func TestMicroCompact_TruncatesMiddle(t *testing.T) {
	// Distinct head and tail so we can confirm both ends survive.
	head := strings.Repeat("H", ToolResultMaxBytes)
	tail := strings.Repeat("T", ToolResultMaxBytes)
	s := head + tail
	out := microCompact(s)

	if len(out) > ToolResultMaxBytes {
		t.Errorf("truncated output should be within the cap, got %d", len(out))
	}
	if !strings.Contains(out, "truncated by octo") {
		t.Errorf("expected a truncation marker:\n%s", out[:200])
	}
	if !strings.HasPrefix(out, "H") {
		t.Errorf("head should survive, got prefix %q", out[:10])
	}
	if !strings.HasSuffix(out, "T") {
		t.Errorf("tail should survive, got suffix %q", out[len(out)-10:])
	}
}

func TestTruncateMiddle_ValidUTF8AtBoundary(t *testing.T) {
	// A long run of 3-byte runes (你) guarantees the byte-level cut lands
	// mid-rune; the result must still be valid UTF-8.
	s := strings.Repeat("你", 50_000) // 150_000 bytes
	out := truncateMiddle(s, 40_000)
	if !utf8.ValidString(out) {
		t.Errorf("truncated output must be valid UTF-8")
	}
	if len(out) > 40_000 {
		t.Errorf("len = %d, want <= 40000", len(out))
	}
}

// hugeExec returns a tool output far larger than the cap.
type hugeExec struct{}

func (hugeExec) Execute(_ context.Context, _ string, _ map[string]any) (ToolResult, error) {
	return ToolResult{Text: strings.Repeat("A", ToolResultMaxBytes*3)}, nil
}

func TestDispatchTools_TruncatesOversizedResult(t *testing.T) {
	blocks := []ContentBlock{NewToolUseBlock("c1", "read_file", map[string]any{"path": "big"})}
	results, err := dispatchTools(context.Background(), hugeExec{}, blocks, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if len(results[0].Result) > ToolResultMaxBytes {
		t.Errorf("tool result should be micro-compacted, got %d bytes", len(results[0].Result))
	}
	if !strings.Contains(results[0].Result, "truncated by octo") {
		t.Errorf("expected truncation marker in the stored result")
	}
}
