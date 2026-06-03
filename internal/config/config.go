// Package config holds the user's persisted CLI defaults at
// ~/.octo/config.yaml — the provider/model/base-URL a fresh `octo chat`
// should use without re-typing flags or re-exporting env vars every session.
//
// Precedence is resolved by the caller (cmd/octo): an explicit CLI flag beats
// this file, which beats the built-in default. API keys are read from the
// environment first; storing one here is opt-in and discouraged (it lands in
// plaintext, mode 0600), so callers fall back to Config.APIKey only when the
// matching env var is empty.
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the persisted set of CLI defaults. Every field is optional; a
// missing file (or a missing field) leaves the zero value, and the caller
// substitutes its built-in default.
type Config struct {
	Provider       string `yaml:"provider,omitempty"`
	Model          string `yaml:"model,omitempty"`
	BaseURL        string `yaml:"base_url,omitempty"`
	PermissionMode string `yaml:"permission_mode,omitempty"`
	// Coauthor, when true, instructs the agent to append a Co-authored-by line
	// to every git commit message it writes. Default is true (enabled).
	Coauthor *bool `yaml:"coauthor,omitempty"`
	// ShowReasoning controls whether a reasoning model's thinking trace is
	// streamed to the terminal (dimmed). Covers both Anthropic thinking blocks
	// and OpenAI reasoning_content. nil means the built-in default (enabled).
	ShowReasoning *bool `yaml:"show_reasoning,omitempty"`
	// ReasoningEffort sets the unified reasoning intensity ("low" | "medium" |
	// "high"). OpenAI sends it as reasoning_effort; Anthropic maps it to an
	// extended-thinking token budget. Empty means off (no extended reasoning).
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
	// APIKey, when set, is a plaintext fallback used only if the provider's
	// env var is empty. Opt-in via `octo config` and stored mode 0600. Prefer
	// the env var.
	APIKey string `yaml:"api_key,omitempty"`
}

// Path returns the absolute path to the config file (~/.octo/config.yaml).
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".octo", "config.yaml"), nil
}

// Load reads ~/.octo/config.yaml. A missing file is not an error — it returns
// the zero Config so first-run callers need no special-casing. A present but
// malformed file IS an error, so a typo surfaces instead of silently
// reverting to defaults.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes the config to ~/.octo/config.yaml with mode 0600 (it may hold an
// API key), creating ~/.octo if needed.
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
