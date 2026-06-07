// Package app defines the provider-vendor registry and protocol mapping.
//
// A "provider" here is a business-level vendor (kimi, deepseek, openai,
// anthropic, …), not a wire protocol.  Each vendor carries:
//   - the wire protocol it speaks (anthropic-messages or openai-completions)
//   - its default endpoint, default model, and API-key env var
//
// CLI, server, and Web UI all resolve through this single registry so a new
// vendor only needs one line here.
package app

// EndpointVariant is a regional endpoint alternative for a vendor.
type EndpointVariant struct {
	Label    string `json:"label"`
	LabelKey string `json:"label_key,omitempty"`
	BaseURL  string `json:"base_url"`
	Region   string `json:"region,omitempty"`
}

// Vendor is everything needed to bootstrap a provider client and render it in
type Vendor struct {
	ID               string            // canonical identifier, e.g. "kimi"
	DisplayName      string            // human label, e.g. "Kimi (Moonshot)"
	Protocol         string            // "anthropic" or "openai"
	API              string            // "anthropic-messages" or "openai-completions" or "openai-responses"
	DefaultBaseURL   string            // vendor's official endpoint
	DefaultModel     string            // cheapest/reasoning-capable default
	Models           []string          // available models (for UI dropdown)
	LiteModel        string            // lightweight/cheaper model variant
	APIKeyEnvVar     string            // environment variable name for the key
	WebsiteURL       string            // link to the key-management page
	EndpointVariants []EndpointVariant // regional endpoint alternatives
}

// Registry is the ordered list of supported vendors.  The order is the
// display order in the Web UI onboarding dropdown.
var Registry = []Vendor{
	{
		ID:             "openrouter",
		DisplayName:    "OpenRouter",
		Protocol:       "openai",
		API:            "openai-responses",
		DefaultBaseURL: "https://openrouter.ai/api/v1",
		DefaultModel:   "anthropic/claude-sonnet-4-6",
		Models: []string{
			"anthropic/claude-sonnet-4-6",
			"anthropic/claude-opus-4-7",
			"anthropic/claude-opus-4-6",
			"anthropic/claude-haiku-4-5",
			"openai/gpt-5.5",
			"openai/gpt-5.4",
			"openai/gpt-5.4-mini",
		},
		APIKeyEnvVar: "OPENROUTER_API_KEY",
		WebsiteURL:   "https://openrouter.ai/keys",
	},
	{
		ID:             "deepseek",
		DisplayName:    "DeepSeek",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.deepseek.com",
		DefaultModel:   "deepseek-v4-pro",
		Models:         []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		LiteModel:      "deepseek-v4-flash",
		APIKeyEnvVar:   "DEEPSEEK_API_KEY",
		WebsiteURL:     "https://platform.deepseek.com/api_keys",
	},
	{
		ID:             "minimax",
		DisplayName:    "Minimax",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.minimaxi.com/v1",
		DefaultModel:   "MiniMax-M2.7",
		Models:         []string{"MiniMax-M2.5", "MiniMax-M2.7"},
		APIKeyEnvVar:   "MINIMAX_API_KEY",
		WebsiteURL:     "https://www.minimaxi.com/user-center/basic-information/interface-key",
		EndpointVariants: []EndpointVariant{
			{Label: "Mainland China", LabelKey: "settings.models.baseurl.variant.mainland_cn", BaseURL: "https://api.minimaxi.com/v1", Region: "cn"},
			{Label: "International", LabelKey: "settings.models.baseurl.variant.international", BaseURL: "https://api.minimax.io/v1", Region: "intl"},
		},
	},
	{
		ID:             "kimi",
		DisplayName:    "Kimi (Moonshot)",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.moonshot.cn/v1",
		DefaultModel:   "kimi-k2.6",
		Models:         []string{"kimi-k2.6", "kimi-k2.5"},
		APIKeyEnvVar:   "MOONSHOT_API_KEY",
		WebsiteURL:     "https://platform.moonshot.cn/console/api-keys",
		EndpointVariants: []EndpointVariant{
			{Label: "Mainland China", LabelKey: "settings.models.baseurl.variant.mainland_cn", BaseURL: "https://api.moonshot.cn/v1", Region: "cn"},
			{Label: "International", LabelKey: "settings.models.baseurl.variant.international", BaseURL: "https://api.moonshot.ai/v1", Region: "intl"},
		},
	},
	{
		ID:             "kimi-coding",
		DisplayName:    "Kimi Coding",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.kimi.com/coding",
		DefaultModel:   "kimi-for-coding",
		Models:         []string{"kimi-for-coding"},
		APIKeyEnvVar:   "MOONSHOT_API_KEY",
		WebsiteURL:     "https://platform.moonshot.cn/console/api-keys",
	},
	{
		ID:             "glm",
		DisplayName:    "GLM (Zhipu)",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://open.bigmodel.cn/api/paas/v4",
		DefaultModel:   "glm-4.5",
		Models:         []string{"glm-4.5", "glm-4.5-air", "glm-4.5-flash"},
		LiteModel:      "glm-4.5-flash",
		APIKeyEnvVar:   "ZHIPU_API_KEY",
		WebsiteURL:     "https://open.bigmodel.cn/usercenter/apikey",
		EndpointVariants: []EndpointVariant{
			{Label: "Mainland China", LabelKey: "settings.models.baseurl.variant.mainland_cn", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Region: "cn"},
			{Label: "International", LabelKey: "settings.models.baseurl.variant.international", BaseURL: "https://api.z.ai/v1", Region: "intl"},
		},
	},
	{
		ID:             "openai",
		DisplayName:    "OpenAI",
		Protocol:       "openai",
		API:            "openai-responses",
		DefaultBaseURL: "https://api.openai.com/v1",
		DefaultModel:   "gpt-5.4",
		Models:         []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"},
		LiteModel:      "gpt-5.4-mini",
		APIKeyEnvVar:   "OPENAI_API_KEY",
		WebsiteURL:     "https://platform.openai.com/api-keys",
	},
	{
		ID:             "anthropic",
		DisplayName:    "Anthropic",
		Protocol:       "anthropic",
		API:            "anthropic-messages",
		DefaultBaseURL: "https://api.anthropic.com",
		DefaultModel:   "claude-sonnet-4-6",
		Models:         []string{"claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"},
		LiteModel:      "claude-haiku-4-5",
		APIKeyEnvVar:   "ANTHROPIC_API_KEY",
		WebsiteURL:     "https://console.anthropic.com/settings/keys",
	},
	{
		ID:             "siliconflow",
		DisplayName:    "SiliconFlow",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.siliconflow.cn/v1",
		DefaultModel:   "deepseek-ai/DeepSeek-V3",
		Models:         []string{"deepseek-ai/DeepSeek-V3", "deepseek-ai/DeepSeek-R1"},
		APIKeyEnvVar:   "SILICONFLOW_API_KEY",
		WebsiteURL:     "https://cloud.siliconflow.cn/account/ak",
	},
	{
		ID:             "mistral",
		DisplayName:    "Mistral",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.mistral.ai/v1",
		DefaultModel:   "mistral-large-latest",
		Models:         []string{"mistral-large-latest", "mistral-medium-latest"},
		APIKeyEnvVar:   "MISTRAL_API_KEY",
		WebsiteURL:     "https://console.mistral.ai/api-keys/",
	},
	{
		ID:             "groq",
		DisplayName:    "Groq",
		Protocol:       "openai",
		API:            "openai-completions",
		DefaultBaseURL: "https://api.groq.com/openai/v1",
		DefaultModel:   "llama-4-scout",
		Models:         []string{"llama-4-scout", "llama-4-maverick"},
		APIKeyEnvVar:   "GROQ_API_KEY",
		WebsiteURL:     "https://console.groq.com/keys",
	},
}

// vendorByID returns the vendor with the given ID, or nil if unknown.
func vendorByID(id string) *Vendor {
	for i := range Registry {
		if Registry[i].ID == id {
			return &Registry[i]
		}
	}
	return nil
}

// VendorBaseURL returns the default base URL for a vendor ID, or "" if unknown.
func VendorBaseURL(id string) string {
	if v := vendorByID(id); v != nil {
		return v.DefaultBaseURL
	}
	return ""
}

// VendorProtocol returns "anthropic" or "openai" for a vendor ID, or "" if unknown.
func VendorProtocol(id string) string {
	if v := vendorByID(id); v != nil {
		return v.Protocol
	}
	return ""
}

// VendorDefaultModel returns the built-in default model for a vendor ID.
func VendorDefaultModel(id string) string {
	if v := vendorByID(id); v != nil {
		return v.DefaultModel
	}
	return ""
}

// VendorAPIKeyEnvVar returns the environment-variable name for a vendor's API key.
func VendorAPIKeyEnvVar(id string) string {
	if v := vendorByID(id); v != nil {
		return v.APIKeyEnvVar
	}
	return ""
}

// VendorDisplayName returns the human-readable label for a vendor ID.
func VendorDisplayName(id string) string {
	if v := vendorByID(id); v != nil {
		return v.DisplayName
	}
	return id
}

// VendorWebsiteURL returns the key-management page URL for a vendor ID.
func VendorWebsiteURL(id string) string {
	if v := vendorByID(id); v != nil {
		return v.WebsiteURL
	}
	return ""
}

// IsKnownVendor reports whether id is a registered vendor ID.
func IsKnownVendor(id string) bool {
	return vendorByID(id) != nil
}

// IsAnthropicProtocol reports whether the vendor speaks the Anthropic protocol.
func IsAnthropicProtocol(id string) bool {
	return VendorProtocol(id) == "anthropic"
}
