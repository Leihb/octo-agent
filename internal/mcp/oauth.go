package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OAuth flow for MCP servers that protect their endpoint with RFC 9728
// metadata pointers (the path the 2024-11-05 MCP spec standardised on).
//
// What this implements
//   - RFC 9728: parse WWW-Authenticate to find protected-resource metadata.
//   - RFC 8414: discover authorization-server metadata (endpoints + grant
//     types) via /.well-known/oauth-authorization-server/<path>.
//   - RFC 7591: dynamic client registration (public client, no secret,
//     PKCE-friendly).
//   - RFC 8628: device authorization grant. Right pick for a CLI — no
//     local listener, just "open <uri> and enter <code>".
//   - Refresh-token flow on near-expiry + cached token persistence.
//
// What this skips
//   - Authorization Code + PKCE (would need a local listener; Device Code
//     gives an equally good UX in a terminal without the extra surface).
//   - Discovery via /.well-known/openid-configuration (we only do the
//     OAuth variant).
//   - Mutual TLS / client_secret_post / etc. — public client, no secret.

// OAuthPrompt is the user-facing side of the device flow. The CLI prints
// the verification URI + user code; tests can stub it.
type OAuthPrompt interface {
	// ShowAuthorization is called when the device flow needs the user to
	// hit a URL and enter a code. verificationURIComplete (if non-empty)
	// is the pre-filled link the user can paste into a browser directly;
	// servers that don't supply it leave it empty and the caller falls
	// back to verificationURI + manual code entry.
	ShowAuthorization(userCode, verificationURI, verificationURIComplete string)
	// Progress is called once per poll cycle so the CLI can keep the user
	// informed ("waiting for authorization…"). Optional — implementations
	// may no-op.
	Progress()
	// Done is called once after a successful authorization.
	Done()
}

// OAuthProvider is the interface HTTPTransport sees. It hands back the
// current bearer token, refreshing or re-authenticating as needed. The
// Invalidate path is used when a 401 is observed against what was
// otherwise a valid-looking cached token — the next Token call re-runs the
// full flow.
type OAuthProvider interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// OAuthClient implements OAuthProvider against a single MCP resource URL.
// Persists state under ~/.octo/mcp-tokens/<server>.json so a fresh
// `octo` session reuses the access token + refresh token from the
// previous run.
type OAuthClient struct {
	resourceURL string
	storePath   string
	clientName  string
	prompt      OAuthPrompt
	hc          *http.Client

	mu       sync.Mutex
	state    *oauthState // loaded lazily on first Token call
	loadOnce sync.Once
}

// oauthState is the persisted token cache shape. Stored as plain JSON at
// storePath — the file is per-user and mode 0600 so other users on the
// host can't read it. Refresh-token rotation is handled by overwriting
// this file on each refresh.
type oauthState struct {
	ResourceURL  string    `json:"resource_url"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenURL     string    `json:"token_url"`
}

// NewOAuthClient builds a provider for the given MCP resource URL.
// serverName names the cache file (and feeds RFC 7591's client_name).
// prompt drives the user-visible parts of the device flow.
func NewOAuthClient(resourceURL, serverName, clientName string, prompt OAuthPrompt) (*OAuthClient, error) {
	if resourceURL == "" {
		return nil, errors.New("oauth: empty resourceURL")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("oauth: resolve home: %w", err)
	}
	storeDir := filepath.Join(home, ".octo", "mcp-tokens")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("oauth: mkdir %s: %w", storeDir, err)
	}
	return &OAuthClient{
		resourceURL: resourceURL,
		storePath:   filepath.Join(storeDir, sanitizeFilename(serverName)+".json"),
		clientName:  clientName,
		prompt:      prompt,
		hc:          &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Token returns a valid bearer token, refreshing or re-authenticating as
// needed. Safe for concurrent callers — one in-flight token negotiation
// at a time via mu.
func (o *OAuthClient) Token(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.loadOnce.Do(func() {
		if st, err := loadOAuthState(o.storePath); err == nil {
			o.state = st
		}
	})

	// 1. Cached + still valid (with a 60s slack for clock skew + round-trip).
	if o.state != nil && o.state.AccessToken != "" &&
		o.state.ExpiresAt.After(time.Now().Add(60*time.Second)) {
		return o.state.AccessToken, nil
	}
	// 2. Cached refresh token + token URL — try to refresh without bothering the user.
	if o.state != nil && o.state.RefreshToken != "" && o.state.TokenURL != "" {
		if err := o.refresh(ctx); err == nil {
			return o.state.AccessToken, nil
		}
		// Refresh failed (revoked / expired). Fall through to full flow.
	}
	// 3. Full flow: discover, register if needed, run device grant.
	if err := o.authorize(ctx); err != nil {
		return "", err
	}
	return o.state.AccessToken, nil
}

// Invalidate forgets the current access token without throwing away the
// refresh token / client_id. The next Token call will try refresh first;
// if refresh fails, it falls into the full flow.
func (o *OAuthClient) Invalidate() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state != nil {
		o.state.AccessToken = ""
		o.state.ExpiresAt = time.Time{}
	}
}

// authorize runs the full discovery + (maybe register) + device flow.
// Caller holds o.mu.
func (o *OAuthClient) authorize(ctx context.Context) error {
	asMeta, err := o.discover(ctx)
	if err != nil {
		return err
	}

	clientID := ""
	clientSecret := ""
	if o.state != nil && o.state.ClientID != "" {
		clientID = o.state.ClientID
		clientSecret = o.state.ClientSecret
	} else if asMeta.RegistrationEndpoint != "" {
		// Public client + PKCE-friendly: no secret expected back.
		id, secret, err := o.register(ctx, asMeta.RegistrationEndpoint)
		if err != nil {
			return fmt.Errorf("oauth: dynamic client registration: %w", err)
		}
		clientID = id
		clientSecret = secret
	} else {
		return errors.New("oauth: server has no registration_endpoint and no cached client_id")
	}

	tok, err := o.deviceFlow(ctx, asMeta, clientID, clientSecret)
	if err != nil {
		return fmt.Errorf("oauth: device flow: %w", err)
	}
	o.state = &oauthState{
		ResourceURL:  o.resourceURL,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiryFromSeconds(tok.ExpiresIn),
		TokenURL:     asMeta.TokenEndpoint,
	}
	if err := saveOAuthState(o.storePath, o.state); err != nil {
		// Persist failure is non-fatal — we still have the token in
		// memory for this session.
		fmt.Fprintf(os.Stderr, "oauth: cache write failed: %v\n", err)
	}
	if o.prompt != nil {
		o.prompt.Done()
	}
	return nil
}

// refresh tries to renew the access token via grant_type=refresh_token.
// Caller holds o.mu. Returns an error if the server rejects the refresh
// (e.g. revoked); caller falls back to full flow.
func (o *OAuthClient) refresh(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", o.state.RefreshToken)
	form.Set("client_id", o.state.ClientID)
	if o.state.ClientSecret != "" {
		form.Set("client_secret", o.state.ClientSecret)
	}
	tok, err := o.postTokenEndpoint(ctx, o.state.TokenURL, form)
	if err != nil {
		return err
	}
	o.state.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		// Some servers rotate refresh tokens on each use; honor the rotation.
		o.state.RefreshToken = tok.RefreshToken
	}
	o.state.ExpiresAt = expiryFromSeconds(tok.ExpiresIn)
	if err := saveOAuthState(o.storePath, o.state); err != nil {
		fmt.Fprintf(os.Stderr, "oauth: cache write failed: %v\n", err)
	}
	return nil
}

// ── discovery (RFC 9728 + 8414) ──────────────────────────────────────────

type asMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	DeviceAuthorizationEndpoint   string   `json:"device_authorization_endpoint"`
	GrantTypesSupported           []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

type prMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// discover does the two-step RFC 9728 → 8414 hop. The 9728 metadata lives
// at the protected resource's well-known path; it points at one or more
// authorization servers. We pick the first.
func (o *OAuthClient) discover(ctx context.Context) (*asMetadata, error) {
	prURL, err := buildPRMetadataURL(o.resourceURL)
	if err != nil {
		return nil, err
	}
	var pr prMetadata
	if err := o.getJSON(ctx, prURL, &pr); err != nil {
		return nil, fmt.Errorf("protected-resource metadata: %w", err)
	}
	if len(pr.AuthorizationServers) == 0 {
		return nil, errors.New("oauth: no authorization_servers in protected-resource metadata")
	}

	asURL, err := buildASMetadataURL(pr.AuthorizationServers[0])
	if err != nil {
		return nil, err
	}
	var as asMetadata
	if err := o.getJSON(ctx, asURL, &as); err != nil {
		return nil, fmt.Errorf("authorization-server metadata: %w", err)
	}
	if as.DeviceAuthorizationEndpoint == "" {
		return nil, errors.New("oauth: server does not support device authorization grant")
	}
	if as.TokenEndpoint == "" {
		return nil, errors.New("oauth: server has no token endpoint")
	}
	return &as, nil
}

// buildPRMetadataURL constructs the RFC 9728 well-known URL by inserting
// /.well-known/oauth-protected-resource just after the host:
//
//	https://host/path  →  https://host/.well-known/oauth-protected-resource
func buildPRMetadataURL(resourceURL string) (string, error) {
	u, err := url.Parse(resourceURL)
	if err != nil {
		return "", err
	}
	// MCP spec says the well-known lives at the resource's origin; suffixing
	// the resource path is non-standard. Reset the path to the well-known.
	u.Path = "/.well-known/oauth-protected-resource"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// buildASMetadataURL constructs the RFC 8414 well-known URL by inserting
// /.well-known/oauth-authorization-server BETWEEN host and the original
// path:
//
//	https://host/b/auth/mcp  →  https://host/.well-known/oauth-authorization-server/b/auth/mcp
//
// Servers may also accept the suffix variant, but the RFC 8414 form is the
// canonical one. We try the canonical form only.
func buildASMetadataURL(issuerURL string) (string, error) {
	u, err := url.Parse(issuerURL)
	if err != nil {
		return "", err
	}
	origPath := strings.TrimSuffix(u.Path, "/")
	u.Path = "/.well-known/oauth-authorization-server" + origPath
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// ── dynamic client registration (RFC 7591) ───────────────────────────────

type registrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (o *OAuthClient) register(ctx context.Context, endpoint string) (clientID, clientSecret string, err error) {
	body := map[string]any{
		"client_name":                o.clientName,
		"grant_types":                []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"token_endpoint_auth_method": "none",
		"application_type":           "native",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", fmt.Errorf("registration HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out registrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if out.ClientID == "" {
		return "", "", errors.New("registration response missing client_id")
	}
	return out.ClientID, out.ClientSecret, nil
}

// ── device flow (RFC 8628) ───────────────────────────────────────────────

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	// Polling-error fields:
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (o *OAuthClient) deviceFlow(ctx context.Context, as *asMetadata, clientID, clientSecret string) (*tokenResponse, error) {
	// 1. Kick off the device authorization.
	form := url.Values{}
	form.Set("client_id", clientID)
	dcResp, err := o.startDevice(ctx, as.DeviceAuthorizationEndpoint, form)
	if err != nil {
		return nil, err
	}

	// 2. Show the user where to go.
	if o.prompt != nil {
		o.prompt.ShowAuthorization(dcResp.UserCode, dcResp.VerificationURI, dcResp.VerificationURIComplete)
	}

	// 3. Poll the token endpoint. Interval defaults to 5s per spec when
	//    the server doesn't specify; the server can ask us to back off
	//    via slow_down.
	interval := time.Duration(dcResp.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dcResp.ExpiresIn) * time.Second)
	if dcResp.ExpiresIn == 0 {
		deadline = time.Now().Add(5 * time.Minute) // server didn't say; pick a sensible bound
	}

	for {
		if time.Now().After(deadline) {
			return nil, errors.New("device flow timed out (user did not authorize)")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if o.prompt != nil {
			o.prompt.Progress()
		}
		tokForm := url.Values{}
		tokForm.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		tokForm.Set("device_code", dcResp.DeviceCode)
		tokForm.Set("client_id", clientID)
		if clientSecret != "" {
			tokForm.Set("client_secret", clientSecret)
		}
		tok, err := o.postTokenEndpoint(ctx, as.TokenEndpoint, tokForm)
		if err == nil {
			return tok, nil
		}
		var oerr *oauthError
		if !errors.As(err, &oerr) {
			return nil, err
		}
		switch oerr.code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token", "access_denied":
			return nil, err
		default:
			// Unknown error code: surface immediately rather than spin.
			return nil, err
		}
	}
}

func (o *OAuthClient) startDevice(ctx context.Context, endpoint string, form url.Values) (*deviceCodeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("device authorization HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return nil, errors.New("device authorization response missing required fields")
	}
	return &out, nil
}

// postTokenEndpoint POSTs to the token endpoint and decodes either a
// successful tokenResponse or an *oauthError. Common error codes (RFC
// 6749 + 8628) are reported as oauthError so the caller can branch on
// authorization_pending / slow_down without parsing strings.
//
// Tolerance: per RFC 6749 OAuth errors should come back as 4xx, but
// servers in the wild (Lark, GitHub Apps, …) routinely return 200 with
// an `{"error":"..."}` body instead. We treat a non-empty `error` field
// as an oauthError regardless of HTTP status, and only declare success
// when `access_token` is set and `error` is empty.
func (o *OAuthClient) postTokenEndpoint(ctx context.Context, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var out tokenResponse
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		// Body wasn't JSON at all — fall through to the HTTP-status error.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, fmt.Errorf("token endpoint returned non-JSON body: %s", strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("token endpoint HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out.Error != "" {
		return nil, &oauthError{code: out.Error, desc: out.ErrorDescription, status: resp.StatusCode}
	}
	if out.AccessToken == "" {
		// Non-error response but no token — usually means the server
		// returned 200 with an empty body or partial payload. Surface as
		// a parse-shaped error so the caller doesn't loop on it forever.
		return nil, fmt.Errorf("token endpoint HTTP %d: missing access_token in response", resp.StatusCode)
	}
	return &out, nil
}

type oauthError struct {
	code   string
	desc   string
	status int
}

func (e *oauthError) Error() string {
	if e.desc != "" {
		return fmt.Sprintf("oauth error %q: %s", e.code, e.desc)
	}
	return fmt.Sprintf("oauth error %q", e.code)
}

// ── helpers ──────────────────────────────────────────────────────────────

func (o *OAuthClient) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// parseWWWAuthenticate extracts the resource_metadata URL from a Bearer
// challenge, e.g.:
//
//	Bearer resource_metadata="https://host/.well-known/oauth-protected-resource"
//
// Returns "" if no resource_metadata key is present. Tolerant of multiple
// Bearer params and varied whitespace.
func parseWWWAuthenticate(header string) string {
	// Find resource_metadata="..."
	const key = "resource_metadata="
	i := strings.Index(header, key)
	if i < 0 {
		return ""
	}
	rest := header[i+len(key):]
	if len(rest) == 0 {
		return ""
	}
	if rest[0] == '"' {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	}
	// Unquoted: take up to comma / whitespace.
	end := strings.IndexAny(rest, ", \t\r\n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func expiryFromSeconds(expiresIn int) time.Time {
	if expiresIn <= 0 {
		return time.Now().Add(1 * time.Hour) // sensible default for servers that omit it
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second)
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func loadOAuthState(path string) (*oauthState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s oauthState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveOAuthState(path string, s *oauthState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 0600 because the file holds an active access token + refresh token.
	return os.WriteFile(path, b, 0o600)
}
