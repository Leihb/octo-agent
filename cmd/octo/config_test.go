package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/config"
)

func TestResolveBaseURL_Precedence(t *testing.T) {
	// env wins over config.
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.deepseek.com/anthropic")
	cfg := config.Config{Provider: providerAnthropic, BaseURL: "https://cfg.example"}
	if got := resolveBaseURL(providerAnthropic, cfg); got != "https://api.deepseek.com/anthropic" {
		t.Errorf("env should win, got %q", got)
	}

	// No env → config (same provider only).
	t.Setenv("ANTHROPIC_BASE_URL", "")
	if got := resolveBaseURL(providerAnthropic, cfg); got != "https://cfg.example" {
		t.Errorf("config base URL = %q, want https://cfg.example", got)
	}
	// Config base URL must not leak onto a different provider.
	if got := resolveBaseURL(providerOpenAI, cfg); got != "" {
		t.Errorf("openai must not inherit anthropic's config base URL, got %q", got)
	}

	// No override anywhere → effectiveEndpoint shows the marked default.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	empty := config.Config{}
	if got := resolveBaseURL(providerAnthropic, empty); got != "" {
		t.Errorf("no override should be empty, got %q", got)
	}
	if got := effectiveEndpoint(providerAnthropic, empty); !strings.Contains(got, "api.anthropic.com") || !strings.Contains(got, "default") {
		t.Errorf("effectiveEndpoint default = %q, want the marked anthropic default", got)
	}
	// Override flows through to effectiveEndpoint verbatim.
	if got := effectiveEndpoint(providerAnthropic, cfg); got != "https://cfg.example" {
		t.Errorf("effectiveEndpoint override = %q, want the config URL unmarked", got)
	}
}

func TestResolveProviderModel_Precedence(t *testing.T) {
	cfg := config.Config{Provider: "openai", Model: "cfg-model"}
	tests := []struct {
		name              string
		flagProv, flagMdl string
		cfg               config.Config
		wantProv, wantMdl string
		wantOK            bool
	}{
		{"flag beats config", "anthropic", "flag-model", cfg, "anthropic", "flag-model", true},
		{"config beats default", "", "", cfg, "openai", "cfg-model", true},
		{"flag provider, default model", "openai", "", config.Config{}, "openai", "gpt-4o-mini", true},
		{"empty everything → anthropic default", "", "", config.Config{}, "anthropic", "claude-haiku-4-5-20251001", true},
		{"config provider, builtin model", "", "", config.Config{Provider: "openai"}, "openai", "gpt-4o-mini", true},
		{"flag provider overrides config provider — no model contamination", "anthropic", "", cfg, "anthropic", "claude-haiku-4-5-20251001", true},
		{"unknown provider, no model → not ok", "bogus", "", config.Config{}, "bogus", "", false},
		{"unknown provider WITH model → ok (buildProvider rejects later)", "bogus", "m", config.Config{}, "bogus", "m", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotProv, gotMdl, ok := resolveProviderModel(tc.flagProv, tc.flagMdl, tc.cfg)
			if gotProv != tc.wantProv || gotMdl != tc.wantMdl || ok != tc.wantOK {
				t.Errorf("resolveProviderModel(%q,%q,%+v) = (%q,%q,%v), want (%q,%q,%v)",
					tc.flagProv, tc.flagMdl, tc.cfg, gotProv, gotMdl, ok, tc.wantProv, tc.wantMdl, tc.wantOK)
			}
		})
	}
}

func TestRunConfig_Path(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"path"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config.json") {
		t.Errorf("path output should mention config.json; got %q", stdout.String())
	}
}

func TestRunConfig_Wizard_WritesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "set-so-wizard-skips-key-prompt")

	// Answers: provider=openai, model=(default), base_url=(blank).
	in := strings.NewReader("openai\n\n\n")
	var stdout, stderr bytes.Buffer
	if code := runConfig(nil, in, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load after wizard: %v", err)
	}
	if got.Provider != "openai" {
		t.Errorf("provider = %q, want openai", got.Provider)
	}
	// Empty model answer keeps it unset so the built-in default applies later.
	if got.Model != "" {
		t.Errorf("model = %q, want empty (use built-in default)", got.Model)
	}
	if got.APIKey != "" {
		t.Errorf("APIKey should not be stored when env var is set; got %q", got.APIKey)
	}
}

func TestRunConfig_Show_ReportsSourcesNotKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "secret-value-should-not-print")

	if err := (config.Config{Provider: "anthropic", Model: "m1"}).Save(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"show"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "secret-value-should-not-print") {
		t.Error("show must never print the API key value")
	}
	if !strings.Contains(out, "anthropic (config)") {
		t.Errorf("show should report provider source; got:\n%s", out)
	}
	if !strings.Contains(out, "ANTHROPIC_API_KEY") {
		t.Errorf("show should report the key is set via env; got:\n%s", out)
	}
}

func TestRunConfig_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}
