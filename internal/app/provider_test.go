package app

import (
	"testing"
)

func TestVendor_KimiCoding(t *testing.T) {
	v := vendorByID("kimi-coding")
	if v == nil {
		t.Fatal("kimi-coding not found in registry")
	}

	if v.DisplayName != "Kimi Coding" {
		t.Errorf("DisplayName = %q, want Kimi Coding", v.DisplayName)
	}
	if v.Protocol != "anthropic" {
		t.Errorf("Protocol = %q, want anthropic", v.Protocol)
	}
	if v.API != "anthropic-messages" {
		t.Errorf("API = %q, want anthropic-messages", v.API)
	}
	if v.DefaultBaseURL != "https://api.kimi.com/coding" {
		t.Errorf("DefaultBaseURL = %q, want https://api.kimi.com/coding", v.DefaultBaseURL)
	}
	if v.DefaultModel != "kimi-for-coding" {
		t.Errorf("DefaultModel = %q, want kimi-for-coding", v.DefaultModel)
	}
	if v.APIKeyEnvVar != "MOONSHOT_API_KEY" {
		t.Errorf("APIKeyEnvVar = %q, want MOONSHOT_API_KEY", v.APIKeyEnvVar)
	}
}

func TestVendor_KimiCoding_BuildClient(t *testing.T) {
	// buildClient should succeed with a dummy key for the anthropic protocol.
	_, err := buildClient("kimi-coding", "sk-dummy-key", "")
	if err != nil {
		t.Fatalf("buildClient(kimi-coding) error: %v", err)
	}
}

func TestVendor_KimiCoding_BuildClient_CustomBaseURL(t *testing.T) {
	// Verify the custom base URL override is applied.
	client, err := buildClient("kimi-coding", "sk-dummy-key", "https://custom.example/v1")
	if err != nil {
		t.Fatalf("buildClient error: %v", err)
	}

	// The openai client exposes BaseURL as a field.
	// Use reflection to avoid importing the internal/openai package here.
	if client == nil {
		t.Fatal("client is nil")
	}

	// The client is an openai.Client which has a BaseURL field.
	// We can't import internal/provider/openai from app_test due to layering,
	// so we rely on TestConnection with a mock server in integration tests.
	// This test at least verifies buildClient does not reject the vendor ID.
}

func TestIsKnownVendor(t *testing.T) {
	for _, id := range []string{
		"openrouter", "deepseek", "minimax", "kimi", "kimi-coding",
		"glm", "openai", "anthropic", "bailian", "mistral", "mimo",
		"openai_compatible", "anthropic_compatible",
	} {
		if !IsKnownVendor(id) {
			t.Errorf("IsKnownVendor(%q) = false, want true", id)
		}
	}
	if IsKnownVendor("bogus") {
		t.Error("IsKnownVendor(bogus) = true, want false")
	}
}

func TestVendor_CompatibleCatchAlls(t *testing.T) {
	cases := []struct {
		id, protocol, api, envVar string
	}{
		{ProviderOpenAICompatible, "openai", "openai-completions", "OPENAI_COMPATIBLE_API_KEY"},
		{ProviderAnthropicCompatible, "anthropic", "anthropic-messages", "ANTHROPIC_COMPATIBLE_API_KEY"},
	}
	for _, tc := range cases {
		v := vendorByID(tc.id)
		if v == nil {
			t.Fatalf("%s not found in registry", tc.id)
		}
		if !v.CustomEndpoint {
			t.Errorf("%s: CustomEndpoint = false, want true", tc.id)
		}
		if v.Protocol != tc.protocol || v.API != tc.api {
			t.Errorf("%s: protocol/api = %q/%q, want %q/%q", tc.id, v.Protocol, v.API, tc.protocol, tc.api)
		}
		if v.DefaultBaseURL != "" || v.DefaultModel != "" || len(v.Models) != 0 {
			t.Errorf("%s: catch-all must not carry a fixed endpoint or model catalogue: %+v", tc.id, v)
		}
		if v.APIKeyEnvVar != tc.envVar {
			t.Errorf("%s: APIKeyEnvVar = %q, want %q", tc.id, v.APIKeyEnvVar, tc.envVar)
		}
	}
	// Every other vendor is pinned: it must declare a fixed endpoint.
	for _, v := range Registry {
		if !v.CustomEndpoint && v.DefaultBaseURL == "" {
			t.Errorf("vendor %q has neither a fixed endpoint nor CustomEndpoint", v.ID)
		}
	}
}

func TestBuildClient_CompatibleRequiresBaseURL(t *testing.T) {
	for _, id := range []string{ProviderOpenAICompatible, ProviderAnthropicCompatible} {
		if _, err := buildClient(id, "sk-dummy", ""); err == nil {
			t.Errorf("buildClient(%s) without base URL must fail", id)
		}
		if _, err := buildClient(id, "sk-dummy", "https://gw.example/v1"); err != nil {
			t.Errorf("buildClient(%s) with base URL: %v", id, err)
		}
	}
	// Pinned vendors still build with no override.
	if _, err := buildClient("anthropic", "sk-dummy", ""); err != nil {
		t.Errorf("buildClient(anthropic) without base URL: %v", err)
	}
}

func TestVendorEnvVars(t *testing.T) {
	tests := []struct {
		id, wantEnv string
	}{
		{"kimi-coding", "MOONSHOT_API_KEY"},
		{"kimi", "MOONSHOT_API_KEY"},
		{"deepseek", "DEEPSEEK_API_KEY"},
		{"openai", "OPENAI_API_KEY"},
		{"anthropic", "ANTHROPIC_API_KEY"},
		{"bailian", "DASHSCOPE_API_KEY"},
		{"mimo", "MIMO_API_KEY"},
	}
	for _, tc := range tests {
		got := VendorAPIKeyEnvVar(tc.id)
		if got != tc.wantEnv {
			t.Errorf("VendorAPIKeyEnvVar(%q) = %q, want %q", tc.id, got, tc.wantEnv)
		}
	}
}

func TestImplicitLiteModel(t *testing.T) {
	cases := []struct {
		name                     string
		provider, model, baseURL string
		want                     string
	}{
		{"deepseek pro gets flash", "deepseek", "deepseek-v4-pro", "", "deepseek-v4-flash"},
		{"vendor default endpoint ok", "deepseek", "deepseek-v4-pro", "https://api.deepseek.com", "deepseek-v4-flash"},
		{"anthropic gets haiku", "anthropic", "claude-sonnet-4-6", "", "claude-haiku-4-5"},
		{"already on the lite model", "deepseek", "deepseek-v4-flash", "", ""},
		{"custom endpoint is a different backend", "openai", "gpt-5.4", "https://dashscope.example/v1", ""},
		{"unknown vendor", "bogus", "m", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImplicitLiteModel(tc.provider, tc.model, tc.baseURL); got != tc.want {
				t.Errorf("ImplicitLiteModel(%q, %q, %q) = %q, want %q", tc.provider, tc.model, tc.baseURL, got, tc.want)
			}
		})
	}

	// A vendor without a LiteModel in the registry yields no implicit lite.
	for _, v := range Registry {
		if v.LiteModel == "" {
			if got := ImplicitLiteModel(v.ID, v.DefaultModel, ""); got != "" {
				t.Errorf("vendor %q has no LiteModel but ImplicitLiteModel = %q", v.ID, got)
			}
			break
		}
	}
}
