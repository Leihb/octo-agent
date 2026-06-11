package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/upgrade"
	"github.com/Leihb/octo-agent/internal/version"
)

// fakeLatest serves only the releases/latest redirect — enough for --check.
func fakeLatest(t *testing.T, ver string) {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/releases/tag/v"+ver, http.StatusFound)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	orig := upgrade.BaseURL
	upgrade.BaseURL = srv.URL
	t.Cleanup(func() { upgrade.BaseURL = orig })
}

func TestRunUpgrade_Check(t *testing.T) {
	fakeLatest(t, "9.9.9")
	origV := version.Version
	version.Version = "0.18.0"
	t.Cleanup(func() { version.Version = origV })

	var stdout, stderr bytes.Buffer
	code := runUpgrade([]string{"--check"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"latest:  9.9.9", "update available"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunUpgrade_CheckUpToDate(t *testing.T) {
	fakeLatest(t, "0.18.0")
	origV := version.Version
	version.Version = "0.18.0"
	t.Cleanup(func() { version.Version = origV })

	var stdout, stderr bytes.Buffer
	if code := runUpgrade([]string{"--check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "up to date") {
		t.Errorf("expected up-to-date message, got:\n%s", stdout.String())
	}
}

func TestRunUpgrade_DevRefusalExitCode(t *testing.T) {
	// No fake server needed: the eligibility refusal fires before any
	// network access (the test binary has no release metadata). Pin the
	// origin to an unroutable address anyway.
	orig := upgrade.BaseURL
	upgrade.BaseURL = "http://127.0.0.1:0"
	t.Cleanup(func() { upgrade.BaseURL = orig })

	var stdout, stderr bytes.Buffer
	code := runUpgrade(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("refusal should hint at --force, got: %s", stderr.String())
	}
}

func TestRunUpgrade_BadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runUpgrade([]string{"--no-such-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2 for flag errors", code)
	}
}
