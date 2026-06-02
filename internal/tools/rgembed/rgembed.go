// Package rgembed provides a bundled ripgrep (rg) binary for the current
// platform. At build time the Makefile downloads the matching release asset
// from BurntSushi/ripgrep and places it in binaries/rg; go:embed bakes it
// into the octo binary. At runtime, if the system does not have rg on PATH,
// the embedded binary is extracted to ~/.octo/bin/rg and used instead.
package rgembed

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

//go:embed binaries/rg
var embeddedRG []byte

// version is the ripgrep release version embedded at build time.
// Overridden via -ldflags in the Makefile.
var version = "unknown"

// Path returns the absolute path to a working rg binary.
//
// 1. If rg is on PATH, return "rg" (let exec.Command find it).
// 2. Otherwise extract the embedded binary to ~/.octo/bin/rg-<version>
//    and return that path. The file is written atomically and its
//    executable bit is set.
func Path() (string, error) {
	if _, err := exec.LookPath("rg"); err == nil {
		return "rg", nil
	}
	return extract()
}

// extract writes the embedded rg binary to the user cache dir and returns
// its path. The destination includes the version so upgrading octo (which
// bumps the embedded version) automatically extracts a fresh binary.
func extract() (string, error) {
	dir, err := octoBinDir()
	if err != nil {
		return "", fmt.Errorf("rgembed: cache dir: %w", err)
	}
	bin := filepath.Join(dir, rgBinName())

	// Already extracted and valid — nothing to do.
	if info, err := os.Stat(bin); err == nil && info.Mode()&0111 != 0 {
		return bin, nil
	}

	// Write to a temp file in the same dir, then rename atomically.
	tmp, err := os.CreateTemp(dir, "rg-*")
	if err != nil {
		return "", fmt.Errorf("rgembed: create temp: %w", err)
	}
	if _, err := tmp.Write(embeddedRG); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("rgembed: write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("rgembed: close temp: %w", err)
	}

	// Make executable (owner + group + other).
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("rgembed: chmod: %w", err)
	}

	if err := os.Rename(tmp.Name(), bin); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("rgembed: rename: %w", err)
	}

	return bin, nil
}

// octoBinDir returns ~/.octo/bin, creating it if necessary.
func octoBinDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".octo", "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// rgBinName returns the platform-specific binary name.
// On Windows it includes .exe; the version is baked in so upgrades
// automatically extract a fresh binary.
func rgBinName() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("rg-%s.exe", version)
	}
	return fmt.Sprintf("rg-%s", version)
}

// CopyTo writes the embedded rg binary to w. Useful for tests or
// diagnostics that need the raw bytes without the extraction logic.
func CopyTo(w io.Writer) error {
	_, err := w.Write(embeddedRG)
	return err
}
