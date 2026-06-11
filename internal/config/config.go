// Package config holds the user's persisted CLI defaults at
// ~/.octo/config.yml — a list of named model configurations plus global
// settings, so a fresh `octo chat` works without re-typing flags or
// re-exporting env vars every session.
//
// Precedence is resolved by the caller (cmd/octo): an explicit CLI flag beats
// this file, which beats the built-in default. API keys are read from the
// environment first; storing one here is opt-in and discouraged (it lands in
// plaintext, mode 0600), so callers fall back to the entry's APIKey only when
// the matching env var is empty.
//
// The file was previously ~/.octo/config.yaml with a single top-level
// provider/model pair. Load reads that legacy file (and legacy fields) when
// config.yml is absent, normalising it into a one-entry Models list; the
// first Save writes the new schema to config.yml and parks the legacy file
// as config.yaml.bak.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelEntry is one named model configuration: everything needed to build a
// sender for it. Name is the entry's identity — the HTTP API uses it as the
// id and `--model <name>` selects the whole entry.
type ModelEntry struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider,omitempty"`
	Model    string `yaml:"model,omitempty"`
	// BaseURL is an optional endpoint override; empty uses the vendor default.
	BaseURL string `yaml:"base_url,omitempty"`
	// APIKey, when set, is a plaintext fallback used only if the provider's
	// env var is empty. Opt-in via `octo config` and stored mode 0600. Prefer
	// the env var.
	APIKey string `yaml:"api_key,omitempty"`
	// ReasoningEffort sets the unified reasoning intensity ("low" | "medium" |
	// "high"). OpenAI sends it as reasoning_effort; Anthropic maps it to an
	// extended-thinking token budget. Empty means off (no extended reasoning).
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
	// ShowReasoning controls whether a reasoning model's thinking trace is
	// streamed to the terminal (dimmed). nil means the built-in default
	// (enabled).
	ShowReasoning *bool `yaml:"show_reasoning,omitempty"`
}

// Config is the persisted set of CLI defaults. Every field is optional; a
// missing file (or a missing field) leaves the zero value, and the caller
// substitutes its built-in default.
type Config struct {
	// Models is the list of named model configurations.
	Models []ModelEntry `yaml:"models,omitempty"`
	// DefaultModel names the entry used when nothing else selects one.
	// Empty falls back to the first entry.
	DefaultModel string `yaml:"default_model,omitempty"`
	// LiteModel optionally names the entry used for cheap internal calls
	// (history compaction). Empty means no lite model.
	LiteModel string `yaml:"lite_model,omitempty"`

	PermissionMode string `yaml:"permission_mode,omitempty"`
	// Coauthor, when true, instructs the agent to append a Co-authored-by line
	// to every git commit message it writes. Default is true (enabled).
	Coauthor *bool `yaml:"coauthor,omitempty"`
	// AccessKey is the shared secret for Web UI / API authentication.
	// When empty, `octo serve` falls back to OCTO_ACCESS_KEY env var or
	// generates a random key on startup.
	AccessKey string `yaml:"access_key,omitempty"`
	// CompactAutoPct is the auto-compaction threshold as a percentage of the
	// model's context window (0–100). When CompactThreshold == 0, the agent
	// compacts once the context exceeds this share of the window. Zero means
	// the built-in default (75%).
	CompactAutoPct int `yaml:"compact_auto_pct,omitempty"`
	// CompactBatchThreshold controls compaction after a tool batch. Semantics
	// mirror CompactThreshold: <0 disables, 0 follows the between-turns trigger,
	// >0 is an explicit token count.
	CompactBatchThreshold int `yaml:"compact_batch_threshold,omitempty"`
	// Tools holds opt-in tooling behaviour (Tool Search for MCP, etc.). A
	// missing block leaves the built-in defaults.
	Tools ToolsConfig `yaml:"tools,omitempty"`
}

// ToolsConfig groups per-tool behaviour knobs under the `tools:` block.
type ToolsConfig struct {
	// ToolSearch defers MCP tool schemas behind a search/describe/call bridge.
	ToolSearch ToolSearchConfig `yaml:"tool_search,omitempty"`
	// DisabledSkills lists skill names the user has toggled off. Disabled skills
	// are hidden from the model (not injected into the system prompt) and from
	// the UI, but remain on disk.
	DisabledSkills []string `yaml:"disabled_skills,omitempty"`
}

// ToolSearchConfig mirrors the documented tools.tool_search block. Empty fields
// fall back to the tools-package defaults (auto / 10% / 5 / 20).
type ToolSearchConfig struct {
	// Enabled is "auto" (default), "on", or "off".
	Enabled string `yaml:"enabled,omitempty"`
	// ThresholdPct is the auto-mode activation threshold as a percent of the
	// model's context window.
	ThresholdPct int `yaml:"threshold_pct,omitempty"`
	// SearchDefaultLimit is how many hits tool_search returns by default.
	SearchDefaultLimit int `yaml:"search_default_limit,omitempty"`
	// MaxSearchLimit caps the caller-supplied limit.
	MaxSearchLimit int `yaml:"max_search_limit,omitempty"`
}

// DefaultEntry returns the entry named by DefaultModel, falling back to the
// first entry, or a zero ModelEntry when none are configured.
func (c Config) DefaultEntry() ModelEntry {
	if e, ok := c.EntryByName(c.DefaultModel); ok {
		return e
	}
	if len(c.Models) > 0 {
		return c.Models[0]
	}
	return ModelEntry{}
}

// EntryByName returns the entry with the given name. An empty name never
// matches.
func (c Config) EntryByName(name string) (ModelEntry, bool) {
	if name == "" {
		return ModelEntry{}, false
	}
	for _, e := range c.Models {
		if e.Name == name {
			return e, true
		}
	}
	return ModelEntry{}, false
}

// SetDefaultEntry replaces the default entry in place (matching by its
// current name), or appends it when no entry exists yet, and points
// DefaultModel at it. Writers that previously mutated the top-level
// provider/model fields go through this.
func (c *Config) SetDefaultEntry(e ModelEntry) {
	if e.Name == "" {
		e.Name = "default"
	}
	cur := c.DefaultEntry()
	for i := range c.Models {
		if c.Models[i].Name == cur.Name && cur.Name != "" {
			// Renaming the default entry keeps references consistent.
			if c.LiteModel == cur.Name {
				c.LiteModel = e.Name
			}
			c.Models[i] = e
			c.DefaultModel = e.Name
			return
		}
	}
	c.Models = append(c.Models, e)
	c.DefaultModel = e.Name
}

// UniqueName derives an entry name from base (typically the model string)
// that does not collide with any existing entry, appending -2, -3, … as
// needed.
func (c Config) UniqueName(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "model"
	}
	if _, ok := c.EntryByName(base); !ok {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, ok := c.EntryByName(candidate); !ok {
			return candidate
		}
	}
}

// Path returns the absolute path to the config file (~/.octo/config.yml).
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".octo", "config.yml"), nil
}

// legacyPath returns the pre-rename location (~/.octo/config.yaml).
func legacyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".octo", "config.yaml"), nil
}

// fileConfig is the on-disk superset: the current schema plus the legacy
// top-level model fields, so both file generations unmarshal through one
// struct and normalize identically.
type fileConfig struct {
	Config `yaml:",inline"`

	LegacyProvider        string `yaml:"provider,omitempty"`
	LegacyModel           string `yaml:"model,omitempty"`
	LegacyBaseURL         string `yaml:"base_url,omitempty"`
	LegacyAPIKey          string `yaml:"api_key,omitempty"`
	LegacyReasoningEffort string `yaml:"reasoning_effort,omitempty"`
	LegacyShowReasoning   *bool  `yaml:"show_reasoning,omitempty"`
}

// normalize folds legacy top-level model fields into a one-entry Models list.
// Files already on the new schema pass through untouched; legacy fields are
// ignored once a models list exists.
func (f fileConfig) normalize() Config {
	c := f.Config
	if len(c.Models) > 0 {
		return c
	}
	if f.LegacyProvider == "" && f.LegacyModel == "" && f.LegacyBaseURL == "" && f.LegacyAPIKey == "" {
		return c
	}
	c.Models = []ModelEntry{{
		Name:            "default",
		Provider:        f.LegacyProvider,
		Model:           f.LegacyModel,
		BaseURL:         f.LegacyBaseURL,
		APIKey:          f.LegacyAPIKey,
		ReasoningEffort: f.LegacyReasoningEffort,
		ShowReasoning:   f.LegacyShowReasoning,
	}}
	c.DefaultModel = "default"
	return c
}

// Load reads ~/.octo/config.yml, falling back to the legacy
// ~/.octo/config.yaml. A missing file is not an error — it returns the zero
// Config so first-run callers need no special-casing. A present but malformed
// file IS an error, so a typo surfaces instead of silently reverting to
// defaults.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		legacy, lerr := legacyPath()
		if lerr != nil {
			return Config{}, lerr
		}
		data, err = os.ReadFile(legacy)
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
	}
	if err != nil {
		return Config{}, err
	}
	var f fileConfig
	if err := yaml.Unmarshal(data, &f); err != nil {
		return Config{}, err
	}
	return f.normalize(), nil
}

// Save writes the config to ~/.octo/config.yml with mode 0600 (it may hold
// API keys), creating ~/.octo if needed. A legacy config.yaml present at that
// moment is renamed to config.yaml.bak — best effort, because config.yml wins
// the read order regardless.
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
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if legacy, lerr := legacyPath(); lerr == nil {
		if _, statErr := os.Stat(legacy); statErr == nil {
			_ = os.Rename(legacy, legacy+".bak")
		}
	}
	return nil
}
