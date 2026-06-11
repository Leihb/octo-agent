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
	for _, e := range cfg.Models {
		if e.APIKey != "" {
			hasKey = true
			break
		}
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
	Language        string        `json:"language,omitempty"`
}

type modelConfig struct {
	ID              string `json:"id"`
	Type            string `json:"type,omitempty"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKeyMasked    string `json:"api_key_masked,omitempty"`
	Provider        string `json:"provider,omitempty"`
	AnthropicFormat bool   `json:"anthropic_format"`
	PermissionMode  string `json:"permission_mode,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ShowReasoning   *bool  `json:"show_reasoning,omitempty"`
}

// defaultEntryIdx returns the index of the default entry: the one named by
// DefaultModel, else 0.
func defaultEntryIdx(cfg config.Config) int {
	for i, e := range cfg.Models {
		if e.Name == cfg.DefaultModel {
			return i
		}
	}
	return 0
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load()

	models := []modelConfig{}
	defaultIdx := defaultEntryIdx(cfg)

	for i, e := range cfg.Models {
		m := modelConfig{
			ID:              e.Name,
			Model:           e.Model,
			BaseURL:         e.BaseURL,
			APIKeyMasked:    maskKey(e.APIKey),
			Provider:        e.Provider,
			AnthropicFormat: app.IsAnthropicProtocol(e.Provider),
			ReasoningEffort: e.ReasoningEffort,
			ShowReasoning:   e.ShowReasoning,
		}
		switch {
		case i == defaultIdx:
			m.Type = "default"
			m.PermissionMode = cfg.PermissionMode
		case e.Name == cfg.LiteModel:
			m.Type = "lite"
		}
		models = append(models, m)
	}

	writeJSON(w, http.StatusOK, configResponse{
		Models:          models,
		DefaultModelIdx: defaultIdx,
		FontSize:        "medium",
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
	// The panel doesn't name a vendor; a known endpoint pins the protocol.
	if v := app.VendorByBaseURL(req.BaseURL); v != "" {
		providerName = v
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
	PermissionMode  string `json:"permission_mode,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ShowReasoning   *bool  `json:"show_reasoning,omitempty"`
}

// applyModelRequestToEntry overlays the request onto an entry. An empty (or
// still-masked) api_key keeps the stored key — the panel echoes masked keys
// and omits unchanged ones. The vendor resolves from explicit request field >
// known endpoint > the entry's current value; only a brand-new entry with no
// signal at all falls back to the anthropic_format flag.
func applyModelRequestToEntry(req saveModelRequest, e *config.ModelEntry) {
	e.Model = req.Model
	e.BaseURL = req.BaseURL
	if req.APIKey != "" && !strings.Contains(req.APIKey, "****") {
		e.APIKey = req.APIKey
	}
	switch {
	case req.Provider != "":
		e.Provider = req.Provider
	case app.VendorByBaseURL(req.BaseURL) != "":
		e.Provider = app.VendorByBaseURL(req.BaseURL)
	case e.Provider == "" && req.AnthropicFormat:
		e.Provider = app.ProviderAnthropic
	case e.Provider == "":
		e.Provider = app.ProviderOpenAI
	}
	if req.ReasoningEffort != "" {
		e.ReasoningEffort = req.ReasoningEffort
	}
	if req.ShowReasoning != nil {
		e.ShowReasoning = req.ShowReasoning
	}
}

// handleSaveModelConfig creates a new model entry (POST /api/config/models).
func (s *Server) handleSaveModelConfig(w http.ResponseWriter, r *http.Request) {
	var req saveModelRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load config: %v", err))
		return
	}

	var entry config.ModelEntry
	applyModelRequestToEntry(req, &entry)
	entry.Name = cfg.UniqueName(req.Model)
	cfg.Models = append(cfg.Models, entry)
	if cfg.DefaultModel == "" || len(cfg.Models) == 1 {
		cfg.DefaultModel = entry.Name
	}
	if req.PermissionMode != "" {
		cfg.PermissionMode = req.PermissionMode
	}

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}
	s.invalidateSenderCache()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": entry.Name})
}

// handleUpdateModelConfig updates the entry named by {id}.
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

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load config: %v", err))
		return
	}

	updated := false
	for i := range cfg.Models {
		if cfg.Models[i].Name == id {
			applyModelRequestToEntry(req, &cfg.Models[i])
			updated = true
			break
		}
	}
	if !updated {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no model config named %q", id))
		return
	}
	if req.PermissionMode != "" {
		cfg.PermissionMode = req.PermissionMode
	}

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}
	s.invalidateSenderCache()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteModelConfig removes the entry named by {id} and repairs the
// default/lite references that pointed at it.
func (s *Server) handleDeleteModelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing model id")
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load config: %v", err))
		return
	}

	kept := cfg.Models[:0]
	removed := false
	for _, e := range cfg.Models {
		if e.Name == id {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no model config named %q", id))
		return
	}
	cfg.Models = kept
	if cfg.DefaultModel == id {
		cfg.DefaultModel = ""
		if len(cfg.Models) > 0 {
			cfg.DefaultModel = cfg.Models[0].Name
		}
	}
	if cfg.LiteModel == id {
		cfg.LiteModel = ""
	}

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}
	s.invalidateSenderCache()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetLiteModelConfig toggles lite_model on the entry named by {id}:
// pointing it at the entry, or clearing it when the entry is already the
// lite model. The lite entry serves history compaction.
func (s *Server) handleSetLiteModelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing model id")
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load config: %v", err))
		return
	}
	if _, ok := cfg.EntryByName(id); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no model config named %q", id))
		return
	}
	if cfg.LiteModel == id {
		cfg.LiteModel = "" // toggle off
	} else {
		cfg.LiteModel = id
	}

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}
	s.invalidateSenderCache()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lite_model": cfg.LiteModel})
}

// handleSetDefaultModelConfig points default_model at the entry named by {id}.
func (s *Server) handleSetDefaultModelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing model id")
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load config: %v", err))
		return
	}
	if _, ok := cfg.EntryByName(id); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no model config named %q", id))
		return
	}
	cfg.DefaultModel = id

	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}

	// The next agent turn should pick up the new default.
	s.model = cfg.DefaultEntry().Model
	s.invalidateSenderCache()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
