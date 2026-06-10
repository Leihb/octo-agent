package channel_test

// External test package: the feishu adapter imports channel, so an in-package
// test importing the adapter would be an import cycle.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Leihb/octo-agent/internal/channel"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/feishu"
)

// writeChannelsYML points HOME at a temp dir holding the given channels.yml.
func writeChannelsYML(t *testing.T, yml string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	dir := filepath.Join(tmp, ".octo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "channels.yml"), []byte(yml), 0600); err != nil {
		t.Fatalf("write channels.yml: %v", err)
	}
}

func TestSendOnce_UnconfiguredPlatform(t *testing.T) {
	writeChannelsYML(t, "channels: {}\n")
	if err := channel.SendOnce("feishu", "oc_x", "hi"); err == nil {
		t.Fatal("want error for unconfigured platform, got nil")
	}
}

func TestSendOnce_UnknownPlatform(t *testing.T) {
	writeChannelsYML(t, "channels:\n  nosuch:\n    enabled: true\n")
	if err := channel.SendOnce("nosuch", "c1", "hi"); err == nil {
		t.Fatal("want error for unregistered adapter, got nil")
	}
}

// TestSendOnce_FeishuDelivers exercises the full standalone-send path: adapter
// constructed from config without Start(), synchronous first token fetch, then
// the message POST — against a stub Feishu API.
func TestSendOnce_FeishuDelivers(t *testing.T) {
	var gotToken, gotSend bool
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			gotToken = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tok-1", "expire": 7200,
			})
		case "/open-apis/im/v1/messages":
			gotSend = true
			if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
				t.Errorf("Authorization = %q, want Bearer tok-1", got)
			}
			var body struct {
				ReceiveID string `json:"receive_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ReceiveID != "oc_test" {
				t.Errorf("receive_id = %q, want oc_test", body.ReceiveID)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]string{"message_id": "om_1"},
			})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer stub.Close()

	writeChannelsYML(t,
		"channels:\n  feishu:\n    enabled: true\n    app_id: a\n    app_secret: s\n    domain: "+stub.URL+"\n")

	if err := channel.SendOnce("feishu", "oc_test", "task done"); err != nil {
		t.Fatalf("SendOnce: %v", err)
	}
	if !gotToken || !gotSend {
		t.Fatalf("token=%v send=%v, want both true", gotToken, gotSend)
	}
}
