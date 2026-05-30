package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// WebSearchDefaultMax is the default number of results returned per
// search. Most tasks need ≤10; capped at 20 to keep responses small.
const WebSearchDefaultMax = 5

// WebSearchHardMax is the upper bound for max_results regardless of what
// the caller asks for.
const WebSearchHardMax = 20

// Backend endpoints. These are vars (not consts) so tests can redirect
// each backend at an httptest server. Production keeps the defaults.
var (
	braveEndpoint  = "https://api.search.brave.com/res/v1/web/search"
	tavilyEndpoint = "https://api.tavily.com/search"
	serperEndpoint = "https://google.serper.dev/search"
	ddgEndpoint    = "https://html.duckduckgo.com/html/"
	bingEndpoint   = "https://cn.bing.com/search"
)

// WebSearchResult is a single normalised search hit. All five backends
// flatten their wire-format response into this shape so the LLM sees the
// same contract regardless of which backend produced the result.
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// WebSearchResponse is what gets serialised back to the LLM. Provider
// records which backend actually produced the results, so the LLM knows
// whether to trust the corpus (Brave/Google index vs. DDG/Bing HTML scrape).
type WebSearchResponse struct {
	Query    string            `json:"query"`
	Results  []WebSearchResult `json:"results"`
	Count    int               `json:"count"`
	Provider string            `json:"provider"`
	Error    string            `json:"error,omitempty"`
}

// WebSearchTool searches the web. Backend priority (descending):
//  1. Brave Search API (env BRAVE_SEARCH_API_KEY)
//  2. Tavily API       (env TAVILY_API_KEY)
//  3. Serper.dev       (env SERPER_API_KEY)
//  4. DuckDuckGo HTML  (zero key, default)
//  5. Bing HTML        (zero key, fallback when DDG returns nothing)
//
// The tool never panics — every backend failure becomes an Error field in
// the returned response, and the next backend is tried.
type WebSearchTool struct{}

func (WebSearchTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "web_search",
		Description: "Search the web. Returns title/url/snippet for each hit. Works " +
			"with no API key (uses DuckDuckGo + Bing HTML scraping); set " +
			"BRAVE_SEARCH_API_KEY / TAVILY_API_KEY / SERPER_API_KEY to opt into a " +
			"paid backend with higher quality and stricter ToS. The Provider field " +
			"in the response tells you which backend actually produced the results.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query.",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Number of results to return. Defaults to 5, capped at 20.",
				},
			},
			"required": []string{"query"},
		},
	}
}

func (WebSearchTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	query, _ := input["query"].(string)
	if strings.TrimSpace(query) == "" {
		return agent.ToolResult{}, fmt.Errorf("web_search: query is required")
	}
	max := intArg(input, "max_results", WebSearchDefaultMax)
	if max < 1 {
		max = WebSearchDefaultMax
	}
	if max > WebSearchHardMax {
		max = WebSearchHardMax
	}

	// Run backends in priority order, accumulating the last error so we
	// can surface SOMETHING if every backend fails.
	var (
		lastErr   error
		response  WebSearchResponse
		succeeded bool
	)
	response.Query = query

	type backend struct {
		name string
		run  func(context.Context, string, int) ([]WebSearchResult, error)
	}
	backends := []backend{}
	if os.Getenv("BRAVE_SEARCH_API_KEY") != "" {
		backends = append(backends, backend{"brave", searchBrave})
	}
	if os.Getenv("TAVILY_API_KEY") != "" {
		backends = append(backends, backend{"tavily", searchTavily})
	}
	if os.Getenv("SERPER_API_KEY") != "" {
		backends = append(backends, backend{"serper", searchSerper})
	}
	// Zero-key fallbacks are always available.
	backends = append(backends,
		backend{"duckduckgo", searchDuckDuckGo},
		backend{"bing", searchBing},
	)

	for _, b := range backends {
		results, err := b.run(ctx, query, max)
		if err != nil {
			lastErr = err
			continue
		}
		if len(results) == 0 {
			lastErr = fmt.Errorf("%s: zero results", b.name)
			continue
		}
		// Belt-and-suspenders: every backend already self-limits to `max`
		// (API `count` param or parser cap), but if a future backend ever
		// drifts, this stops oversize responses from leaking through.
		if len(results) > max {
			results = results[:max]
		}
		response.Results = results
		response.Count = len(results)
		response.Provider = b.name
		succeeded = true
		break
	}

	if !succeeded {
		if lastErr != nil {
			response.Error = fmt.Sprintf("all backends failed (last error: %v)", lastErr)
		} else {
			// No backend ran at all — only reachable if both fallbacks are
			// somehow stripped from the chain in the future.
			response.Error = "no search backend available"
		}
	}

	body, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("web_search: marshal: %w", err)
	}
	return agent.ToolResult{Text: string(body)}, nil
}

// ───────────────────── paid API backends ─────────────────────

// searchBrave: https://api.search.brave.com/res/v1/web/search
func searchBrave(ctx context.Context, query string, max int) ([]WebSearchResult, error) {
	u := braveEndpoint + "?q=" + url.QueryEscape(query) +
		"&count=" + strconv.Itoa(max)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("X-Subscription-Token", os.Getenv("BRAVE_SEARCH_API_KEY"))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := webHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("brave: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("brave: decode: %w", err)
	}
	out := make([]WebSearchResult, 0, len(data.Web.Results))
	for _, r := range data.Web.Results {
		out = append(out, WebSearchResult{Title: stripHTML(r.Title), URL: r.URL, Snippet: stripHTML(r.Description)})
	}
	return out, nil
}

// searchTavily: https://api.tavily.com/search
func searchTavily(ctx context.Context, query string, max int) ([]WebSearchResult, error) {
	body, _ := json.Marshal(map[string]any{
		"api_key":     os.Getenv("TAVILY_API_KEY"),
		"query":       query,
		"max_results": max,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tavilyEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := webHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("tavily: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var data struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("tavily: decode: %w", err)
	}
	out := make([]WebSearchResult, 0, len(data.Results))
	for _, r := range data.Results {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// searchSerper: https://google.serper.dev/search
func searchSerper(ctx context.Context, query string, max int) ([]WebSearchResult, error) {
	body, _ := json.Marshal(map[string]any{"q": query, "num": max})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, serperEndpoint, bytes.NewReader(body))
	req.Header.Set("X-API-KEY", os.Getenv("SERPER_API_KEY"))
	req.Header.Set("Content-Type", "application/json")

	resp, err := webHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("serper: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("serper: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var data struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("serper: decode: %w", err)
	}
	out := make([]WebSearchResult, 0, len(data.Organic))
	for _, r := range data.Organic {
		out = append(out, WebSearchResult{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return out, nil
}

// ───────────────────── zero-key HTML backends ─────────────────────

// userAgents rotates across five realistic Chrome strings to reduce the
// chance a single UA gets banned. Picked at random per request.
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
}

// browserGet performs a GET request with a complete browser-like header
// set — the bare minimum needed for Bing/DDG to serve the real HTML page
// instead of a bot-detection skeleton.
//
// CRITICAL: we deliberately DO NOT send an Accept-Encoding header. If we
// announce gzip support to Bing, Bing returns a ~39 KB JavaScript skeleton
// page instead of the ~120 KB real results page. Took us a while to
// discover; leave it off.
func browserGet(ctx context.Context, target string, followRedirects int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgents[rand.Intn(len(userAgents))])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	// NOTE: NO Accept-Encoding. See doc comment.

	client := &http.Client{
		Timeout:       12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// Manual redirect following (so we can cap and not auto-follow on POST,
	// though all our requests here are GET).
	for hops := 0; resp.StatusCode >= 300 && resp.StatusCode < 400 && hops < followRedirects; hops++ {
		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}
		_ = resp.Body.Close()
		next := loc
		if strings.HasPrefix(loc, "/") {
			u, _ := url.Parse(target)
			next = u.Scheme + "://" + u.Host + loc
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		for k, vs := range map[string][]string{
			"User-Agent":                {userAgents[rand.Intn(len(userAgents))]},
			"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
			"Accept-Language":           {"zh-CN,zh;q=0.9,en;q=0.8"},
			"Sec-Fetch-Dest":            {"document"},
			"Sec-Fetch-Mode":            {"navigate"},
			"Sec-Fetch-Site":            {"none"},
			"Upgrade-Insecure-Requests": {"1"},
		} {
			req.Header[k] = vs
		}
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}
		target = next
	}
	return resp, nil
}

// ddgCooldown coordinates the DuckDuckGo cooldown across concurrent
// callers. Without the mutex, two goroutines racing the package-level
// time.Time would tear (it's a multi-word struct in Go's runtime). M8's
// web server is the first place where concurrent searches become
// routine; fix here so the bug doesn't ship there.
var ddgCooldown struct {
	mu    sync.RWMutex
	until time.Time
}

// markDDGUnavailable parks DDG attempts for the given duration. Safe to
// call concurrently.
func markDDGUnavailable(d time.Duration) {
	ddgCooldown.mu.Lock()
	ddgCooldown.until = time.Now().Add(d)
	ddgCooldown.mu.Unlock()
}

// ddgCoolingDownUntil returns the cooldown deadline (zero if not cooling).
// Safe to call concurrently.
func ddgCoolingDownUntil() time.Time {
	ddgCooldown.mu.RLock()
	defer ddgCooldown.mu.RUnlock()
	return ddgCooldown.until
}

func searchDuckDuckGo(ctx context.Context, query string, max int) ([]WebSearchResult, error) {
	if until := ddgCoolingDownUntil(); time.Now().Before(until) {
		return nil, fmt.Errorf("duckduckgo: skipped (cooling down until %s)", until.Format(time.Kitchen))
	}
	endpoint := ddgEndpoint + "?q=" + url.QueryEscape(query)
	resp, err := browserGet(ctx, endpoint, 1)
	if err != nil {
		markDDGUnavailable(10 * time.Minute)
		return nil, fmt.Errorf("duckduckgo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		markDDGUnavailable(10 * time.Minute)
		return nil, fmt.Errorf("duckduckgo: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: read: %w", err)
	}
	return parseDuckDuckGoHTML(string(body), max), nil
}

// DDG result anchors look like:
//
//	<a class="result__a" href="//duckduckgo.com/l/?uddg=<urlencoded URL>&…">Title</a>
//	…
//	<a class="result__snippet" …>Snippet text…</a>
var ddgLinkRE = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="//duckduckgo\.com/l/\?uddg=([^"&]+)[^"]*"[^>]*>(.*?)</a>`)
var ddgSnippetRE = regexp.MustCompile(`(?s)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)

func parseDuckDuckGoHTML(body string, max int) []WebSearchResult {
	links := ddgLinkRE.FindAllStringSubmatch(body, -1)
	snippets := ddgSnippetRE.FindAllStringSubmatch(body, -1)
	out := make([]WebSearchResult, 0, len(links))
	for i, m := range links {
		if len(out) >= max {
			break
		}
		decoded, err := url.QueryUnescape(m[1])
		if err != nil || decoded == "" {
			continue
		}
		title := stripHTML(m[2])
		snippet := ""
		if i < len(snippets) {
			snippet = stripHTML(snippets[i][1])
		}
		out = append(out, WebSearchResult{Title: title, URL: decoded, Snippet: snippet})
	}
	return out
}

func searchBing(ctx context.Context, query string, max int) ([]WebSearchResult, error) {
	endpoint := bingEndpoint + "?q=" + url.QueryEscape(query) +
		"&count=" + strconv.Itoa(max)
	resp, err := browserGet(ctx, endpoint, 2)
	if err != nil {
		return nil, fmt.Errorf("bing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bing: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, fmt.Errorf("bing: read: %w", err)
	}
	return parseBingHTML(string(body), max), nil
}

// Bing result blocks: <li class="b_algo"> … </li>
//
// LIMITATION: the non-greedy `</li>` stops at the FIRST closing tag, so if
// Bing ever nests a `<li>` inside a `b_algo` (e.g. site-link clusters,
// FAQ accordions) the captured block will be truncated and parsing of
// that block silently drops it (no title match → skip at the caller).
// Today's Bing HTML doesn't trip this; if results start unexpectedly
// vanishing, this is the first place to look. A net/html tokenizer
// rewrite is the proper fix and is out of scope here.
var bingBlockRE = regexp.MustCompile(`(?s)<li[^>]*class="b_algo"[^>]*>(.*?)</li>`)
var bingTitleRE = regexp.MustCompile(`(?s)<h2[^>]*>.*?<a[^>]*href="(https?://[^"]+)"[^>]*>(.*?)</a>`)
var bingLineclampRE = regexp.MustCompile(`(?s)<p[^>]*class="b_lineclamp[^"]*"[^>]*>(.*?)</p>`)
var bingCaptionRE = regexp.MustCompile(`(?s)<div[^>]*class="b_caption"[^>]*>.*?<p[^>]*>(.*?)</p>`)

func parseBingHTML(body string, max int) []WebSearchResult {
	blocks := bingBlockRE.FindAllStringSubmatch(body, -1)
	out := make([]WebSearchResult, 0, len(blocks))
	for _, b := range blocks {
		if len(out) >= max {
			break
		}
		titleMatch := bingTitleRE.FindStringSubmatch(b[1])
		if titleMatch == nil {
			continue
		}
		realURL := decodeBingURL(titleMatch[1])
		title := stripHTML(titleMatch[2])
		snippet := ""
		if m := bingLineclampRE.FindStringSubmatch(b[1]); m != nil {
			snippet = stripHTML(m[1])
		} else if m := bingCaptionRE.FindStringSubmatch(b[1]); m != nil {
			snippet = stripHTML(m[1])
		}
		out = append(out, WebSearchResult{Title: title, URL: realURL, Snippet: snippet})
	}
	return out
}

// Bing's outbound links are wrapped: bing.com/ck/a?...&u=a1<URL-safe base64>&ntb=1
// The "u" param is "a1" prefix + URL-safe base64 (without padding) of the
// real URL. Decode failures fall back to the wrapper URL — never let a
// single broken link kill the whole result set.
//
// Parsing uses net/url so a `#` fragment or any future query-string oddity
// doesn't trip us up (previous hand-rolled split would mistakenly bake
// `#frag` into the encoded payload and base64-fail).
func decodeBingURL(wrapped string) string {
	if !strings.Contains(wrapped, "bing.com/ck/") {
		return wrapped
	}
	u, err := url.Parse(wrapped)
	if err != nil {
		return wrapped
	}
	uVal := u.Query().Get("u")
	if uVal == "" || !strings.HasPrefix(uVal, "a1") {
		return wrapped
	}
	payload := uVal[2:]
	// URL-safe base64 without padding — restore padding before decoding.
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	dec, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return wrapped
	}
	return string(dec)
}

// ───────────────────── helpers ─────────────────────

var tagRE = regexp.MustCompile(`<[^>]+>`)
var htmlEntities = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&nbsp;", " ",
)

// stripHTML removes inline tags and decodes the most common HTML entities.
// Search-result snippets are short and noisy; we don't need a real HTML
// parser, and pulling one in would balloon dependencies.
func stripHTML(s string) string {
	s = tagRE.ReplaceAllString(s, "")
	s = htmlEntities.Replace(s)
	return strings.TrimSpace(s)
}
