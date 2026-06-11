package prompt

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIdentityPath_LegacyUppercaseFallback: pre-0.19 onboarding wrote
// SOUL.md/USER.md while composition reads soul.md/user.md — distinct files
// on case-sensitive filesystems. IdentityPath must keep those users' files
// readable. Assertions go through file content, not path equality, so the
// test is meaningful on Linux and trivially consistent on case-insensitive
// macOS.
func TestIdentityPath_LegacyUppercaseFallback(t *testing.T) {
	t.Run("legacy uppercase only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("legacy soul"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(IdentityPath(dir, "soul.md"))
		if err != nil {
			t.Fatalf("legacy SOUL.md unreachable via IdentityPath: %v", err)
		}
		if string(got) != "legacy soul" {
			t.Errorf("content = %q, want %q", got, "legacy soul")
		}
	})

	t.Run("lowercase wins when both exist", func(t *testing.T) {
		dir := t.TempDir()
		// On case-insensitive filesystems the second write overwrites the
		// first, so both spellings hold the canonical content — the
		// assertion stays valid on every platform.
		if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("canonical"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "soul.md"), []byte("canonical"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(IdentityPath(dir, "soul.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "canonical" {
			t.Errorf("content = %q, want %q", got, "canonical")
		}
	})

	t.Run("neither exists returns canonical path", func(t *testing.T) {
		dir := t.TempDir()
		want := filepath.Join(dir, "user.md")
		if got := IdentityPath(dir, "user.md"); got != want {
			t.Errorf("IdentityPath = %q, want canonical %q", got, want)
		}
	})
}
