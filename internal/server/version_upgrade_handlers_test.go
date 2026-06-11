package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Leihb/octo-agent/internal/upgrade"
	"github.com/Leihb/octo-agent/internal/version"
)

// TestLatestVersion_ChecksAndCaches points the upgrade base URL at a fake
// release origin: with UpdateCheck on, the first lookup goes upstream, the
// second is served from cache (one upstream hit total), and needs_update
// reflects a release build behind latest.
func TestLatestVersion_ChecksAndCaches(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	var fake *httptest.Server
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, fake.URL+"/releases/tag/v9.9.9", http.StatusFound)
	})
	fake = httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	origURL := upgrade.BaseURL
	upgrade.BaseURL = fake.URL
	t.Cleanup(func() { upgrade.BaseURL = origURL })

	// Pin a release-like build so needsUpdate can fire (the test binary is
	// otherwise a dev build with no commit).
	origV, origC := version.Version, version.Commit
	version.Version, version.Commit = "0.18.0", "abc1234"
	t.Cleanup(func() { version.Version, version.Commit = origV, origC })

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false, UpdateCheck: true})

	latest, needs := srv.latestVersion()
	if latest != "9.9.9" || !needs {
		t.Fatalf("latestVersion = (%q, %v), want (9.9.9, true)", latest, needs)
	}
	if latest2, _ := srv.latestVersion(); latest2 != "9.9.9" {
		t.Fatalf("second lookup = %q, want cached 9.9.9", latest2)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (second lookup must be cached)", got)
	}
}

// TestLatestVersion_DevBuildNeverNags: even with an update available, a
// dev build reports needs_update=false — the badge must not offer an
// upgrade the backend would refuse.
func TestLatestVersion_DevBuildNeverNags(t *testing.T) {
	mux := http.NewServeMux()
	var fake *httptest.Server
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fake.URL+"/releases/tag/v9.9.9", http.StatusFound)
	})
	fake = httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	origURL := upgrade.BaseURL
	upgrade.BaseURL = fake.URL
	t.Cleanup(func() { upgrade.BaseURL = origURL })

	origV, origC := version.Version, version.Commit
	version.Version, version.Commit = "0.18.0-dev", "abc1234"
	t.Cleanup(func() { version.Version, version.Commit = origV, origC })

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false, UpdateCheck: true})
	if latest, needs := srv.latestVersion(); needs {
		t.Errorf("dev build reported needs_update for latest %q", latest)
	}
}
