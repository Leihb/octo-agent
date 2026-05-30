package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// swapEndpoint replaces a backend endpoint var for the duration of the
// test, restoring it on cleanup. Saves a t.Cleanup boilerplate at every
// test site.
func swapEndpoint(t *testing.T, target *string, value string) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// clearSearchEnv unsets every backend env var so a test starts from a
// known "zero-key" state, then restores them on cleanup.
func clearSearchEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BRAVE_SEARCH_API_KEY", "TAVILY_API_KEY", "SERPER_API_KEY"} {
		t.Setenv(k, "")
	}
}

// resetDDGCooldown ensures the per-process DDG cooldown isn't tripped
// from an earlier test. Without this, the second test in a run can
// skip DDG and silently fail the assertion.
func resetDDGCooldown() {
	ddgCooldown.mu.Lock()
	ddgCooldown.until = time.Time{}
	ddgCooldown.mu.Unlock()
}

func TestWebSearch_BravePreferred(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()
	t.Setenv("BRAVE_SEARCH_API_KEY", "test-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "test-key" {
			t.Errorf("Brave header missing/wrong: %q", r.Header.Get("X-Subscription-Token"))
		}
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"R1","url":"https://r1","description":"desc1"},
			{"title":"R2","url":"https://r2","description":"desc2"}
		]}}`))
	}))
	defer srv.Close()
	swapEndpoint(t, &braveEndpoint, srv.URL)

	out, err := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{
		"query": "go generics",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp WebSearchResponse
	if err := json.Unmarshal([]byte(out.Text), &resp); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out.Text)
	}
	if resp.Provider != "brave" {
		t.Errorf("Provider = %q, want 'brave'", resp.Provider)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://r1" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestWebSearch_TavilyWhenBraveAbsent(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()
	t.Setenv("TAVILY_API_KEY", "tk")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"api_key":"tk"`) {
			t.Errorf("Tavily key not in body: %s", body)
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"T1","url":"https://t1","content":"hi"}]}`))
	}))
	defer srv.Close()
	swapEndpoint(t, &tavilyEndpoint, srv.URL)

	out, _ := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{"query": "x"})
	var resp WebSearchResponse
	_ = json.Unmarshal([]byte(out.Text), &resp)
	if resp.Provider != "tavily" || len(resp.Results) != 1 || resp.Results[0].URL != "https://t1" {
		t.Errorf("unexpected: %+v", resp)
	}
}

func TestWebSearch_SerperWhenOnlySerperSet(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()
	t.Setenv("SERPER_API_KEY", "sk")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "sk" {
			t.Errorf("Serper key header wrong: %q", r.Header.Get("X-API-KEY"))
		}
		_, _ = w.Write([]byte(`{"organic":[{"title":"S1","link":"https://s1","snippet":"sn"}]}`))
	}))
	defer srv.Close()
	swapEndpoint(t, &serperEndpoint, srv.URL)

	out, _ := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{"query": "x"})
	var resp WebSearchResponse
	_ = json.Unmarshal([]byte(out.Text), &resp)
	if resp.Provider != "serper" || resp.Results[0].URL != "https://s1" {
		t.Errorf("unexpected: %+v", resp)
	}
}

func TestWebSearch_FallsBackToDDG(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()

	// DDG HTML with one well-formed result block.
	ddgHTML := `
		<html><body>
			<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fhit&amp;rut=abc">Example Site</a>
			<a class="result__snippet" href="//x">This is a sample snippet.</a>
		</body></html>
	`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ddgHTML))
	}))
	defer srv.Close()
	swapEndpoint(t, &ddgEndpoint, srv.URL)

	out, err := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{"query": "test"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp WebSearchResponse
	if err := json.Unmarshal([]byte(out.Text), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.Text)
	}
	if resp.Provider != "duckduckgo" {
		t.Errorf("Provider = %q, want 'duckduckgo'", resp.Provider)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %+v", resp.Results)
	}
	if resp.Results[0].URL != "https://example.com/hit" {
		t.Errorf("URL = %q", resp.Results[0].URL)
	}
	if !strings.Contains(resp.Results[0].Snippet, "sample snippet") {
		t.Errorf("snippet = %q", resp.Results[0].Snippet)
	}
}

func TestWebSearch_FallsBackToBing_WhenDDGReturnsZero(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()

	ddgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>no results</html>"))
	}))
	defer ddgSrv.Close()
	swapEndpoint(t, &ddgEndpoint, ddgSrv.URL)

	bingHTML := `
		<html><body>
			<li class="b_algo">
				<h2><a href="https://bingresult.example.com/page">Bing Result Title</a></h2>
				<p class="b_lineclamp2">Bing snippet text.</p>
			</li>
		</body></html>
	`
	bingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(bingHTML))
	}))
	defer bingSrv.Close()
	swapEndpoint(t, &bingEndpoint, bingSrv.URL)

	out, _ := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{"query": "x"})
	var resp WebSearchResponse
	_ = json.Unmarshal([]byte(out.Text), &resp)
	if resp.Provider != "bing" {
		t.Errorf("Provider = %q, want 'bing'", resp.Provider)
	}
	if len(resp.Results) != 1 || resp.Results[0].Title != "Bing Result Title" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestWebSearch_AllBackendsFail(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()
	// Point DDG and Bing at sinks that return 500.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer fail.Close()
	swapEndpoint(t, &ddgEndpoint, fail.URL)
	swapEndpoint(t, &bingEndpoint, fail.URL)

	out, err := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{"query": "anything"})
	if err != nil {
		t.Fatalf("tool should not return Go error: %v", err)
	}
	var resp WebSearchResponse
	_ = json.Unmarshal([]byte(out.Text), &resp)
	if resp.Error == "" {
		t.Errorf("expected Error field set, got: %s", out.Text)
	}
	if resp.Count != 0 {
		t.Errorf("expected zero results, got %d", resp.Count)
	}
}

func TestWebSearch_RequiresQuery(t *testing.T) {
	_, err := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{})
	if err == nil {
		t.Fatal("missing query should error")
	}
}

func TestWebSearch_RespectsMaxResults(t *testing.T) {
	clearSearchEnv(t)
	resetDDGCooldown()
	t.Setenv("BRAVE_SEARCH_API_KEY", "k")
	// Brave returns 5; ask for 2.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"a","url":"https://a","description":""},
			{"title":"b","url":"https://b","description":""},
			{"title":"c","url":"https://c","description":""},
			{"title":"d","url":"https://d","description":""},
			{"title":"e","url":"https://e","description":""}
		]}}`))
	}))
	defer srv.Close()
	swapEndpoint(t, &braveEndpoint, srv.URL)

	out, _ := WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{
		"query":       "x",
		"max_results": 2,
	})
	var resp WebSearchResponse
	_ = json.Unmarshal([]byte(out.Text), &resp)
	if len(resp.Results) != 2 {
		t.Errorf("want 2 results, got %d", len(resp.Results))
	}
}

func TestDecodeBingURL(t *testing.T) {
	// Construct a synthetic Bing wrapper URL with a real encoded payload.
	// Real URL: https://example.com/page
	// Base64-URL of that (no padding): aHR0cHM6Ly9leGFtcGxlLmNvbS9wYWdl
	// Bing prefixes with "a1": a1aHR0cHM6Ly9leGFtcGxlLmNvbS9wYWdl
	wrapper := "https://www.bing.com/ck/a?u=a1aHR0cHM6Ly9leGFtcGxlLmNvbS9wYWdl&ntb=1"
	got := decodeBingURL(wrapper)
	if got != "https://example.com/page" {
		t.Errorf("decode = %q, want %q", got, "https://example.com/page")
	}
}

func TestDecodeBingURL_PassesThroughOnFailure(t *testing.T) {
	// Not a Bing wrapper — should pass through unchanged.
	plain := "https://example.com/normal-link"
	if got := decodeBingURL(plain); got != plain {
		t.Errorf("passthrough failed: %q", got)
	}
	// Malformed Bing wrapper — should fall back to wrapper rather than panic.
	bad := "https://www.bing.com/ck/a?u=garbage&ntb=1"
	if got := decodeBingURL(bad); got != bad {
		t.Errorf("malformed should pass through: %q", got)
	}
}

func TestStripHTML(t *testing.T) {
	got := stripHTML(`<b>Hello</b> &amp; world &#39;quoted&#39;`)
	want := "Hello & world 'quoted'"
	if got != want {
		t.Errorf("stripHTML = %q, want %q", got, want)
	}
}

func TestDDGCooldown_ConcurrentSafeUnderRace(t *testing.T) {
	// Reproducer for the data race on ddgCooldown that earlier lived as
	// an unsynced package-level time.Time. Run under `go test -race ./...`;
	// without the mutex, this fails because of concurrent read+write.
	clearSearchEnv(t)
	resetDDGCooldown()
	t.Cleanup(resetDDGCooldown)

	// Pin DDG and Bing at a server that always 500s so both backends fail
	// and the loop hits the cooldown write path.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer fail.Close()
	swapEndpoint(t, &ddgEndpoint, fail.URL)
	swapEndpoint(t, &bingEndpoint, fail.URL)

	const goroutines = 16
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = WebSearchTool{}.Execute(context.Background(), "web_search", map[string]any{"query": "x"})
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
}

func TestParseBingHTML_NestedLi_KnownLimitation(t *testing.T) {
	// Locks in the documented LIMITATION on bingBlockRE: a nested `<li>`
	// inside a b_algo block truncates the captured block. The expected
	// behavior today is that the truncated block is silently dropped
	// (title regex misses), so the test asserts an EMPTY result set.
	// When this regex is rewritten with a real HTML tokenizer, this test
	// should flip — leave a clear breadcrumb here so the next maintainer
	// knows what changed.
	html := `
		<html><body>
			<li class="b_algo">
				<ul><li>nested sitelink</li></ul>
				<h2><a href="https://example.com/page">Real Title</a></h2>
				<p class="b_lineclamp2">Real snippet.</p>
			</li>
		</body></html>
	`
	got := parseBingHTML(html, 5)
	if len(got) != 0 {
		t.Errorf("nested-<li> currently truncates parsing; expected 0 results, got %d: %+v", len(got), got)
		t.Logf("If this test starts failing because the parser now returns 1, the limitation has been fixed — update the test and remove the LIMITATION comment in web_search.go.")
	}
}
