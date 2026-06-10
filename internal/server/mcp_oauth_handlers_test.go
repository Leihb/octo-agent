package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/mcp"
	"github.com/Leihb/octo-agent/internal/tools"
)

// fakeOAuthMCPServer is an MCP endpoint behind a device-flow OAuth gate:
// unauthenticated JSON-RPC posts get 401 + resource metadata, the OAuth
// discovery/registration/device/token endpoints all live on the same mux,
// and an authorized post gets real initialize/tools-list responses.
type fakeOAuthMCPServer struct {
	srv   *httptest.Server
	token string
}

func newFakeOAuthMCPServer(t *testing.T) *fakeOAuthMCPServer {
	t.Helper()
	f := &fakeOAuthMCPServer{token: "access-xyz"}
	mux := http.NewServeMux()

	writeJSONBody := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+f.srv.URL+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		var in mcp.Message
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.ID == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var result any
		switch in.Method {
		case "initialize":
			result = mcp.InitializeResult{
				ProtocolVersion: mcp.ProtocolVersion,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
				ServerInfo:      mcp.Implementation{Name: "fake-oauth", Version: "0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []mcp.Tool{{Name: "secure_ping", Description: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
		case "resources/list":
			result = map[string]any{"resources": []mcp.Resource{}}
		case "prompts/list":
			result = map[string]any{"prompts": []mcp.Prompt{}}
		default:
			result = map[string]any{}
		}
		raw, _ := json.Marshal(result)
		writeJSONBody(w, mcp.Message{JSONRPC: "2.0", ID: in.ID, Result: raw})
	})

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.srv.URL + "/auth"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/auth", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, map[string]any{
			"issuer":                        f.srv.URL,
			"authorization_endpoint":        f.srv.URL + "/auth",
			"token_endpoint":                f.srv.URL + "/token",
			"registration_endpoint":         f.srv.URL + "/register",
			"device_authorization_endpoint": f.srv.URL + "/device",
			"grant_types_supported":         []string{"urn:ietf:params:oauth:grant-type:device_code"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "client-1"})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, map[string]any{
			"device_code":               "device-1",
			"user_code":                 "ABCD-1234",
			"verification_uri":          f.srv.URL + "/auth",
			"verification_uri_complete": f.srv.URL + "/auth?code=ABCD-1234",
			"expires_in":                60,
			"interval":                  1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		// Authorize on the first poll — the web flow's states are still
		// observable because ShowAuthorization runs before polling starts.
		writeJSONBody(w, map[string]any{
			"access_token": f.token,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	f.srv = httptest.NewServer(mux)
	return f
}

func TestMCPOAuth_WebDeviceFlow_EndToEnd(t *testing.T) {
	fake := newFakeOAuthMCPServer(t)
	defer fake.srv.Close()

	home := mcpTestHome(t, `{"mcpServers": {"secure": {"url": "`+fake.srv.URL+`/mcp", "auth": "oauth"}}}`)
	t.Cleanup(app.ShutdownMCP)
	tools.SetMCPRegistry(nil)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: true})

	w := doJSON(t, srv, http.MethodPost, "/api/mcp/servers/secure/oauth/start", "")
	if w.Code != http.StatusOK {
		t.Fatalf("start: status = %d: %s", w.Code, w.Body.String())
	}

	// Poll until the flow settles (the fake authorizes on the first token
	// poll, ~1s after the device code is issued).
	deadline := time.Now().Add(15 * time.Second)
	var last map[string]any
	for {
		if time.Now().After(deadline) {
			t.Fatalf("flow did not settle; last status: %+v", last)
		}
		w = doJSON(t, srv, http.MethodGet, "/api/mcp/servers/secure/oauth/status", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d: %s", w.Code, w.Body.String())
		}
		last = map[string]any{}
		if err := json.Unmarshal(w.Body.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
		state := last["state"]
		if state == "connected" {
			break
		}
		if state == "failed" {
			t.Fatalf("flow failed: %+v", last)
		}
		// While authorizing, the device code must be exposed to the UI.
		if state == "authorizing" && last["user_code"] != "ABCD-1234" {
			t.Fatalf("authorizing without user_code: %+v", last)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The connection must be live in the registry with its tools listed.
	reg := tools.ActiveMCPRegistry()
	if reg == nil || reg.Get("secure") == nil {
		t.Fatal("authorized server should be connected in the registry")
	}
	if n := len(reg.Get("secure").Tools); n != 1 {
		t.Fatalf("tools = %d, want 1", n)
	}

	// The token must be cached for future non-interactive connects.
	if _, err := os.Stat(filepath.Join(home, ".octo", "mcp-tokens", "secure.json")); err != nil {
		t.Fatalf("token cache not written: %v", err)
	}

	// The panel list reflects the connected state.
	w = doJSON(t, srv, http.MethodGet, "/api/mcp/servers", "")
	servers := decodeServers(t, w)
	if len(servers) != 1 || servers[0].Status != "connected" {
		t.Fatalf("panel list after oauth: %+v", servers)
	}
}

func TestMCPOAuth_StartValidation(t *testing.T) {
	mcpTestHome(t, `{"mcpServers": {
		"plain": {"url": "https://x.example"},
		"off":   {"url": "https://y.example", "auth": "oauth", "disabled": true}
	}}`)
	t.Cleanup(app.ShutdownMCP)
	tools.SetMCPRegistry(nil)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: true})

	cases := []struct {
		path string
		want int
	}{
		{"/api/mcp/servers/missing/oauth/start", http.StatusNotFound},
		{"/api/mcp/servers/plain/oauth/start", http.StatusBadRequest}, // not oauth
		{"/api/mcp/servers/off/oauth/start", http.StatusConflict},     // disabled
	}
	for _, c := range cases {
		w := doJSON(t, srv, http.MethodPost, c.path, "")
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.path, w.Code, c.want)
		}
	}

	// No flow started → status is 404.
	w := doJSON(t, srv, http.MethodGet, "/api/mcp/servers/plain/oauth/status", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status without flow: %d, want 404", w.Code)
	}
}
