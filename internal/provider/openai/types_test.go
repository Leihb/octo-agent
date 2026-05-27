package openai

import "testing"

func TestApiUsage_CachedTokens(t *testing.T) {
	// DeepSeek: explicit hit count.
	if got := (apiUsage{PromptCacheHitTokens: 128}).cachedTokens(); got != 128 {
		t.Errorf("deepseek hit = %d, want 128", got)
	}
	// OpenAI: nested prompt_tokens_details.cached_tokens.
	u := apiUsage{}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 64}
	if got := u.cachedTokens(); got != 64 {
		t.Errorf("openai cached = %d, want 64", got)
	}
	// Neither reported → 0.
	if got := (apiUsage{}).cachedTokens(); got != 0 {
		t.Errorf("no cache info = %d, want 0", got)
	}
	// DeepSeek hit takes precedence when both somehow present.
	u2 := apiUsage{PromptCacheHitTokens: 200}
	u2.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 1}
	if got := u2.cachedTokens(); got != 200 {
		t.Errorf("precedence = %d, want 200", got)
	}
}
