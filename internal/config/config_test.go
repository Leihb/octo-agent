package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	return home
}

func writeOcto(t *testing.T, home, name, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".octo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_MissingFileIsZeroNotError(t *testing.T) {
	setHome(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() on missing file = %v, want nil", err)
	}
	if len(c.Models) != 0 || c.DefaultModel != "" {
		t.Errorf("Load() on missing file = %+v, want zero Config", c)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	setHome(t)

	want := Config{
		Models: []ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-fable-5"},
			{Name: "kimi", Provider: "kimi", Model: "kimi-k2.6", BaseURL: "https://x.example"},
		},
		DefaultModel: "kimi",
		LiteModel:    "main",
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Models) != 2 || got.Models[1] != want.Models[1] {
		t.Errorf("round-trip models = %+v, want %+v", got.Models, want.Models)
	}
	if got.DefaultModel != "kimi" || got.LiteModel != "main" {
		t.Errorf("round-trip refs = default %q lite %q", got.DefaultModel, got.LiteModel)
	}
	if e := got.DefaultEntry(); e.Name != "kimi" || e.BaseURL != "https://x.example" {
		t.Errorf("DefaultEntry = %+v, want kimi entry", e)
	}
}

func TestLoad_LegacyYAMLIsNormalised(t *testing.T) {
	home := setHome(t)
	writeOcto(t, home, "config.yaml",
		"provider: openai\nmodel: gpt-4o-mini\nbase_url: https://x.example\napi_key: sk-old\nreasoning_effort: high\npermission_mode: strict\n")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Models) != 1 {
		t.Fatalf("Models = %+v, want one synthesized entry", c.Models)
	}
	e := c.Models[0]
	if e.Name != "default" || e.Provider != "openai" || e.Model != "gpt-4o-mini" ||
		e.BaseURL != "https://x.example" || e.APIKey != "sk-old" || e.ReasoningEffort != "high" {
		t.Errorf("synthesized entry = %+v", e)
	}
	if c.DefaultModel != "default" {
		t.Errorf("DefaultModel = %q, want default", c.DefaultModel)
	}
	if c.PermissionMode != "strict" {
		t.Errorf("global PermissionMode lost: %q", c.PermissionMode)
	}
}

func TestLoad_NewFileShadowsLegacy(t *testing.T) {
	home := setHome(t)
	writeOcto(t, home, "config.yaml", "model: old-model\nprovider: openai\n")
	writeOcto(t, home, "config.yml",
		"models:\n  - name: main\n    provider: anthropic\n    model: new-model\ndefault_model: main\n")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e := c.DefaultEntry(); e.Model != "new-model" {
		t.Errorf("DefaultEntry().Model = %q, want new-model (config.yml must win)", e.Model)
	}
}

func TestSave_MigratesLegacyToBak(t *testing.T) {
	home := setHome(t)
	legacy := writeOcto(t, home, "config.yaml", "model: old-model\nprovider: openai\n")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy config.yaml still present after Save")
	}
	if _, err := os.Stat(legacy + ".bak"); err != nil {
		t.Errorf("config.yaml.bak missing: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if e := got.DefaultEntry(); e.Model != "old-model" || e.Provider != "openai" {
		t.Errorf("migrated entry = %+v", e)
	}
}

func TestSave_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows doesn't honor Unix permission bits — os.WriteFile(…, 0600)
		// reports 0666 via Mode().Perm(). The 0600 intent still applies on the
		// Unix platforms where it's a real access control.
		t.Skip("Unix file permissions not enforced on Windows")
	}
	home := setHome(t)

	cfg := Config{Models: []ModelEntry{{Name: "main", APIKey: "sk-secret"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".octo", "config.yml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// A file that can carry an API key must not be world/group readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestLoad_MalformedIsError(t *testing.T) {
	home := setHome(t)
	writeOcto(t, home, "config.yml", "not: valid: yaml: [")
	if _, err := Load(); err == nil {
		t.Error("Load() on malformed file = nil, want error")
	}
}

func TestSetDefaultEntry(t *testing.T) {
	var c Config

	// Appends when empty.
	c.SetDefaultEntry(ModelEntry{Name: "main", Model: "m1"})
	if len(c.Models) != 1 || c.DefaultModel != "main" {
		t.Fatalf("after first set: %+v", c)
	}

	// Replaces in place, including a rename, and keeps the lite reference.
	c.LiteModel = "main"
	c.SetDefaultEntry(ModelEntry{Name: "primary", Model: "m2"})
	if len(c.Models) != 1 || c.Models[0].Name != "primary" || c.Models[0].Model != "m2" {
		t.Fatalf("after rename: %+v", c.Models)
	}
	if c.DefaultModel != "primary" || c.LiteModel != "primary" {
		t.Errorf("references not updated: default %q lite %q", c.DefaultModel, c.LiteModel)
	}

	// Names an unnamed entry "default".
	var c2 Config
	c2.SetDefaultEntry(ModelEntry{Model: "m3"})
	if c2.Models[0].Name != "default" || c2.DefaultModel != "default" {
		t.Errorf("unnamed entry: %+v", c2)
	}
}

func TestUniqueName(t *testing.T) {
	c := Config{Models: []ModelEntry{{Name: "kimi-k2.6"}, {Name: "kimi-k2.6-2"}}}
	if got := c.UniqueName("kimi-k2.6"); got != "kimi-k2.6-3" {
		t.Errorf("UniqueName = %q, want kimi-k2.6-3", got)
	}
	if got := c.UniqueName("fresh"); got != "fresh" {
		t.Errorf("UniqueName = %q, want fresh", got)
	}
	if got := c.UniqueName(" "); got != "model" {
		t.Errorf("UniqueName(blank) = %q, want model", got)
	}
}

func TestEntryByName_EmptyNeverMatches(t *testing.T) {
	c := Config{Models: []ModelEntry{{Name: "", Model: "m"}}}
	if _, ok := c.EntryByName(""); ok {
		t.Error("EntryByName(\"\") matched, want no match")
	}
}
