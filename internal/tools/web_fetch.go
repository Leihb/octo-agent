package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/version"
)

// JinaReaderHost is the public Jina AI Reader endpoint. Sending a URL via
// `https://r.jina.ai/<URL>` returns the page rendered to Markdown — handles
// JavaScript-rendered pages, paywalls (where Jina has access), and most
// of the noisy chrome a raw curl would dump on the LLM.
const JinaReaderHost = "https://r.jina.ai/"

// jinaReaderHostForTest is the actual host the tool uses. Tests swap it
// out to a local httptest server; production reads the const default.
var jinaReaderHostForTest = JinaReaderHost

// WebFetchMaxBytes caps the response size returned to the LLM. Past this,
// the body is truncated with a clear marker so the LLM can ask for more
// targeted slices via grep / a follow-up fetch.
const WebFetchMaxBytes = 200_000

// WebFetchTool fetches a URL and returns its body as Markdown. It prefers
// the Jina AI Reader proxy for JS-rendered pages and clean HTML-to-Markdown
// conversion, but falls back to a direct HTTP fetch when the proxy fails.
type WebFetchTool struct{}

func (WebFetchTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "web_fetch",
		Description: "Fetch a URL and return its content. Prefers the Jina Reader proxy " +
			"for JS-rendered pages and clean HTML-to-Markdown conversion; falls back to " +
			"a direct HTTP fetch when the proxy is unavailable. Response is truncated at " +
			"~200 KB; for long pages, fetch a more specific URL or grep the returned text. " +
			"Public web only — no authentication.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Full URL to fetch (http or https).",
				},
			},
			"required": []string{"url"},
		},
	}
}

func (WebFetchTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if !NetworkAllowed() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("web_fetch: network access is disabled by sandbox")
	}

	raw, _ := input["url"].(string)
	if strings.TrimSpace(raw) == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("web_fetch: url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("web_fetch: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("web_fetch: only http/https URLs are allowed (got %q)", u.Scheme)
	}

	// Strategy: try Jina Reader proxy first (better quality), then fall back
	// to a direct fetch on network-level or 5xx/429 proxy failures.
	// Jina gets a short 5s timeout so a slow proxy doesn't block the fallback.
	jinaCtx, jinaCancel := context.WithTimeout(ctx, 5*time.Second)
	out, jinaErr := fetchViaJina(jinaCtx, raw)
	jinaCancel()
	if jinaErr == nil {
		return out, nil
	}

	// Fallback conditions: network errors (TLS, DNS, timeout), 5xx proxy
	// errors, or 429 rate-limit. 4xx client errors (e.g. 404) from the proxy
	// are NOT retried — the proxy correctly reflected an upstream 404.
	if shouldFallback(jinaErr) {
		// Give the direct fetch the remaining time up to 30s total.
		directCtx, directCancel := context.WithTimeout(ctx, 30*time.Second)
		out, directErr := fetchDirect(directCtx, raw)
		directCancel()
		if directErr == nil {
			return out, nil
		}
		// Both failed — surface both errors so the LLM knows what happened.
		return agent.ToolResult{Text: ""}, fmt.Errorf("web_fetch: jina proxy failed (%v); direct fetch also failed (%v)", jinaErr, directErr)
	}

	return agent.ToolResult{Text: ""}, fmt.Errorf("web_fetch: %w", jinaErr)
}

// shouldFallback returns true when a Jina proxy error is worth retrying
// with a direct fetch. 4xx errors (except 429 rate-limit) are assumed to
// be legitimate upstream responses and are not retried.
func shouldFallback(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// Network-level errors always fallback.
	if strings.Contains(s, "tls:") ||
		strings.Contains(s, "x509:") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "temporary failure") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "EOF") {
		return true
	}
	// HTTP 5xx or 429 from the proxy — the proxy itself is struggling.
	if strings.Contains(s, "HTTP 5") || strings.Contains(s, "HTTP 429") {
		return true
	}
	return false
}

// fetchViaJina calls the Jina Reader proxy and returns the rendered Markdown.
func fetchViaJina(ctx context.Context, rawURL string) (agent.ToolResult, error) {
	// Jina's contract is literal string concatenation: r.jina.ai/<rest>.
	// Do NOT QueryEscape — Jina parses the rest-of-path itself, and
	// escaping breaks routing. A `#fragment` in raw is technically lost
	// here (Jina won't see it), but fragments are never sent to servers
	// anyway, so this matches normal HTTP semantics.
	jinaURL := jinaReaderHostForTest + rawURL

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaURL, nil)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/markdown,text/plain,*/*")
	// Identify ourselves so Jina can reach us about issues.
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := webFetchHTTPClient().Do(req)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return agent.ToolResult{Text: ""}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return readBody(resp.Body)
}

// fetchDirect performs a direct HTTP GET against the original URL. It uses
// a browser-like header set so simple anti-bot checks don't immediately
// reject us, but it does NOT run JavaScript — dynamic pages will return
// their static HTML skeleton.
func fetchDirect(ctx context.Context, rawURL string) (agent.ToolResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return agent.ToolResult{Text: ""}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return readBody(resp.Body)
}

// readBody reads up to WebFetchMaxBytes+1 from r, truncating if necessary
// and appending a clear marker.
func readBody(r io.Reader) (agent.ToolResult, error) {
	body, err := io.ReadAll(io.LimitReader(r, WebFetchMaxBytes+1))
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("read body: %w", err)
	}
	truncated := false
	if len(body) > WebFetchMaxBytes {
		body = body[:WebFetchMaxBytes]
		truncated = true
	}

	out := string(body)
	if truncated {
		out += "\n\n…[truncated at " + strconv.Itoa(WebFetchMaxBytes) + " bytes]"
	}
	return agent.ToolResult{Text: out}, nil
}

// webHTTPClient is the shared http.Client used by the network backends of
// web_search. Default Go client has no timeout — we set 30s to keep agents
// responsive.
func webHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// webFetchHTTPClient is web_fetch's dedicated client. It refuses to follow a
// cross-host 3xx redirect: web_fetch always targets r.jina.ai, so a redirect
// to a different host means the proxy is bouncing us somewhere unexpected —
// a classic SSRF / data-exfil vector. Same-host redirects (path changes) are
// still followed. web_search keeps the plain webHTTPClient because search
// backends legitimately redirect across hosts.
func webFetchHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			prev := via[len(via)-1].URL.Host
			if !strings.EqualFold(req.URL.Host, prev) {
				return fmt.Errorf("refusing cross-host redirect to %q (from %q); "+
					"re-issue web_fetch against the final URL if that destination is intended",
					req.URL.Host, prev)
			}
			return nil
		},
	}
}
