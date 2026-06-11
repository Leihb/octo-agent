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
// set on the CLI". A --model value that names a config entry selects the whole
// entry — provider, model, base URL, and key all come from it (overriding even
// an explicit --provider, which can't meaningfully combine with a named
// entry). The returned entry is the config entry the resolution anchored on,
// so base-URL/key/reasoning lookups stay on the same entry. ok is false when
// the resolved provider has no known default model (i.e. an unknown provider
// with no explicit model) — the caller prints an error and exits.
func resolveProviderModel(flagProvider, flagModel string, cfg config.Config) (provider, model string, entry config.ModelEntry, ok bool) {
	if e, found := cfg.EntryByName(flagModel); found {
		provider = firstNonEmpty(e.Provider, app.ProviderAnthropic)
		model = firstNonEmpty(e.Model, defaultModels[provider])
		return provider, model, e, model != ""
	}

	entry = cfg.DefaultEntry()
	provider = firstNonEmpty(flagProvider, os.Getenv("OCTO_PROVIDER"), entry.Provider, app.ProviderAnthropic)
	// Precedence: --model flag > env (ANTHROPIC_MODEL / OPENAI_MODEL) > config
	// (same-provider only) > built-in default. The env tier mirrors how the
	// base URL and key resolve env-first, so a third-party Claude-/OpenAI-
	// compatible endpoint can be configured entirely through env vars (e.g.
	// ANTHROPIC_BASE_URL + ANTHROPIC_API_KEY + ANTHROPIC_MODEL).
	model = flagModel
	if model == "" {
		model = modelFromEnv(provider)
	}
	// The entry's model is specific to the entry's provider — only honor it
	// when the resolved provider matches, so `--provider <other>` doesn't carry
	// one vendor's model onto another.
	if model == "" && provider == entry.Provider {
		model = entry.Model
	}
	if model == "" {
		model = defaultModels[provider]
	}
	return provider, model, entry, model != ""
}

// providerBaseURL returns the entry's base URL only when the entry targets the
// same provider — a base URL is endpoint-specific to its provider, so it must
// not leak onto a different one selected via --provider.
func providerBaseURL(provider string, entry config.ModelEntry) string {
	if entry.Provider == provider {
		return entry.BaseURL
	}
	return ""
}

// modelFromEnv returns the per-provider model env override, or "" if unset.
func modelFromEnv(provider string) string {
	envVar := strings.ToUpper(provider) + "_MODEL"
	return os.Getenv(envVar)
}

// resolveBaseURL returns the base-URL override for the provider, env-first
// (<PROVIDER>_BASE_URL) then the resolved config entry. "" means no
// override — the provider uses its built-in default endpoint.
func resolveBaseURL(provider string, entry config.ModelEntry) string {
	envVar := strings.ToUpper(provider) + "_BASE_URL"
	return firstNonEmpty(os.Getenv(envVar), providerBaseURL(provider, entry))
}

// effectiveEndpoint is resolveBaseURL for display: it substitutes the
// provider's built-in default (marked) when there's no override, so a verbose
// run always shows the host that will actually be called.
func effectiveEndpoint(provider string, entry config.ModelEntry) string {
	if u := resolveBaseURL(provider, entry); u != "" {
		return u
	}
	if def := app.DefaultBaseURL(provider); def != "" {
		return def + " (default)"
	}
	return "(default)"
}

// runConfig handles `octo config [show|path]` and, with no subcommand, an
// interactive setup wizard that writes ~/.octo/config.yml.
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
	entry := cfg.DefaultEntry()

	provider := firstNonEmpty(os.Getenv("OCTO_PROVIDER"), entry.Provider, providerAnthropic)
	provSrc := "default"
	if envProv := os.Getenv("OCTO_PROVIDER"); envProv != "" {
		provSrc = "env"
	} else if entry.Provider != "" {
		provSrc = "config"
	}
	model := firstNonEmpty(entry.Model, defaultModels[provider])
	modelSrc := "default"
	if entry.Model != "" {
		modelSrc = "config"
	}

	fmt.Fprintf(stdout, "Config file: %s\n", path)
	if _, statErr := os.Stat(path); statErr != nil {
		fmt.Fprintln(stdout, "  (not created yet — run `octo config` to set it up)")
	}
	fmt.Fprintln(stdout)
	if len(cfg.Models) > 1 {
		fmt.Fprintf(stdout, "  models:    %d configured, default %q (others: %s)\n",
			len(cfg.Models), entry.Name, otherEntryNames(cfg, entry.Name))
	}
	fmt.Fprintf(stdout, "  provider:  %s (%s)\n", provider, provSrc)
	fmt.Fprintf(stdout, "  model:     %s (%s)\n", model, modelSrc)
	if entry.BaseURL != "" {
		fmt.Fprintf(stdout, "  base URL:  %s (config)\n", entry.BaseURL)
	} else {
		fmt.Fprintln(stdout, "  base URL:  (provider default)")
	}
	fmt.Fprintf(stdout, "  API key:   %s\n", apiKeyStatus(provider, entry))
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
	if entry.ReasoningEffort != "" {
		effortStatus = entry.ReasoningEffort + " (config)"
	}
	fmt.Fprintf(stdout, "  reasoning: %s\n", effortStatus)
	showStatus := "on (default)"
	if entry.ShowReasoning != nil {
		if *entry.ShowReasoning {
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

// otherEntryNames lists the configured entry names besides skip, for display.
func otherEntryNames(cfg config.Config, skip string) string {
	names := make([]string, 0, len(cfg.Models))
	for _, e := range cfg.Models {
		if e.Name != skip {
			names = append(names, e.Name)
		}
	}
	return strings.Join(names, ", ")
}

// apiKeyStatus reports where a key for the given provider would come from,
// without revealing it. Env always wins over a config-stored key.
func apiKeyStatus(provider string, entry config.ModelEntry) string {
	envVar := app.VendorAPIKeyEnvVar(provider)
	if envVar == "" {
		envVar = strings.ToUpper(provider) + "_API_KEY"
	}
	if os.Getenv(envVar) != "" {
		return "set via $" + envVar
	}
	if entry.APIKey != "" && entry.Provider == provider {
		return "stored in config (mode 0600)"
	}
	return "not set — export $" + envVar
}

// runConfigWizard prompts for the persisted defaults and writes the file.
// Existing values are offered as the default for each prompt so re-running it
// edits rather than resets.
func runConfigWizard(stdin io.Reader, stdout, stderr io.Writer) int {
	full, _ := config.Load() // a malformed file is treated as empty here — the wizard overwrites it
	existing := full.DefaultEntry()

	// An arrow-key menu needs an editable terminal on both ends. When stdin is
	// piped, the output isn't a TTY, or readline declines (Windows), fall back
	// to the typed-answer flow so scripts and tests keep working unchanged.
	tty := stdinIsTTY(stdin) && writerIsTTY(stdout)

	var reader lineReader
	if tty {
		if rl, err := newReadlineReader(defaultHistoryFile()); err == nil {
			reader = rl
		} else {
			tty = false
		}
	}
	if reader == nil {
		reader = newScannerLineReader(stdin, stdout)
	}
	defer reader.Close()

	fmt.Fprintln(stdout, "octo config — set your default provider and model (~/.octo/config.yml).")
	if tty {
		fmt.Fprintln(stdout, "Use ↑/↓ to choose, Enter to confirm. CLI flags and env vars still override per run.")
	} else {
		fmt.Fprintln(stdout, "Press Enter to keep the shown default. CLI flags and env vars still override per run.")
	}
	fmt.Fprintln(stdout)

	// Provider.
	var provider string
	if tty {
		items := make([]selectItem, len(app.Registry))
		for i, v := range app.Registry {
			items[i] = selectItem{label: v.DisplayName, desc: v.ID, value: v.ID}
		}
		choice, ok := runSelect(stdin, stdout, "Provider", items, firstNonEmpty(existing.Provider, app.ProviderAnthropic))
		if !ok {
			return cancelWizard(stderr)
		}
		provider = choice.value
		fmt.Fprintf(stdout, "Provider: %s (%s)\n\n", app.VendorDisplayName(provider), provider)
	} else {
		provider = strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
			"Provider (anthropic | openai | kimi | deepseek | ...)", firstNonEmpty(existing.Provider, app.ProviderAnthropic))))
		if !app.IsKnownVendor(provider) {
			fmt.Fprintf(stderr, "octo config: unknown provider %q\n", provider)
			return 2
		}
	}

	// sameProvider gates reuse of the existing entry's model/base URL — values
	// stored for one vendor must not seed prompts for a different one.
	sameProvider := existing.Provider == provider

	// Model. Compatible (custom-endpoint) vendors have no catalogue or default,
	// so the model is a required free-text answer.
	var model string
	if app.VendorCustomEndpoint(provider) {
		def := ""
		if sameProvider {
			def = existing.Model
		}
		model = strings.TrimSpace(promptDefault(reader, stdout, "Model (required)", def))
		if model == "" {
			fmt.Fprintf(stderr, "octo config: provider %q has no default model — enter one\n", provider)
			return 2
		}
		if tty {
			fmt.Fprintf(stdout, "Model: %s\n\n", model)
		}
	} else if tty {
		def := defaultModels[provider]
		startVal := def
		if sameProvider && existing.Model != "" {
			startVal = existing.Model
		}
		items := buildModelItems(app.VendorModels(provider), def, startVal)
		items = append(items, selectItem{label: "Custom model…", desc: "enter a model id", value: customSentinel})
		choice, ok := runSelect(stdin, stdout, "Model", items, startVal)
		if !ok {
			return cancelWizard(stderr)
		}
		if choice.value == customSentinel {
			model = strings.TrimSpace(promptDefault(reader, stdout, "Model", startVal))
		} else {
			model = choice.value
		}
		shown := model
		if shown == "" || shown == def {
			shown = def + " (default)"
		}
		fmt.Fprintf(stdout, "Model: %s\n\n", shown)
	} else {
		model = strings.TrimSpace(promptDefault(reader, stdout, "Model", firstNonEmpty(existing.Model, defaultModels[provider])))
	}
	// Accepting the provider's built-in default leaves Model unset so it floats
	// with future releases (and never contaminates a different --provider).
	if model == defaultModels[provider] {
		model = ""
	}

	// Base URL / endpoint. Compatible (custom-endpoint) vendors require a
	// free-text URL; vendors with regional variants get a fixed menu; everyone
	// else is pinned to the default endpoint — no question asked. A legacy
	// custom URL already stored for the same vendor is preserved as-is.
	baseURL := ""
	if sameProvider {
		baseURL = existing.BaseURL
	}
	variants := app.VendorEndpointVariants(provider)
	switch {
	case app.VendorCustomEndpoint(provider):
		baseURL = strings.TrimSpace(promptDefault(reader, stdout, "Base URL (required)", baseURL))
		if baseURL == "" {
			fmt.Fprintf(stderr, "octo config: provider %q requires a base URL\n", provider)
			return 2
		}
		if tty {
			fmt.Fprintf(stdout, "Endpoint: %s\n\n", baseURL)
		}
	case tty && len(variants) > 0:
		items := []selectItem{{label: "Default endpoint", desc: app.DefaultBaseURL(provider), value: ""}}
		for _, v := range variants {
			items = append(items, selectItem{label: v.Label, desc: v.BaseURL, value: v.BaseURL})
		}
		choice, ok := runSelect(stdin, stdout, "Endpoint", items, baseURL)
		if !ok {
			return cancelWizard(stderr)
		}
		baseURL = choice.value
		if baseURL == "" {
			fmt.Fprintf(stdout, "Endpoint: default\n\n")
		} else {
			fmt.Fprintf(stdout, "Endpoint: %s\n\n", baseURL)
		}
	case len(variants) > 0:
		// A stale value that is no longer a vendor endpoint must not become the
		// press-Enter default — it would fail validation below.
		def := ""
		if app.VendorByBaseURL(baseURL) == provider {
			def = baseURL
		}
		ans := strings.TrimSpace(promptDefault(reader, stdout, "Endpoint URL (empty = default)", def))
		if ans != "" && app.VendorByBaseURL(ans) != provider {
			fmt.Fprintf(stderr, "octo config: %q is not an endpoint of %s — for a custom endpoint use the %s or %s provider\n",
				ans, app.VendorDisplayName(provider), app.ProviderOpenAICompatible, app.ProviderAnthropicCompatible)
			return 2
		}
		baseURL = ans
	}

	// The wizard edits the default entry in place; other entries and global
	// settings (permission mode, tools, …) pass through untouched.
	outEntry := existing
	outEntry.Provider = provider
	outEntry.Model = model
	outEntry.BaseURL = baseURL

	// Co-authored-by: default on; ask once in wizard.
	coauthorDefault := full.Coauthor == nil || *full.Coauthor
	coauthorVal, ok := pickYesNo(tty, reader, stdin, stdout,
		"Append Co-authored-by to git commits?", coauthorDefault)
	if !ok {
		return cancelWizard(stderr)
	}
	full.Coauthor = &coauthorVal

	// Reasoning effort: off (empty) by default; offer the existing value.
	if tty {
		choice, ok := runSelect(stdin, stdout, "Reasoning effort", []selectItem{
			{label: "Off", value: ""},
			{label: "Low", value: "low"},
			{label: "Medium", value: "medium"},
			{label: "High", value: "high"},
		}, existing.ReasoningEffort)
		if !ok {
			return cancelWizard(stderr)
		}
		outEntry.ReasoningEffort = choice.value
	} else {
		effortAns := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
			"Reasoning effort (low | medium | high, empty = off)", existing.ReasoningEffort)))
		if !validReasoningEffort(effortAns) {
			fmt.Fprintf(stderr, "octo config: invalid reasoning effort %q (use 'low', 'medium', 'high', or empty)\n", effortAns)
			return 2
		}
		outEntry.ReasoningEffort = effortAns
	}

	// Show the reasoning/thinking trace: default on.
	showDefault := existing.ShowReasoning == nil || *existing.ShowReasoning
	showVal, ok := pickYesNo(tty, reader, stdin, stdout,
		"Show the reasoning/thinking trace while streaming?", showDefault)
	if !ok {
		return cancelWizard(stderr)
	}
	outEntry.ShowReasoning = &showVal

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
	if needsKeyPrompt {
		outEntry.APIKey = ""
		ans := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout,
			"Store the API key in this file? Not recommended — prefer "+envVar+" (y/N)", "n")))
		if ans == "y" || ans == "yes" {
			key := strings.TrimSpace(promptDefault(reader, stdout, "API key", ""))
			outEntry.APIKey = key
		}
	}

	full.SetDefaultEntry(outEntry)
	if err := full.Save(); err != nil {
		fmt.Fprintf(stderr, "octo config: %v\n", err)
		return 1
	}
	path, _ := config.Path()
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Saved %s\n", path)
	if outEntry.APIKey == "" && os.Getenv(envVar) == "" {
		fmt.Fprintf(stdout, "Next: export %s=... (or re-run `octo config` to store it), then `octo`.\n", envVar)
	} else {
		fmt.Fprintln(stdout, "Run `octo` to start.")
	}
	return 0
}

// customSentinel is the menu value standing in for "let me type my own". It
// uses a NUL byte so it can never collide with a real model id or URL.
const customSentinel = "\x00custom"

// cancelWizard reports the wizard was aborted (Esc/Ctrl-C at a menu) and
// returns the process exit code for that.
func cancelWizard(stderr io.Writer) int {
	fmt.Fprintln(stderr, "octo config: cancelled, nothing saved.")
	return 1
}

// buildModelItems turns a vendor's model catalogue into menu rows, marking the
// built-in default and folding in the user's current pick when it isn't part
// of the catalogue (so re-running the wizard shows it pre-selected).
func buildModelItems(models []string, def, current string) []selectItem {
	items := make([]selectItem, 0, len(models)+1)
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		seen[m] = true
		desc := ""
		if m == def {
			desc = "default"
		}
		items = append(items, selectItem{label: m, desc: desc, value: m})
	}
	if current != "" && current != def && !seen[current] {
		items = append(items, selectItem{label: current, desc: "current", value: current})
	}
	return items
}

// pickYesNo asks a boolean question: an arrow-key Yes/No menu on a TTY, the
// typed "(Y/n)" prompt otherwise. ok is false only when a TTY menu is
// cancelled.
func pickYesNo(tty bool, reader lineReader, stdin io.Reader, stdout io.Writer, prompt string, def bool) (val, ok bool) {
	defVal := "n"
	if def {
		defVal = "y"
	}
	if tty {
		choice, ok := runSelect(stdin, stdout, prompt, []selectItem{
			{label: "Yes", value: "y"},
			{label: "No", value: "n"},
		}, defVal)
		if !ok {
			return false, false
		}
		return choice.value == "y", true
	}
	ans := strings.ToLower(strings.TrimSpace(promptDefault(reader, stdout, prompt+" (Y/n)", defVal)))
	return ans != "n" && ans != "no", true
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
