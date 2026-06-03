// Package config holds the user's persisted CLI defaults at
// ~/.octo/config.json — the provider/model/base-URL a fresh `octo chat`
// should use without re-typing flags or re-exporting env vars every session.
//
// Precedence is resolved by the caller (cmd/octo): an explicit CLI flag beats
// this file, which beats the built-in default. API keys are read from the
// environment first; storing one here is opt-in and discouraged (it lands in
// plaintext, mode 0600), so callers fall back to Config.APIKey only when the
// matching env var is empty.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is the persisted set of CLI defaults. Every field is optional; a
// missing file (or a missing field) leaves the zero value, and the caller
// substitutes its built-in default.
type Config struct {
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	// Coauthor, when true, instructs the agent to append a Co-authored-by line
	// to every git commit message it writes. Default is true (enabled).
	Coauthor *bool `json:"coauthor,omitempty"`
	// ShowReasoning controls whether a reasoning model's thinking trace is
	// streamed to the terminal (dimmed). Covers both Anthropic thinking blocks
	// and OpenAI reasoning_content. nil means the built-in default (enabled).
	ShowReasoning *bool `json:"show_reasoning,omitempty"`
	// ReasoningEffort sets the unified reasoning intensity ("low" | "medium" |
	// "high"). OpenAI sends it as reasoning_effort; Anthropic maps it to an
	// extended-thinking token budget. Empty means off (no extended reasoning).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// APIKey, when set, is a plaintext fallback used only if the provider's
	// env var is empty. Opt-in via `octo config` and stored mode 0600. Prefer
	// the env var.
	APIKey string `json:"api_key,omitempty"`
}

// Path returns the absolute path to the config file (~/.octo/config.json).
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".octo", "config.json"), nil
}

// Load reads ~/.octo/config.json. A missing file is not an error — it returns
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
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes the config to ~/.octo/config.json with mode 0600 (it may hold an
// API key), creating ~/.octo if needed.
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
