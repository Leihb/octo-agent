package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoad_MissingFileIsZeroNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir()) // Windows

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() on missing file = %v, want nil", err)
	}
	if (c != Config{}) {
		t.Errorf("Load() on missing file = %+v, want zero Config", c)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	want := Config{Provider: "openai", Model: "gpt-4o-mini", BaseURL: "https://x.example"}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func TestSave_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows doesn't honor Unix permission bits — os.WriteFile(…, 0600)
		// reports 0666 via Mode().Perm(). The 0600 intent still applies on the
		// Unix platforms where it's a real access control.
		t.Skip("Unix file permissions not enforced on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := (Config{APIKey: "sk-secret"}).Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(home, ".octo", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// A file that can carry an API key must not be world/group readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestLoad_MalformedIsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".octo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("not: valid: yaml: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("Load() on malformed file = nil, want error")
	}
}
