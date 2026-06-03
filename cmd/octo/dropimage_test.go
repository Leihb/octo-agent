package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findImagePath must recognise a dropped image path in the shapes terminals
// actually emit: bare, backslash-escaped spaces (macOS Terminal/iTerm drag),
// quoted, and embedded in typed text. The regression that motivated the
// rewrite: filenames with spaces (every macOS screenshot) were split at the
// former-escaped space and never matched.
func TestFindImagePath(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "shot.png")
	withSpaces := filepath.Join(dir, "Screenshot 2026-06-03 at 12.00.00.png")
	for _, p := range []string{plain, withSpaces} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	type tc struct {
		name string
		in   string
		want string
	}
	// Shapes that hold on every platform: bare paths, and spaced paths
	// wrapped in quotes (how a Windows drag delivers spaces).
	cases := []tc{
		{"bare", plain, plain},
		{"bare trailing space", plain + " ", plain},
		{"single quoted", "'" + withSpaces + "'", withSpaces},
		{"double quoted", `"` + withSpaces + `"`, withSpaces},
		{"embedded plain", "see " + plain + " here", plain},
	}
	if runtime.GOOS != "windows" {
		// POSIX shells backslash-escape spaces on drag; Windows uses '\' as
		// the path separator, so this form is POSIX-only.
		escaped := strings.ReplaceAll(withSpaces, " ", `\ `)
		cases = append(cases,
			tc{"escaped spaces", escaped, withSpaces},
			tc{"escaped trailing space", escaped + " ", withSpaces},
			tc{"embedded in text", "please look at " + escaped + " and explain", withSpaces},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, start, end, ok := findImagePath(c.in)
			if !ok {
				t.Fatalf("findImagePath(%q) returned ok=false, want path %q", c.in, c.want)
			}
			if got != c.want {
				t.Errorf("path = %q, want %q", got, c.want)
			}
			if start < 0 || end > len(c.in) || start >= end {
				t.Errorf("range [%d,%d) out of bounds for %q", start, end, c.in)
			}
		})
	}
}

func TestFindImagePathMisses(t *testing.T) {
	dir := t.TempDir()
	notImage := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notImage, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"",
		"just some text with no path",
		"a word ending in png but not a file",
		notImage,                          // exists, but not an image
		filepath.Join(dir, "missing.png"), // image ext, no file
		"talk about screenshot.png that isn't here", // image ext, no file
	}
	for _, in := range cases {
		if _, _, _, ok := findImagePath(in); ok {
			t.Errorf("findImagePath(%q) = ok, want miss", in)
		}
	}
}

func TestIsUnescapedBoundary(t *testing.T) {
	type tc struct {
		s    string
		i    int
		want bool
	}
	cases := []tc{
		{"a b", 1, true},  // plain space
		{"a\tb", 1, true}, // tab
		{"abc", 1, false}, // not whitespace
	}
	if runtime.GOOS != "windows" {
		// Backslash only escapes on POSIX; on Windows it is a path separator
		// and a following space is still a boundary.
		cases = append(cases,
			tc{`a\ b`, 2, false}, // escaped space (odd backslashes)
			tc{`a\\ b`, 3, true}, // escaped backslash then space (even)
		)
	}
	for _, c := range cases {
		if got := isUnescapedBoundary(c.s, c.i); got != c.want {
			t.Errorf("isUnescapedBoundary(%q, %d) = %v, want %v", c.s, c.i, got, c.want)
		}
	}
}
