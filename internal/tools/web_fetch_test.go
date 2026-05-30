package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
