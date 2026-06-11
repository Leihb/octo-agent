package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/config"
)

// oneEntryConfig builds a Config whose default entry has the given fields —
// the multi-model equivalent of the old top-level provider/model literals.
func oneEntryConfig(e config.ModelEntry) config.Config {
	if e.Name == "" {
		e.Name = "default"
	}
	return config.Config{Models: []config.ModelEntry{e}, DefaultModel: e.Name}
}

func TestResolveBaseURL_Precedence(t *testing.T) {
	// Isolate from host env vars so the test is deterministic.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")

	// env wins over config.
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.deepseek.com/anthropic")
	entry := config.ModelEntry{Provider: providerAnthropic, BaseURL: "https://cfg.example"}
	if got := resolveBaseURL(providerAnthropic, entry); got != "https://api.deepseek.com/anthropic" {
		t.Errorf("env should win, got %q", got)
	}

	// No env → config entry (same provider only).
	t.Setenv("ANTHROPIC_BASE_URL", "")
	if got := resolveBaseURL(providerAnthropic, entry); got != "https://cfg.example" {
		t.Errorf("config base URL = %q, want https://cfg.example", got)
	}
	// Entry base URL must not leak onto a different provider.
	if got := resolveBaseURL(providerOpenAI, entry); got != "" {
		t.Errorf("openai must not inherit anthropic's config base URL, got %q", got)
	}

	// No override anywhere → effectiveEndpoint shows the marked default.
	empty := config.ModelEntry{}
	if got := resolveBaseURL(providerAnthropic, empty); got != "" {
		t.Errorf("no override should be empty, got %q", got)
	}
	if got := effectiveEndpoint(providerAnthropic, empty); !strings.Contains(got, "api.anthropic.com") || !strings.Contains(got, "default") {
		t.Errorf("effectiveEndpoint default = %q, want the marked anthropic default", got)
	}
	// Override flows through to effectiveEndpoint verbatim.
	if got := effectiveEndpoint(providerAnthropic, entry); got != "https://cfg.example" {
		t.Errorf("effectiveEndpoint override = %q, want the config URL unmarked", got)
	}
}

func TestResolveProviderModel_ModelEnv(t *testing.T) {
	// env beats config + default, but the --model flag still beats env.
	t.Setenv("ANTHROPIC_MODEL", "claude-from-env")
	t.Setenv("OPENAI_MODEL", "")
	cfg := oneEntryConfig(config.ModelEntry{Provider: providerAnthropic, Model: "cfg-model"})

	if _, m, _, _ := resolveProviderModel("", "", cfg); m != "claude-from-env" {
		t.Errorf("env should beat config, got %q", m)
	}
	if _, m, _, _ := resolveProviderModel("", "flag-model", cfg); m != "flag-model" {
		t.Errorf("--model flag should beat env, got %q", m)
	}
	// The model env is per-provider: ANTHROPIC_MODEL must not leak onto openai.
	if _, m, _, _ := resolveProviderModel(providerOpenAI, "", config.Config{}); m != "gpt-5.4" {
		t.Errorf("ANTHROPIC_MODEL must not affect openai, got %q (want default)", m)
	}
	// OPENAI_MODEL drives the openai default slot.
	t.Setenv("OPENAI_MODEL", "deepseek-chat")
	if _, m, _, _ := resolveProviderModel(providerOpenAI, "", config.Config{}); m != "deepseek-chat" {
		t.Errorf("OPENAI_MODEL = %q, want deepseek-chat", m)
	}
}

func TestResolveProviderModel_Precedence(t *testing.T) {
	// Isolate from any model env vars in the host so the flag/config/default
	// cases below are deterministic.
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OCTO_PROVIDER", "")
	cfg := oneEntryConfig(config.ModelEntry{Provider: "openai", Model: "cfg-model"})
	tests := []struct {
		name              string
		flagProv, flagMdl string
		cfg               config.Config
		wantProv, wantMdl string
		wantOK            bool
	}{
		{"flag beats config", "anthropic", "flag-model", cfg, "anthropic", "flag-model", true},
		{"config beats default", "", "", cfg, "openai", "cfg-model", true},
		{"flag provider, default model", "openai", "", config.Config{}, "openai", "gpt-5.4", true},
		{"empty everything → anthropic default", "", "", config.Config{}, "anthropic", "claude-sonnet-4-6", true},
		{"config provider, builtin model", "", "", oneEntryConfig(config.ModelEntry{Provider: "openai"}), "openai", "gpt-5.4", true},
		{"flag provider overrides config provider — no model contamination", "anthropic", "", cfg, "anthropic", "claude-sonnet-4-6", true},
		{"unknown provider, no model → not ok", "bogus", "", config.Config{}, "bogus", "", false},
		{"unknown provider WITH model → ok (buildProvider rejects later)", "bogus", "m", config.Config{}, "bogus", "m", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotProv, gotMdl, _, ok := resolveProviderModel(tc.flagProv, tc.flagMdl, tc.cfg)
			if gotProv != tc.wantProv || gotMdl != tc.wantMdl || ok != tc.wantOK {
				t.Errorf("resolveProviderModel(%q,%q,%+v) = (%q,%q,%v), want (%q,%q,%v)",
					tc.flagProv, tc.flagMdl, tc.cfg, gotProv, gotMdl, ok, tc.wantProv, tc.wantMdl, tc.wantOK)
			}
		})
	}
}

func TestResolveProviderModel_EntryNameSelectsWholeEntry(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OCTO_PROVIDER", "")
	cfg := config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6"},
			{Name: "kimi", Provider: "kimi", Model: "kimi-k2.6", BaseURL: "https://kimi.example", APIKey: "sk-kimi"},
		},
		DefaultModel: "main",
	}

	prov, model, entry, ok := resolveProviderModel("", "kimi", cfg)
	if !ok || prov != "kimi" || model != "kimi-k2.6" {
		t.Fatalf("named entry: got (%q,%q,%v), want (kimi, kimi-k2.6, true)", prov, model, ok)
	}
	if entry.BaseURL != "https://kimi.example" || entry.APIKey != "sk-kimi" {
		t.Errorf("entry should carry base URL and key: %+v", entry)
	}
	// The entry wins over an explicit --provider — the combination is
	// meaningless and the entry is what the user named.
	if p, _, _, _ := resolveProviderModel("anthropic", "kimi", cfg); p != "kimi" {
		t.Errorf("entry name should override --provider, got %q", p)
	}
	// A non-matching --model value stays a raw model string on the default entry.
	if p, m, _, _ := resolveProviderModel("", "some-model", cfg); p != "anthropic" || m != "some-model" {
		t.Errorf("raw model string: got (%q,%q)", p, m)
	}
}

func TestResolveProviderModel_ProviderEnv(t *testing.T) {
	// OCTO_PROVIDER beats config and default, but --provider flag still beats env.
	t.Setenv("OCTO_PROVIDER", "openai")
	t.Setenv("OPENAI_MODEL", "")
	cfg := oneEntryConfig(config.ModelEntry{Provider: providerAnthropic, Model: "cfg-model"})

	if p, _, _, _ := resolveProviderModel("", "", cfg); p != "openai" {
		t.Errorf("OCTO_PROVIDER should beat config, got %q", p)
	}
	if p, _, _, _ := resolveProviderModel("anthropic", "", cfg); p != "anthropic" {
		t.Errorf("--provider flag should beat OCTO_PROVIDER, got %q", p)
	}
	// When OCTO_PROVIDER selects a provider, its model env var is read.
	t.Setenv("OPENAI_MODEL", "deepseek-chat")
	if _, m, _, _ := resolveProviderModel("", "", config.Config{}); m != "deepseek-chat" {
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
	if !strings.Contains(stdout.String(), "config.yml") {
		t.Errorf("path output should mention config.yml; got %q", stdout.String())
	}
}

func TestRunConfig_Wizard_WritesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "set-so-wizard-skips-key-prompt")

	// Answers: provider=openai, model=(default). openai is pinned to its
	// default endpoint, so the wizard no longer asks for a base URL.
	in := strings.NewReader("openai\n\n")
	var stdout, stderr bytes.Buffer
	if code := runConfig(nil, in, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load after wizard: %v", err)
	}
	entry := got.DefaultEntry()
	if entry.Provider != "openai" {
		t.Errorf("provider = %q, want openai", entry.Provider)
	}
	// Empty model answer keeps it unset so the built-in default applies later.
	if entry.Model != "" {
		t.Errorf("model = %q, want empty (use built-in default)", entry.Model)
	}
	if entry.APIKey != "" {
		t.Errorf("APIKey should not be stored when env var is set; got %q", entry.APIKey)
	}
	if strings.Contains(stdout.String(), "base URL") || strings.Contains(stdout.String(), "Endpoint") {
		t.Errorf("pinned vendor must not be asked about its endpoint; got:\n%s", stdout.String())
	}
}

func TestRunConfig_Wizard_PreservesOtherEntriesAndGlobals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "set-so-wizard-skips-key-prompt")

	seed := config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6"},
			{Name: "kimi", Provider: "kimi", Model: "kimi-k2.6"},
		},
		DefaultModel:   "main",
		PermissionMode: "strict",
	}
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	// Answers: provider=openai, model=(default). No endpoint question for a
	// pinned vendor.
	in := strings.NewReader("openai\n\n")
	var stdout, stderr bytes.Buffer
	if code := runConfig(nil, in, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load after wizard: %v", err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("wizard must not drop other entries: %+v", got.Models)
	}
	if _, ok := got.EntryByName("kimi"); !ok {
		t.Error("kimi entry lost")
	}
	if got.PermissionMode != "strict" {
		t.Errorf("permission_mode lost: %q", got.PermissionMode)
	}
	if got.DefaultEntry().Provider != "openai" {
		t.Errorf("default entry provider = %q, want openai", got.DefaultEntry().Provider)
	}
}

func TestRunConfig_Show_ReportsSourcesNotKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "secret-value-should-not-print")

	if err := oneEntryConfig(config.ModelEntry{Provider: "anthropic", Model: "m1"}).Save(); err != nil {
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
	if err := oneEntryConfig(config.ModelEntry{Provider: "anthropic", APIKey: "old-anthropic-key"}).Save(); err != nil {
		t.Fatal(err)
	}

	// Answers: provider=openai, model=(default), coauthor=y,
	// reasoning-effort=(off), show-reasoning=(default), store_key=y,
	// key=new-openai-key. No endpoint question for a pinned vendor.
	in := strings.NewReader("openai\n\ny\n\n\ny\nnew-openai-key\n")
	var stdout, stderr bytes.Buffer
	if code := runConfig(nil, in, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load after wizard: %v", err)
	}
	entry := got.DefaultEntry()
	if entry.Provider != "openai" {
		t.Errorf("provider = %q, want openai", entry.Provider)
	}
	if entry.APIKey != "new-openai-key" {
		t.Errorf("APIKey = %q, want new-openai-key", entry.APIKey)
	}
}

func TestRunConfig_Wizard_CompatibleProviderRequiresModelAndBaseURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "set-so-wizard-skips-key-prompt")

	// Answers: provider, model, base URL — both free-text fields are required.
	in := strings.NewReader("openai_compatible\ndeepseek-chat\nhttps://gw.example/v1\n")
	var stdout, stderr bytes.Buffer
	if code := runConfig(nil, in, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load after wizard: %v", err)
	}
	entry := got.DefaultEntry()
	if entry.Provider != "openai_compatible" || entry.Model != "deepseek-chat" || entry.BaseURL != "https://gw.example/v1" {
		t.Errorf("entry = %+v, want openai_compatible/deepseek-chat/https://gw.example/v1", entry)
	}

	// An empty base URL is a hard error, not a silent default.
	in = strings.NewReader("openai_compatible\ndeepseek-chat\n\n")
	stdout.Reset()
	stderr.Reset()
	// Wipe the entry so its stored URL doesn't become the press-Enter default.
	if err := (config.Config{}).Save(); err != nil {
		t.Fatal(err)
	}
	if code := runConfig(nil, in, &stdout, &stderr); code != 2 {
		t.Errorf("empty base URL: exit = %d, want 2 (stderr=%q)", code, stderr.String())
	}

	// An empty model is equally a hard error.
	in = strings.NewReader("openai_compatible\n\nhttps://gw.example/v1\n")
	if code := runConfig(nil, in, &stdout, &stderr); code != 2 {
		t.Errorf("empty model: exit = %d, want 2 (stderr=%q)", code, stderr.String())
	}
}

func TestRunConfig_Wizard_PinnedVendorRejectsForeignEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MOONSHOT_API_KEY", "set-so-wizard-skips-key-prompt")

	// kimi has regional variants: a variant URL is accepted…
	in := strings.NewReader("kimi\n\nhttps://api.moonshot.ai\n")
	var stdout, stderr bytes.Buffer
	if code := runConfig(nil, in, &stdout, &stderr); code != 0 {
		t.Fatalf("variant URL: exit = %d, stderr=%q", code, stderr.String())
	}
	got, _ := config.Load()
	if got.DefaultEntry().BaseURL != "https://api.moonshot.ai" {
		t.Errorf("base URL = %q, want the picked variant", got.DefaultEntry().BaseURL)
	}

	// …but an arbitrary URL is rejected with a pointer to the catch-alls.
	in = strings.NewReader("kimi\n\nhttps://evil.example\n")
	stdout.Reset()
	stderr.Reset()
	if code := runConfig(nil, in, &stdout, &stderr); code != 2 {
		t.Errorf("foreign URL: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "openai_compatible") {
		t.Errorf("error should point at the compatible catch-alls; got %q", stderr.String())
	}
}

func TestRunConfig_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}
