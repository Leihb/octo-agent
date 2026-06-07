package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/config"
)

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveProviderModel applies precedence flag > env > config file > built-in
// default to the provider and model. An empty flagProvider/flagModel means "not
// set on the CLI". ok is false when the resolved provider has no known default
// model (i.e. an unknown provider with no explicit model) — the caller prints
// an error and exits.
func resolveProviderModel(flagProvider, flagModel string, cfg config.Config) (provider, model string, ok bool) {
	provider = firstNonEmpty(flagProvider, os.Getenv("OCTO_PROVIDER"), cfg.Provider, app.ProviderAnthropic)
	// Precedence: --model flag > env (ANTHROPIC_MODEL / OPENAI_MODEL) > config
	// (same-provider only) > built-in default. The env tier mirrors how the
	// base URL and key resolve env-first, so a third-party Claude-/OpenAI-
	// compatible endpoint can be configured entirely through env vars (e.g.
	// ANTHROPIC_BASE_URL + ANTHROPIC_API_KEY + ANTHROPIC_MODEL).
	model = flagModel
	if model == "" {
		model = modelFromEnv(provider)
	}
	// The config's model is specific to the config's provider — only honor it
	// when the resolved provider matches, so `--provider <other>` doesn't carry
	// one vendor's model onto another.
	if model == "" && provider == cfg.Provider {
		model = cfg.Model
	}
	if model == "" {
		model = defaultModels[provider]
	}
	return provider, model, model != ""
}

// providerBaseURL returns the config's base URL only when the stored config
// targets the same provider — a base URL is endpoint-specific to its provider,
// so it must not leak onto a different one selected via --provider.
func providerBaseURL(provider string, cfg config.Config) string {
	if cfg.Provider == provider {
		return cfg.BaseURL
	}
	return ""
}

// modelFromEnv returns the per-provider model env override, or "" if unset.
func modelFromEnv(provider string) string {
	envVar := strings.ToUpper(provider) + "_MODEL"
	return os.Getenv(envVar)
}

// resolveBaseURL returns the base-URL override for the provider, env-first
// (<PROVIDER>_BASE_URL) then the persisted config. "" means no
// override — the provider uses its built-in default endpoint.
func resolveBaseURL(provider string, cfg config.Config) string {
	envVar := strings.ToUpper(provider) + "_BASE_URL"
	return firstNonEmpty(os.Getenv(envVar), providerBaseURL(provider, cfg))
}

// effectiveEndpoint is resolveBaseURL for display: it substitutes the
// provider's built-in default (marked) when there's no override, so a verbose
// run always shows the host that will actually be called.
func effectiveEndpoint(provider string, cfg config.Config) string {
	if u := resolveBaseURL(provider, cfg); u != "" {
		return u
	}
	if def := app.DefaultBaseURL(provider); def != "" {
		return def + " (default)"
	}
	return "(default)"
}

// runConfig handles `octo config [show|path]` and, with no subcommand, an
// interactive setup wizard that writes ~/.octo/config.yaml.
func runConfig(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "setup", "init":
		return runConfigWizard(stdin, stdout, stderr)
	case "show", "get":
		return runConfigShow(stdout, stderr)
	case "path":
		path, err := config.Path()
		if err != nil {
			fmt.Fprintf(stderr, "octo config: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, path)
		return 0
	default:
		fmt.Fprintf(stderr, "octo config: unknown subcommand %q (use: setup | show | path)\n", sub)
		return 2
	}
}

// runConfigShow prints the effective provider/model/base-URL and where each
// value comes from (CLI flags aside — those are per-invocation). It never
// prints the API key, only whether one is reachable.
func runConfigShow(stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo config: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo config` to rewrite it.")
		return 1
	}
	path, _ := config.Path()

	provider := firstNonEmpty(os.Getenv("OCTO_PROVIDER"), cfg.Provider, providerAnthropic)
	provSrc := "default"
	if envProv := os.Getenv("OCTO_PROVIDER"); envProv != "" {
		provSrc = "env"
	} else if cfg.Provider != "" {
		provSrc = "config"
	}
	model := firstNonEmpty(cfg.Model, defaultModels[provider])
	modelSrc := "default"
	if cfg.Model != "" {
		modelSrc = "config"
	}

	fmt.Fprintf(stdout, "Config file: %s\n", path)
	if _, statErr := os.Stat(path); statErr != nil {
		fmt.Fprintln(stdout, "  (not created yet — run `octo config` to set it up)")
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  provider:  %s (%s)\n", provider, provSrc)
	fmt.Fprintf(stdout, "  model:     %s (%s)\n", model, modelSrc)
	if cfg.BaseURL != "" {
		fmt.Fprintf(stdout, "  base URL:  %s (config)\n", cfg.BaseURL)
	} else {
		fmt.Fprintln(stdout, "  base URL:  (provider default)")
	}
	fmt.Fprintf(stdout, "  API key:   %s\n", apiKeyStatus(provider, cfg))
	coauthorStatus := "on (default)"
	if cfg.Coauthor != nil {
		if *cfg.Coauthor {
			coauthorStatus = "on (config)"
		} else {
			coauthorStatus = "off (config)"
		}
	}
	fmt.Fprintf(stdout, "  coauthor:  %s\n", coauthorStatus)
	effortStatus := "off (default)"
	if cfg.ReasoningEffort != "" {
		effortStatus = cfg.ReasoningEffort + " (config)"
	}
	fmt.Fprintf(stdout, "  reasoning: %s\n", effortStatus)
	showStatus := "on (default)"
	if cfg.ShowReasoning != nil {
		if *cfg.ShowReasoning {
			showStatus = "on (config)"
		} else {
			showStatus = "off (config)"
		}
	}
	fmt.Fprintf(stdout, "  show trace: %s\n", showStatus)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "CLI flags (--provider, --model, --system) and env vars override this file per run.")
	return 0
}

// apiKeyStatus reports where a key for the given provider would come from,
// without revealing it. Env always wins over a config-stored key.
func apiKeyStatus(provider string, cfg config.Config) string {
	envVar := app.VendorAPIKeyEnvVar(provider)
	if envVar == "" {
		envVar = strings.ToUpper(provider) + "_API_KEY"
	}
	if os.Getenv(envVar) != "" {
		return "set via $" + envVar
	}
	if cfg.APIKey != "" && cfg.Provider == provider {
		return "stored in config (mode 0600)"
	}
	return "not set — export $" + envVar
}

// runConfigWizard prompts for the persisted defaults and writes the file.
// Existing values are offered as the default for each prompt so re-running it
// edits rather than resets.
func runConfigWizard(stdin io.Reader, stdout, stderr io.Writer) int {
	existing, _ := config.Load() // a malformed file is treated as empty here — the wizard overwrites it

	var reader lineReader
	if stdinIsTTY(stdin) {
		if rl, err := newReadlineReader(defaultHistoryFile()); err == nil {
			reader = rl
		}
	}
	if reader == nil {
		reader = newScannerLineReader(stdin, stdout)
	}
	defer reader.Close()

	fmt.Fprintln(stdout, "octo config — set your default provider and model (~/.octo/config.yaml).")
	fmt.Fprintln(stdout, "Press Enter to keep the shown default. CLI flags and env vars still override per run.")
	fmt.Fprintln(stdout)

	provider := promptDefault(reader, stdout, "Provider (anthropic | openai | kimi | deepseek | ...)", firstNonEmpty(existing.Provider, app.ProviderAnthropic))
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !app.IsKnownVendor(provider) {
		fmt.Fprintf(stderr, "octo config: unknown provider %q\n", provider)
		return 2
	}

	modelDefault := firstNonEmpty(existing.Model, defaultModels[provider])
	model := strings.TrimSpace(promptDefault(reader, stdout, "Model", modelDefault))
	// Accepting the provider's built-in default leaves Model unset so it floats
	// with future releases (and never contaminates a different --provider).
	if model == defaultModels[provider] {
		model = ""
	}

	baseURL := strings.TrimSpace(promptDefault(reader, stdout, "Custom base URL (optional, for DeepSeek/Kimi/etc.)", existing.BaseURL))

	out := config.Config{Provider: provider, Model: model, BaseURL: baseURL, APIKey: existing.APIKey}

	// Co-authored-by: default on; ask once in wizard.
	coauthorDefault := "y"
	if existing.Coauthor != nil && !*existing.Coauthor {
		coauthorDefault = "n"
	}
	coauthorAns := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
		"Append Co-authored-by to git commits? (Y/n)", coauthorDefault)))
	coauthorVal := coauthorAns != "n" && coauthorAns != "no"
	out.Coauthor = &coauthorVal

	// Reasoning effort: empty (off) by default; offer the existing value.
	effortAns := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
		"Reasoning effort (low | medium | high, empty = off)", existing.ReasoningEffort)))
	if !validReasoningEffort(effortAns) {
		fmt.Fprintf(stderr, "octo config: invalid reasoning effort %q (use 'low', 'medium', 'high', or empty)\n", effortAns)
		return 2
	}
	out.ReasoningEffort = effortAns

	// Show the reasoning/thinking trace: default on.
	showDefault := "y"
	if existing.ShowReasoning != nil && !*existing.ShowReasoning {
		showDefault = "n"
	}
	showAns := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
		"Show the reasoning/thinking trace while streaming? (Y/n)", showDefault)))
	showVal := showAns != "n" && showAns != "no"
	out.ShowReasoning = &showVal

	// API key: env is the recommended home for it. Offer to store it only if
	// the env var is empty, and make declining the obvious default.
	envVar := app.VendorAPIKeyEnvVar(provider)
	if envVar == "" {
		envVar = strings.ToUpper(provider) + "_API_KEY"
	}
	// Prompt for key when env is empty AND (no stored key OR switching provider —
	// a key stored for a different provider doesn't help the new one).
	needsKeyPrompt := os.Getenv(envVar) == "" &&
		(existing.APIKey == "" || existing.Provider != provider)
	if !needsKeyPrompt {
		// Preserve the existing key when staying on the same provider and not
		// explicitly re-prompting.
		out.APIKey = existing.APIKey
	} else {
		ans := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
			"Store the API key in this file? Not recommended — prefer "+envVar+" (y/N)", "n")))
		if ans == "y" || ans == "yes" {
			key := strings.TrimSpace(promptDefault(reader, stdout, "API key", ""))
			out.APIKey = key
		}
	}

	if err := out.Save(); err != nil {
		fmt.Fprintf(stderr, "octo config: %v\n", err)
		return 1
	}
	path, _ := config.Path()
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Saved %s\n", path)
	if out.APIKey == "" && os.Getenv(envVar) == "" {
		fmt.Fprintf(stdout, "Next: export %s=... (or re-run `octo config` to store it), then `octo chat`.\n", envVar)
	} else {
		fmt.Fprintln(stdout, "Run `octo chat` to start.")
	}
	return 0
}

// promptDefault asks one question, showing def as the value used on empty input.
func promptDefault(reader lineReader, stdout io.Writer, label, def string) string {
	prompt := label
	if def != "" {
		prompt += " [" + def + "]"
	}
	prompt += ": "
	line, ok := reader.ReadLine(prompt)
	if !ok {
		return def
	}
	if strings.TrimSpace(line) == "" {
		return def
	}
	return line
}
