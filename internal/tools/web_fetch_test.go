package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withJinaHost swaps JinaReaderHost for the test server URL. Since
// JinaReaderHost is a const, we test by directly hitting the helper that
// uses the URL passed in. Instead, we mock at the network layer by setting
// up an httptest server and rewriting the URL the tool would call.
//
// The simpler approach: spin up a server, then call WebFetchTool.Execute
// with a URL that points at the test server but is shaped like a normal
// HTTP url. Since the production code does `JinaReaderHost + raw`, we
// can't easily redirect that without changing the const to a var.
//
// We change JinaReaderHost to a var below (see web_fetch.go) — but to
// keep the test file standalone, we test the simpler case: pointing at
// the test server directly while exercising the full HTTP path, error
// handling, and truncation.
func TestWebFetch_RequiresURL(t *testing.T) {
	_, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{})
	if err == nil {
		t.Fatal("missing url should error")
	}
}

func TestWebFetch_RejectsNonHTTPScheme(t *testing.T) {
	_, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": "ftp://example.com/file",
	})
	if err == nil || !strings.Contains(err.Error(), "only http/https") {
		t.Errorf("expected scheme rejection, got %v", err)
	}
}

func TestWebFetch_AgainstHTTPTest(t *testing.T) {
	// Stand up an httptest server that responds like Jina would — plain
	// Markdown. Then temporarily redirect JinaReaderHost to it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte("# Hello\n\nthis is markdown"))
	}))
	defer srv.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = srv.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": "https://example.com/article",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "Hello") || !strings.Contains(out.Text, "markdown") {
		t.Errorf("unexpected body: %q", out.Text)
	}
}

func TestWebFetch_RefusesCrossHostRedirect(t *testing.T) {
	// Destination server (a different host:port than the "jina" entry point).
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret internal content"))
	}))
	defer dest.Close()

	// Entry server stands in for r.jina.ai and 302s us to dest — a different
	// host. The web_fetch client must refuse to follow.
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusFound)
	}))
	defer entry.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = entry.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": "https://example.com/x",
	})
	if err == nil {
		t.Fatalf("expected cross-host redirect to be refused; got body %q", out.Text)
	}
	if !strings.Contains(err.Error(), "cross-host redirect") {
		t.Errorf("error should mention cross-host redirect, got %v", err)
	}
	if strings.Contains(out.Text, "secret internal content") {
		t.Errorf("must not have followed the redirect to the destination")
	}
}

func TestWebFetch_FollowsSameHostRedirect(t *testing.T) {
	// A redirect that stays on the same host (path change only) is fine.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("# arrived"))
	}))
	defer srv.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = srv.URL + "/start?u=" // shape: <host>/start?u=<rawURL>
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": "https://example.com/x",
	})
	if err != nil {
		t.Fatalf("same-host redirect should be followed: %v", err)
	}
	if !strings.Contains(out.Text, "arrived") {
		t.Errorf("expected to reach the same-host redirect target, got %q", out.Text)
	}
}

func TestWebFetch_HTTPErrorWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream is sad", http.StatusBadGateway)
	}))
	defer srv.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = srv.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	_, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": "https://example.com/x",
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("expected HTTP 502 error, got %v", err)
	}
}

func TestWebFetch_Truncates(t *testing.T) {
	// Generate a body larger than WebFetchMaxBytes.
	big := strings.Repeat("a", WebFetchMaxBytes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = srv.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": "https://example.com/big",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "truncated") {
		t.Errorf("expected truncation marker, got tail: %q", out.Text[max(0, len(out.Text)-100):])
	}
}

// ───────────────────── fallback tests ─────────────────────

func TestWebFetch_FallsBackOnTLSFailure(t *testing.T) {
	// Jina proxy returns a TLS error (simulated by a server that closes
	// immediately, causing a connection error that triggers fallback).
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force a connection reset by hijacking and closing.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack failed: %v", err)
		}
		conn.Close()
	}))
	defer jina.Close()

	// Direct server serves the real content.
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct fallback content"))
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out.Text, "direct fallback content") {
		t.Errorf("expected direct fallback content, got: %q", out.Text)
	}
}

func TestWebFetch_FallsBackOnHTTP5xx(t *testing.T) {
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "jina overloaded", http.StatusServiceUnavailable)
	}))
	defer jina.Close()

	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct ok"))
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out.Text, "direct ok") {
		t.Errorf("expected direct ok, got: %q", out.Text)
	}
}

func TestWebFetch_NoFallbackOnHTTP404(t *testing.T) {
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer jina.Close()

	// Even though direct would succeed, we should NOT fallback on 4xx.
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not reach here"))
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	_, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err == nil {
		t.Fatal("expected 404 error, got success")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 in error, got: %v", err)
	}
}

func TestWebFetch_BothFail(t *testing.T) {
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "jina bad", http.StatusBadGateway)
	}))
	defer jina.Close()

	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "direct bad", http.StatusInternalServerError)
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	_, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err == nil {
		t.Fatal("expected error when both fail")
	}
	if !strings.Contains(err.Error(), "jina proxy failed") {
		t.Errorf("error should mention jina proxy failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "direct fetch also failed") {
		t.Errorf("error should mention direct fetch failure, got: %v", err)
	}
}

func TestWebFetch_FallsBackOnHTTP429(t *testing.T) {
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer jina.Close()

	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct after rate limit"))
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out.Text, "direct after rate limit") {
		t.Errorf("expected direct content, got: %q", out.Text)
	}
}

func TestWebFetch_FallsBackOnConnectionResetByPeer(t *testing.T) {
	// Simulate the exact error Jina returns when the upstream resets the TCP
	// connection — this must trigger the direct-fetch fallback.
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack failed: %v", err)
		}
		conn.Close()
	}))
	defer jina.Close()

	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct after reset"))
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	out, err := WebFetchTool{}.Execute(context.Background(), "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out.Text, "direct after reset") {
		t.Errorf("expected direct content, got: %q", out.Text)
	}
}

func TestWebFetch_FallsBackOnContextDeadlineExceeded(t *testing.T) {
	// Jina proxy that sleeps longer than the 5s jina timeout but less than
	// the total budget, so the fallback has time to succeed.
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(6 * time.Second)
	}))
	defer jina.Close()

	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct after deadline"))
	}))
	defer direct.Close()

	old := jinaReaderHostForTest
	jinaReaderHostForTest = jina.URL + "/"
	defer func() { jinaReaderHostForTest = old }()

	// Total budget 10s — jina gets 5s, direct gets up to 30s (capped by this 10s).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := WebFetchTool{}.Execute(ctx, "web_fetch", map[string]any{
		"url": direct.URL + "/page",
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out.Text, "direct after deadline") {
		t.Errorf("expected direct content, got: %q", out.Text)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
