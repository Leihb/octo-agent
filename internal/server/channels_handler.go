package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Leihb/octo-agent/internal/channel"
)

// ─── Channels REST API ────────────────────────────────────────────────────

// channelInfo is the safe-for-frontend view of a platform config (secrets masked).
type channelInfo struct {
	Platform  string            `json:"platform"`
	Enabled   bool              `json:"enabled"`
	Running   bool              `json:"running"`
	HasConfig bool              `json:"has_config"`
	Fields    map[string]string `json:"fields"`
}

var secretKeys = map[string]bool{
	"client_secret": true,
	"app_secret":    true,
	"secret":        true,
	"token":         true,
	"bot_token":     true,
	"bot_secret":    true,
}

// handleListChannels returns all platform configs with secrets masked.
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	cfg, err := channel.LoadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]channelInfo, 0, len(cfg.Channels))
	for name, pc := range cfg.Channels {
		info := platformToInfo(name, pc)
		info.Running = s.isAdapterRunning(name)
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

// handleGetChannel returns a single platform's config.
func (s *Server) handleGetChannel(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if platform == "" {
		writeError(w, http.StatusBadRequest, "missing platform name")
		return
	}

	cfg, err := channel.LoadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	pc := cfg.Platform(platform)
	if pc == nil {
		writeError(w, http.StatusNotFound, "platform not configured")
		return
	}

	info := platformToInfo(platform, pc)
	info.Running = s.isAdapterRunning(platform)
	writeJSON(w, http.StatusOK, info)
}

type channelUpdateRequest struct {
	Enabled bool              `json:"enabled"`
	Fields  map[string]string `json:"fields"`
}

// handleSaveChannel creates or updates a platform's config.
func (s *Server) handleSaveChannel(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if platform == "" {
		writeError(w, http.StatusBadRequest, "missing platform name")
		return
	}

	var req channelUpdateRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	cfg, err := channel.LoadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Convert string fields to map[string]any for SetPlatform.
	fields := make(map[string]any)
	for k, v := range req.Fields {
		fields[k] = v
	}
	if _, ok := fields["enabled"]; !ok {
		fields["enabled"] = req.Enabled
	}

	cfg.SetPlatform(platform, fields)
	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleDeleteChannel removes a platform config entirely.
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if platform == "" {
		writeError(w, http.StatusBadRequest, "missing platform name")
		return
	}

	cfg, err := channel.LoadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cfg.RemovePlatform(platform)
	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleTestChannel tests a connection to a platform with the given credentials.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	var req channelUpdateRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	platform := r.PathValue("platform")

	// Build PlatformConfig from request.
	pc := make(channel.PlatformConfig)
	for k, v := range req.Fields {
		pc[k] = v
	}

	ctor, err := channel.Find(platform)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ad, err := ctor(pc)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	errs := ad.ValidateConfig(pc)
	if len(errs) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": strings.Join(errs, "; ")})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Configuration valid. Connection test requires runtime start."})
}

// handleAvailableChannels returns all registered adapter platforms.
func (s *Server) handleAvailableChannels(w http.ResponseWriter, r *http.Request) {
	platforms := channel.RegisteredPlatforms()
	type availInfo struct {
		Platform string   `json:"platform"`
		Label    string   `json:"label"`
		Fields   []string `json:"fields"`
	}

	// Field definitions per platform.
	fieldDefs := map[string][]string{
		"dingtalk": {"client_id", "client_secret", "allowed_users"},
		"discord":  {"bot_token", "allowed_users"},
		"feishu":   {"app_id", "app_secret", "domain", "allowed_users"},
		"telegram": {"bot_token", "base_url", "parse_mode", "allowed_users"},
		"wecom":    {"bot_id", "secret", "allowed_users"},
		"weixin":   {"base_url", "cred_path"},
	}

	labels := map[string]string{
		"dingtalk": "DingTalk (钉钉)",
		"discord":  "Discord",
		"feishu":   "Feishu (飞书)",
		"telegram": "Telegram",
		"wecom":    "WeCom (企业微信)",
		"weixin":   "WeChat (微信)",
	}

	out := make([]availInfo, 0, len(platforms))
	for _, p := range platforms {
		label := labels[p]
		if label == "" {
			label = p
		}
		fields := fieldDefs[p]
		if fields == nil {
			fields = []string{}
		}
		out = append(out, availInfo{Platform: p, Label: label, Fields: fields})
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func platformToInfo(name string, pc channel.PlatformConfig) channelInfo {
	fields := make(map[string]string)
	for k, v := range pc {
		if k == "enabled" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if secretKeys[k] && len(s) > 4 {
			s = maskSecret(s)
		}
		fields[k] = s
	}

	enabled := false
	if v, ok := pc["enabled"]; ok {
		switch val := v.(type) {
		case bool:
			enabled = val
		case string:
			enabled = val == "true"
		}
	}

	return channelInfo{
		Platform:  name,
		Enabled:   enabled,
		HasConfig: true,
		Fields:    fields,
	}
}

// isAdapterRunning reports whether the platform adapter is currently active.
// The HTTP server no longer runs IM adapters — that moved to the standalone
// `octo channel start` command, which lives in a separate process the server
// can't observe — so this always reports false.
func (s *Server) isAdapterRunning(platform string) bool {
	return false
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}
