package rgembed

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPath_SystemRG(t *testing.T) {
	// If rg is on PATH, Path() should return "rg" directly.
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH — skipping system-rg test")
	}
	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	if p != "rg" {
		t.Errorf("Path() = %q, want %q", p, "rg")
	}
}

func TestPath_ExtractEmbedded(t *testing.T) {
	// Temporarily shadow PATH so LookPath fails, forcing extraction.
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	p, err := Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}

	// Should be an absolute path to the extracted binary.
	if !filepath.IsAbs(p) {
		t.Errorf("Path() = %q, expected absolute path", p)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat(%q): %v", p, err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("%q is not executable", p)
	}

	// The binary should actually run and report --version.
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		t.Fatalf("%q --version failed: %v", p, err)
	}
	if len(out) == 0 {
		t.Errorf("%q --version produced no output", p)
	}
}

func TestRgBinName(t *testing.T) {
	name := rgBinName()
	if runtime.GOOS == "windows" {
		if !hasSuffix(name, ".exe") {
			t.Errorf("rgBinName() = %q, expected .exe suffix on Windows", name)
		}
	} else {
		if hasSuffix(name, ".exe") {
			t.Errorf("rgBinName() = %q, unexpected .exe suffix on %s", name, runtime.GOOS)
		}
	}
	if name == "" {
		t.Error("rgBinName() returned empty string")
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
