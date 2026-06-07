package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/config"
)

// ─── Provider Presets ───────────────────────────────────────────────────────

// endpointVariant is the JSON shape the frontend expects for regional endpoints.
type endpointVariant struct {
	Label    string `json:"label"`
	LabelKey string `json:"label_key,omitempty"`
	BaseURL  string `json:"base_url"`
	Region   string `json:"region,omitempty"`
}

// providerPreset is the JSON shape returned by GET /api/providers.
type providerPreset struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	BaseURL          string            `json:"base_url"`
	API              string            `json:"api"`
	DefaultModel     string            `json:"default_model"`
	Models           []string          `json:"models,omitempty"`
	LiteModel        string            `json:"lite_model,omitempty"`
	EndpointVariants []endpointVariant `json:"endpoint_variants,omitempty"`
	WebsiteURL       string            `json:"website_url,omitempty"`
}

// buildProviderPresets builds the response for GET /api/providers from the
// canonical app.Registry.  This is the single source of truth — a new vendor
// only needs one line in internal/app/provider.go.
func buildProviderPresets() []providerPreset {
	presets := make([]providerPreset, 0, len(app.Registry))
	for _, v := range app.Registry {
		variants := make([]endpointVariant, len(v.EndpointVariants))
		for i, ev := range v.EndpointVariants {
			variants[i] = endpointVariant{
				Label:    ev.Label,
				LabelKey: ev.LabelKey,
				BaseURL:  ev.BaseURL,
				Region:   ev.Region,
			}
		}
		presets = append(presets, providerPreset{
			ID:               v.ID,
			Name:             v.DisplayName,
			BaseURL:          v.DefaultBaseURL,
			API:              v.API,
			DefaultModel:     v.DefaultModel,
			Models:           v.Models,
			LiteModel:        v.LiteModel,
			EndpointVariants: variants,
			WebsiteURL:       v.WebsiteURL,
		})
	}
	return presets
}

// ─── GET /api/providers ─────────────────────────────────────────────────────

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": buildProviderPresets()})
}

// ─── GET /api/onboard/status ────────────────────────────────────────────────

func (s *Server) handleOnboardStatus(w http.ResponseWriter, r *http.Request) {
	phase := detectOnboardPhase()
	writeJSON(w, http.StatusOK, map[string]any{
		"needs_onboard": phase != "",
		"phase":         phase,
	})
}

// detectOnboardPhase determines whether first-run setup is needed.
//
//	"key_setup"  — no API key configured (hard block).
//	"soul_setup" — key present but SOUL.md missing (soft nudge).
//	""           — fully configured.
func detectOnboardPhase() string {
	cfg, _ := config.Load()

	// Check if any provider key is available.
	hasKey := false
	if cfg.APIKey != "" {
		hasKey = true
	}
	if !hasKey {
		for _, v := range app.Registry {
			if os.Getenv(v.APIKeyEnvVar) != "" {
				hasKey = true
				break
			}
		}
	}
	if !hasKey {
		return "key_setup"
	}

	// Check SOUL.md.
	soulPath := filepath.Join(octoDir(), "SOUL.md")
	if _, err := os.Stat(soulPath); os.IsNotExist(err) {
		return "soul_setup"
	}

	return ""
}

// ─── POST /api/onboard/complete ─────────────────────────────────────────────

func (s *Server) handleOnboardComplete(w http.ResponseWriter, r *http.Request) {
	// Ensure the ~/.octo directory exists.
	_ = os.MkdirAll(octoDir(), 0o700)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── GET /api/config ────────────────────────────────────────────────────────

// configResponse mirrors what the Ruby frontend expects.
type configResponse struct {
	Models          []modelConfig `json:"models,omitempty"`
	DefaultModelIdx int           `json:"default_model_idx,omitempty"`
	FontSize        string        `json:"font_size,omitempty"`
	Currency        string        `json:"currency,omitempty"`
	Language        string        `json:"language,omitempty"`
}

type modelConfig struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key,omitempty"`
	Provider        string `json:"provider,omitempty"`
	AnthropicFormat bool   `json:"anthropic_format"`
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load()

	models := []modelConfig{}
	defaultIdx := 0

	if cfg.Model != "" || cfg.BaseURL != "" {
		models = append(models, modelConfig{
			ID:              "default",
			Type:            "default",
			Model:           cfg.Model,
			BaseURL:         cfg.BaseURL,
			APIKey:          maskKey(cfg.APIKey),
			Provider:        cfg.Provider,
			AnthropicFormat: app.IsAnthropicProtocol(cfg.Provider),
		})
	}

	writeJSON(w, http.StatusOK, configResponse{
		Models:          models,
		DefaultModelIdx: defaultIdx,
		FontSize:        "medium",
		Currency:        "USD",
		Language:        "en",
	})
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", len(k)-8) + k[len(k)-4:]
}

// ─── POST /api/config/test ──────────────────────────────────────────────────

type testConfigRequest struct {
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	Index           int    `json:"index"`
	AnthropicFormat bool   `json:"anthropic_format"`
}

func (s *Server) handleTestConfig(w http.ResponseWriter, r *http.Request) {
	var req testConfigRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Model == "" || req.BaseURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "model and base_url are required"})
		return
	}

	key := req.APIKey
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
	}
	if key == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "no API key provided"})
		return
	}

	providerName := app.ProviderOpenAI
	if req.AnthropicFormat {
		providerName = app.ProviderAnthropic
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if err := app.TestConnection(ctx, providerName, key, req.BaseURL, req.Model); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Connection successful"})
}

// ─── POST /api/config/models ────────────────────────────────────────────────

type saveModelRequest struct {
	Type            string `json:"type"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	Provider        string `json:"provider,omitempty"`
	AnthropicFormat bool   `json:"anthropic_format"`
}

func (s *Server) handleSaveModelConfig(w http.ResponseWriter, r *http.Request) {
	var req saveModelRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	cfg, _ := config.Load()
	cfg.Model = req.Model
	cfg.BaseURL = req.BaseURL
	if req.APIKey != "" {
		cfg.APIKey = req.APIKey
	}
	if req.Provider != "" {
		cfg.Provider = req.Provider
	} else if req.AnthropicFormat {
		cfg.Provider = "anthropic"
	} else {
		cfg.Provider = "openai"
	}

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": "default"})
}

// handleUpdateModelConfig updates an existing model config (same as save for single-model).
func (s *Server) handleUpdateModelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing model id")
		return
	}

	var req saveModelRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	cfg, _ := config.Load()
	cfg.Model = req.Model
	cfg.BaseURL = req.BaseURL
	if req.APIKey != "" {
		cfg.APIKey = req.APIKey
	}
	if req.Provider != "" {
		cfg.Provider = req.Provider
	} else if req.AnthropicFormat {
		cfg.Provider = "anthropic"
	} else {
		cfg.Provider = "openai"
	}

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteModelConfig removes the single model config.
func (s *Server) handleDeleteModelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing model id")
		return
	}

	cfg, _ := config.Load()
	cfg.Model = ""
	cfg.BaseURL = ""
	cfg.APIKey = ""
	cfg.Provider = ""

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetDefaultModelConfig sets the model as default (no-op in single-model system).
func (s *Server) handleSetDefaultModelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing model id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
