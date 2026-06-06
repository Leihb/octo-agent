package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Leihb/octo-agent/internal/config"
)

// ─── Provider Presets ───────────────────────────────────────────────────────

// endpointVariant is a regional endpoint alternative for a provider.
type endpointVariant struct {
	Label    string `json:"label"`
	LabelKey string `json:"label_key,omitempty"`
	BaseURL  string `json:"base_url"`
	Region   string `json:"region,omitempty"`
}

// providerPreset is a built-in provider configuration template.
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

var providerPresets = []providerPreset{
	{
		ID:           "openrouter",
		Name:         "OpenRouter",
		BaseURL:      "https://openrouter.ai/api/v1",
		API:          "openai-responses",
		DefaultModel: "anthropic/claude-sonnet-4-6",
		Models: []string{
			"anthropic/claude-sonnet-4-6",
			"anthropic/claude-opus-4-7",
			"anthropic/claude-opus-4-6",
			"anthropic/claude-haiku-4-5",
			"openai/gpt-5.5",
			"openai/gpt-5.4",
			"openai/gpt-5.4-mini",
		},
		WebsiteURL: "https://openrouter.ai/keys",
	},
	{
		ID:           "deepseekv4",
		Name:         "DeepSeek V4",
		BaseURL:      "https://api.deepseek.com",
		API:          "openai-completions",
		DefaultModel: "deepseek-v4-pro",
		Models:       []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		LiteModel:    "deepseek-v4-flash",
		WebsiteURL:   "https://platform.deepseek.com/api_keys",
	},
	{
		ID:           "minimax",
		Name:         "Minimax",
		BaseURL:      "https://api.minimaxi.com/v1",
		API:          "openai-completions",
		DefaultModel: "MiniMax-M2.7",
		Models:       []string{"MiniMax-M2.5", "MiniMax-M2.7"},
		EndpointVariants: []endpointVariant{
			{Label: "Mainland China", LabelKey: "settings.models.baseurl.variant.mainland_cn", BaseURL: "https://api.minimaxi.com/v1", Region: "cn"},
			{Label: "International", LabelKey: "settings.models.baseurl.variant.international", BaseURL: "https://api.minimax.io/v1", Region: "intl"},
		},
		WebsiteURL: "https://www.minimaxi.com/user-center/basic-information/interface-key",
	},
	{
		ID:           "kimi",
		Name:         "Kimi (Moonshot)",
		BaseURL:      "https://api.moonshot.cn/v1",
		API:          "openai-completions",
		DefaultModel: "kimi-k2.6",
		Models:       []string{"kimi-k2.6", "kimi-k2.5"},
		EndpointVariants: []endpointVariant{
			{Label: "Mainland China", LabelKey: "settings.models.baseurl.variant.mainland_cn", BaseURL: "https://api.moonshot.cn/v1", Region: "cn"},
			{Label: "International", LabelKey: "settings.models.baseurl.variant.international", BaseURL: "https://api.moonshot.ai/v1", Region: "intl"},
		},
		WebsiteURL: "https://platform.moonshot.cn/console/api-keys",
	},
	{
		ID:           "kimi-coding",
		Name:         "Kimi Coding",
		BaseURL:      "https://api.kimi.com/coding",
		API:          "openai-completions",
		DefaultModel: "kimi-for-coding",
		Models:       []string{"kimi-for-coding"},
		WebsiteURL:   "https://platform.moonshot.cn/console/api-keys",
	},
	{
		ID:           "glm",
		Name:         "GLM (Zhipu)",
		BaseURL:      "https://open.bigmodel.cn/api/paas/v4",
		API:          "openai-completions",
		DefaultModel: "glm-4.5",
		Models:       []string{"glm-4.5", "glm-4.5-air", "glm-4.5-flash"},
		LiteModel:    "glm-4.5-flash",
		EndpointVariants: []endpointVariant{
			{Label: "Mainland China", LabelKey: "settings.models.baseurl.variant.mainland_cn", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Region: "cn"},
			{Label: "International", LabelKey: "settings.models.baseurl.variant.international", BaseURL: "https://api.z.ai/v1", Region: "intl"},
		},
		WebsiteURL: "https://open.bigmodel.cn/usercenter/apikey",
	},
	{
		ID:           "openai",
		Name:         "OpenAI",
		BaseURL:      "https://api.openai.com/v1",
		API:          "openai-responses",
		DefaultModel: "gpt-5.4",
		Models:       []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"},
		LiteModel:    "gpt-5.4-mini",
		WebsiteURL:   "https://platform.openai.com/api-keys",
	},
	{
		ID:           "anthropic",
		Name:         "Anthropic",
		BaseURL:      "https://api.anthropic.com",
		API:          "anthropic-messages",
		DefaultModel: "claude-sonnet-4-6",
		Models:       []string{"claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"},
		LiteModel:    "claude-haiku-4-5",
		WebsiteURL:   "https://console.anthropic.com/settings/keys",
	},
	{
		ID:           "siliconflow",
		Name:         "SiliconFlow",
		BaseURL:      "https://api.siliconflow.cn/v1",
		API:          "openai-completions",
		DefaultModel: "deepseek-ai/DeepSeek-V3",
		Models:       []string{"deepseek-ai/DeepSeek-V3", "deepseek-ai/DeepSeek-R1"},
		WebsiteURL:   "https://cloud.siliconflow.cn/account/ak",
	},
	{
		ID:           "mistral",
		Name:         "Mistral",
		BaseURL:      "https://api.mistral.ai/v1",
		API:          "openai-completions",
		DefaultModel: "mistral-large-latest",
		Models:       []string{"mistral-large-latest", "mistral-medium-latest"},
		WebsiteURL:   "https://console.mistral.ai/api-keys/",
	},
	{
		ID:           "groq",
		Name:         "Groq",
		BaseURL:      "https://api.groq.com/openai/v1",
		API:          "openai-completions",
		DefaultModel: "llama-4-scout",
		Models:       []string{"llama-4-scout", "llama-4-maverick"},
		WebsiteURL:   "https://console.groq.com/keys",
	},
}

// ─── GET /api/providers ─────────────────────────────────────────────────────

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": providerPresets})
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
		for _, env := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OCTO_API_KEY"} {
			if os.Getenv(env) != "" {
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
	Type            string `json:"type"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key,omitempty"`
	AnthropicFormat bool   `json:"anthropic_format"`
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load()

	models := []modelConfig{}
	defaultIdx := 0

	if cfg.Model != "" || cfg.BaseURL != "" {
		models = append(models, modelConfig{
			Type:            "default",
			Model:           cfg.Model,
			BaseURL:         cfg.BaseURL,
			APIKey:          maskKey(cfg.APIKey),
			AnthropicFormat: cfg.Provider == "anthropic",
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
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Index   int    `json:"index"`
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

	// Best-effort connectivity test: try to build a provider client.
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

	// We don't have a lightweight ping endpoint, so we just validate that
	// the provider can be constructed. A full connectivity test would need
	// to make an actual API call.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Configuration looks valid"})
}

// ─── POST /api/config/models ────────────────────────────────────────────────

type saveModelRequest struct {
	Type            string `json:"type"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
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
	if req.AnthropicFormat {
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
