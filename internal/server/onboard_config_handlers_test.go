package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/config"
)

func setTestHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	return tmp
}

func seedModels(t *testing.T, cfg config.Config) {
	t.Helper()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func getConfigResponse(t *testing.T, srv *Server) configResponse {
	t.Helper()
	w := doJSON(t, srv, http.MethodGet, "/api/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d: %s", w.Code, w.Body.String())
	}
	var resp configResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestConfigModels_GetListsAllEntries(t *testing.T) {
	setTestHome(t)
	tru := true
	seedModels(t, config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6", APIKey: "sk-main-12345678"},
			{Name: "kimi", Provider: "kimi", Model: "kimi-k2.6", BaseURL: "https://kimi.example", ShowReasoning: &tru},
			{Name: "cheap", Provider: "deepseek", Model: "deepseek-chat"},
		},
		DefaultModel: "kimi",
		LiteModel:    "cheap",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	resp := getConfigResponse(t, srv)
	if len(resp.Models) != 3 {
		t.Fatalf("models = %d, want 3: %+v", len(resp.Models), resp.Models)
	}
	if resp.DefaultModelIdx != 1 {
		t.Errorf("default_model_idx = %d, want 1", resp.DefaultModelIdx)
	}
	byID := map[string]modelConfig{}
	for _, m := range resp.Models {
		byID[m.ID] = m
	}
	if byID["kimi"].Type != "default" || byID["cheap"].Type != "lite" || byID["main"].Type != "" {
		t.Errorf("type badges wrong: %+v", resp.Models)
	}
	if byID["main"].APIKeyMasked == "" || byID["main"].APIKeyMasked == "sk-main-12345678" {
		t.Errorf("api_key_masked = %q, want masked non-empty", byID["main"].APIKeyMasked)
	}
	if !byID["main"].AnthropicFormat || byID["cheap"].AnthropicFormat {
		t.Errorf("anthropic_format flags wrong: %+v", resp.Models)
	}
}

func TestConfigModels_PostAppendsEntry(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{
		Models:       []config.ModelEntry{{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6"}},
		DefaultModel: "main",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/config/models",
		`{"model":"kimi-k2.6","base_url":"https://api.moonshot.cn","api_key":"sk-kimi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID == "" {
		t.Fatalf("response = %+v", resp)
	}

	cfg, _ := config.Load()
	if len(cfg.Models) != 2 {
		t.Fatalf("entries = %d, want 2 — the second card must not overwrite the first", len(cfg.Models))
	}
	if cfg.DefaultModel != "main" {
		t.Errorf("default moved to %q, want main", cfg.DefaultModel)
	}
	added, ok := cfg.EntryByName(resp.ID)
	if !ok {
		t.Fatalf("entry %q not stored", resp.ID)
	}
	if added.APIKey != "sk-kimi" || added.BaseURL != "https://api.moonshot.cn" {
		t.Errorf("stored entry = %+v", added)
	}
	// base_url matches the kimi vendor preset → provider inferred.
	if added.Provider != "kimi" {
		t.Errorf("provider = %q, want kimi (inferred from base_url)", added.Provider)
	}
}

func TestConfigModels_PostFirstEntryBecomesDefault(t *testing.T) {
	setTestHome(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/config/models",
		`{"model":"claude-sonnet-4-6","base_url":"https://api.anthropic.com","api_key":"sk-x","provider":"anthropic"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load()
	if len(cfg.Models) != 1 || cfg.DefaultModel != cfg.Models[0].Name {
		t.Fatalf("first entry must become default: %+v", cfg)
	}
	if cfg.Models[0].Provider != "anthropic" {
		t.Errorf("explicit provider must win: %q", cfg.Models[0].Provider)
	}
}

func TestConfigModels_PatchUpdatesAndKeepsKey(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6", APIKey: "sk-keep-me"},
		},
		DefaultModel: "main",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	// No api_key in the request → stored key survives. A still-masked echo is
	// also treated as "no change".
	w := doJSON(t, srv, http.MethodPatch, "/api/config/models/main",
		`{"model":"claude-opus-4-7","base_url":"","api_key":"sk-k****1234"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load()
	e, _ := cfg.EntryByName("main")
	if e.Model != "claude-opus-4-7" {
		t.Errorf("model = %q, want claude-opus-4-7", e.Model)
	}
	if e.APIKey != "sk-keep-me" {
		t.Errorf("api_key = %q, want preserved sk-keep-me", e.APIKey)
	}
	// The panel hardcodes anthropic_format=false and sends no provider; an
	// update with no vendor signal must not clobber the stored provider.
	if e.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic preserved", e.Provider)
	}

	if w := doJSON(t, srv, http.MethodPatch, "/api/config/models/ghost", `{"model":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("PATCH unknown id = %d, want 404", w.Code)
	}
}

func TestConfigModels_DeleteRepairsReferences(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "m1"},
			{Name: "kimi", Provider: "kimi", Model: "m2"},
		},
		DefaultModel: "main",
		LiteModel:    "kimi",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	// Deleting the default reassigns it to the first remaining entry.
	if w := doJSON(t, srv, http.MethodDelete, "/api/config/models/main", ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load()
	if len(cfg.Models) != 1 || cfg.DefaultModel != "kimi" {
		t.Fatalf("after delete: %+v", cfg)
	}

	// Deleting the lite entry clears the lite reference.
	if w := doJSON(t, srv, http.MethodDelete, "/api/config/models/kimi", ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ = config.Load()
	if len(cfg.Models) != 0 || cfg.LiteModel != "" || cfg.DefaultModel != "" {
		t.Fatalf("after second delete: %+v", cfg)
	}

	if w := doJSON(t, srv, http.MethodDelete, "/api/config/models/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown id = %d, want 404", w.Code)
	}
}

func TestConfigModels_SetDefault(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "m1"},
			{Name: "kimi", Provider: "kimi", Model: "m2"},
		},
		DefaultModel: "main",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	if w := doJSON(t, srv, http.MethodPost, "/api/config/models/kimi/default", ""); w.Code != http.StatusOK {
		t.Fatalf("set-default = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load()
	if cfg.DefaultModel != "kimi" {
		t.Fatalf("default = %q, want kimi", cfg.DefaultModel)
	}
	if srv.model != "m2" {
		t.Errorf("runtime model = %q, want m2", srv.model)
	}

	if w := doJSON(t, srv, http.MethodPost, "/api/config/models/ghost/default", ""); w.Code != http.StatusNotFound {
		t.Errorf("set-default unknown id = %d, want 404", w.Code)
	}
}

func TestConfigModels_UniqueNameGeneration(t *testing.T) {
	setTestHome(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		w := doJSON(t, srv, http.MethodPost, "/api/config/models",
			fmt.Sprintf(`{"model":"kimi-k2.6","api_key":"sk-%d"}`, i))
		if w.Code != http.StatusOK {
			t.Fatalf("POST #%d = %d: %s", i, w.Code, w.Body.String())
		}
		var resp struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if ids[resp.ID] {
			t.Fatalf("duplicate id %q", resp.ID)
		}
		ids[resp.ID] = true
	}
	cfg, _ := config.Load()
	if len(cfg.Models) != 3 {
		t.Fatalf("entries = %d, want 3", len(cfg.Models))
	}
}

func TestCreateSession_EntryIDBindsSession(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "claude-sonnet-4-6"},
			{Name: "kimi", Provider: "kimi", Model: "kimi-k2.6"},
		},
		DefaultModel: "main",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/sessions", `{"model":"kimi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/sessions = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Session struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Session.Model != "kimi-k2.6" {
		t.Errorf("session model = %q, want kimi-k2.6 (entry id resolved)", resp.Session.Model)
	}
	sess, err := agent.LoadSession(resp.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ModelConfig != "kimi" {
		t.Errorf("model_config = %q, want kimi", sess.ModelConfig)
	}

	// A raw model string stays unbound.
	w = doJSON(t, srv, http.MethodPost, "/api/sessions", `{"model":"some-other-model"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST raw = %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	sess, err = agent.LoadSession(resp.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ModelConfig != "" || sess.Model != "some-other-model" {
		t.Errorf("raw session = (%q, %q), want (some-other-model, \"\")", sess.Model, sess.ModelConfig)
	}
}

func TestConfigModels_SetLiteToggles(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{
		Models: []config.ModelEntry{
			{Name: "main", Provider: "anthropic", Model: "m1"},
			{Name: "cheap", Provider: "deepseek", Model: "m2"},
		},
		DefaultModel: "main",
	})
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	// Set.
	if w := doJSON(t, srv, http.MethodPost, "/api/config/models/cheap/lite", ""); w.Code != http.StatusOK {
		t.Fatalf("set-lite = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load()
	if cfg.LiteModel != "cheap" {
		t.Fatalf("lite = %q, want cheap", cfg.LiteModel)
	}
	// The badge surfaces in GET /api/config.
	resp := getConfigResponse(t, srv)
	for _, m := range resp.Models {
		if m.ID == "cheap" && m.Type != "lite" {
			t.Errorf("cheap type = %q, want lite", m.Type)
		}
	}

	// Toggle off.
	if w := doJSON(t, srv, http.MethodPost, "/api/config/models/cheap/lite", ""); w.Code != http.StatusOK {
		t.Fatalf("unset-lite = %d: %s", w.Code, w.Body.String())
	}
	cfg, _ = config.Load()
	if cfg.LiteModel != "" {
		t.Fatalf("lite = %q, want cleared", cfg.LiteModel)
	}

	if w := doJSON(t, srv, http.MethodPost, "/api/config/models/ghost/lite", ""); w.Code != http.StatusNotFound {
		t.Errorf("set-lite unknown id = %d, want 404", w.Code)
	}
}
