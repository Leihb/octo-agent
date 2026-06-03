package main

import (
	"os"
	"path/filepath"
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

	// A terminal escapes spaces with backslashes when a file is dragged in.
	escaped := strings.ReplaceAll(withSpaces, " ", `\ `)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", plain, plain},
		{"bare trailing space", plain + " ", plain},
		{"escaped spaces", escaped, withSpaces},
		{"escaped trailing space", escaped + " ", withSpaces},
		{"single quoted", "'" + withSpaces + "'", withSpaces},
		{"double quoted", `"` + withSpaces + `"`, withSpaces},
		{"embedded in text", "please look at " + escaped + " and explain", withSpaces},
		{"embedded plain", "see " + plain + " here", plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, start, end, ok := findImagePath(tc.in)
			if !ok {
				t.Fatalf("findImagePath(%q) returned ok=false, want path %q", tc.in, tc.want)
			}
			if got != tc.want {
				t.Errorf("path = %q, want %q", got, tc.want)
			}
			if start < 0 || end > len(tc.in) || start >= end {
				t.Errorf("range [%d,%d) out of bounds for %q", start, end, tc.in)
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
	cases := []struct {
		s    string
		i    int
		want bool
	}{
		{"a b", 1, true},   // plain space
		{`a\ b`, 2, false}, // escaped space (odd backslashes)
		{`a\\ b`, 3, true}, // escaped backslash then space (even)
		{"a\tb", 1, true},  // tab
		{"abc", 1, false},  // not whitespace
	}
	for _, tc := range cases {
		if got := isUnescapedBoundary(tc.s, tc.i); got != tc.want {
			t.Errorf("isUnescapedBoundary(%q, %d) = %v, want %v", tc.s, tc.i, got, tc.want)
		}
	}
}
