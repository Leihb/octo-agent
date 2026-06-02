package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/config"
)

func TestResolveBaseURL_Precedence(t *testing.T) {
	// Isolate from host env vars so the test is deterministic.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")

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

func TestResolveProviderModel_ModelEnv(t *testing.T) {
	// env beats config + default, but the --model flag still beats env.
	t.Setenv("ANTHROPIC_MODEL", "claude-from-env")
	t.Setenv("OPENAI_MODEL", "")
	cfg := config.Config{Provider: providerAnthropic, Model: "cfg-model"}

	if _, m, _ := resolveProviderModel("", "", cfg); m != "claude-from-env" {
		t.Errorf("env should beat config, got %q", m)
	}
	if _, m, _ := resolveProviderModel("", "flag-model", cfg); m != "flag-model" {
		t.Errorf("--model flag should beat env, got %q", m)
	}
	// The model env is per-provider: ANTHROPIC_MODEL must not leak onto openai.
	if _, m, _ := resolveProviderModel(providerOpenAI, "", config.Config{}); m != "gpt-4o-mini" {
		t.Errorf("ANTHROPIC_MODEL must not affect openai, got %q (want default)", m)
	}
	// OPENAI_MODEL drives the openai default slot.
	t.Setenv("OPENAI_MODEL", "deepseek-chat")
	if _, m, _ := resolveProviderModel(providerOpenAI, "", config.Config{}); m != "deepseek-chat" {
		t.Errorf("OPENAI_MODEL = %q, want deepseek-chat", m)
	}
}

func TestResolveProviderModel_Precedence(t *testing.T) {
	// Isolate from any model env vars in the host so the flag/config/default
	// cases below are deterministic.
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OCTO_PROVIDER", "")
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

func TestResolveProviderModel_ProviderEnv(t *testing.T) {
	// OCTO_PROVIDER beats config and default, but --provider flag still beats env.
	t.Setenv("OCTO_PROVIDER", "openai")
	t.Setenv("OPENAI_MODEL", "")
	cfg := config.Config{Provider: providerAnthropic, Model: "cfg-model"}

	if p, _, _ := resolveProviderModel("", "", cfg); p != "openai" {
		t.Errorf("OCTO_PROVIDER should beat config, got %q", p)
	}
	if p, _, _ := resolveProviderModel("anthropic", "", cfg); p != "anthropic" {
		t.Errorf("--provider flag should beat OCTO_PROVIDER, got %q", p)
	}
	// When OCTO_PROVIDER selects a provider, its model env var is read.
	t.Setenv("OPENAI_MODEL", "deepseek-chat")
	if _, m, _ := resolveProviderModel("", "", config.Config{}); m != "deepseek-chat" {
		t.Errorf("OCTO_PROVIDER=openai + OPENAI_MODEL should resolve model, got %q", m)
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

func TestRunConfig_Wizard_SwitchesProviderAndPromptsForKey(t *testing.T) {
	// When the stored config targets one provider and the user switches to
	// another, the wizard must prompt for an API key for the NEW provider —
	// the old provider's key is useless for the new one.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	// Start with an anthropic config that has a stored key.
	if err := (config.Config{Provider: "anthropic", APIKey: "old-anthropic-key"}).Save(); err != nil {
		t.Fatal(err)
	}

	// Answers: provider=openai, model=(default), base_url=(blank), coauthor=y, store_key=y, key=new-openai-key.
	in := strings.NewReader("openai\n\n\ny\ny\nnew-openai-key\n")
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
	if got.APIKey != "new-openai-key" {
		t.Errorf("APIKey = %q, want new-openai-key", got.APIKey)
	}
}

func TestRunConfig_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}
