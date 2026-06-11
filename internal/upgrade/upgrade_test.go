package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/version"
)

// fakeRelease stands up an httptest GitHub: the releases/latest redirect,
// one platform archive containing the binary member, and checksums.txt.
// sumOverride, when non-empty, replaces the real checksum line.
func fakeRelease(t *testing.T, ver, binContent, sumOverride string) *httptest.Server {
	t.Helper()
	asset := AssetName(ver)
	archive := buildArchive(t, asset, binaryName(), binContent)

	sum := sha256.Sum256(archive)
	line := hex.EncodeToString(sum[:])
	if sumOverride != "" {
		line = sumOverride
	}
	sums := fmt.Sprintf("%s  %s\n", line, asset)

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/releases/tag/v"+ver, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v"+ver+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/releases/download/v"+ver+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sums))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// buildArchive produces a tar.gz (or zip, matching AssetName's extension)
// holding the binary member plus a README, mirroring the goreleaser layout.
func buildArchive(t *testing.T, asset, member, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(asset, ".zip") {
		zw := zip.NewWriter(&buf)
		for name, data := range map[string]string{member: content, "README.md": "readme"} {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(data)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range map[string]string{member: content, "README.md": "readme"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// asRelease pins the version vars to a release build for the test's
// duration. Tests mutating package state must not run in parallel.
func asRelease(t *testing.T, ver, commit string) {
	t.Helper()
	origV, origC := version.Version, version.Commit
	version.Version, version.Commit = ver, commit
	t.Cleanup(func() { version.Version, version.Commit = origV, origC })
}

func pointAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := BaseURL
	BaseURL = srv.URL
	t.Cleanup(func() { BaseURL = orig })
}

func writeTarget(t *testing.T, content string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "octo")
	if err := os.WriteFile(target, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestCheck(t *testing.T) {
	srv := fakeRelease(t, "0.19.0", "bin", "")
	pointAt(t, srv)

	got, err := Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.19.0" {
		t.Errorf("Check = %q, want 0.19.0", got)
	}
}

func TestCheck_NoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	pointAt(t, srv)

	if _, err := Check(context.Background()); err == nil {
		t.Fatal("expected error on non-redirect response")
	}
}

func TestRun_EndToEnd(t *testing.T) {
	asRelease(t, "0.18.0", "abc1234")
	srv := fakeRelease(t, "0.19.0", "NEW BINARY", "")
	pointAt(t, srv)
	target := writeTarget(t, "OLD BINARY")

	var lines []string
	err := Run(context.Background(), Options{TargetPath: target, Log: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatalf("Run: %v (log: %v)", err, lines)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Errorf("target content = %q, want NEW BINARY", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("target not executable: %v", info.Mode())
	}
	if _, err := os.Stat(target + ".upgrade.lock"); !os.IsNotExist(err) {
		t.Error("lock file not released")
	}
	// The aside is removed after a successful swap (POSIX; on Windows the
	// running image would survive until the next upgrade's sweep — but the
	// target here is not a running image on any platform).
	if asides, _ := filepath.Glob(target + ".old.*"); len(asides) != 0 {
		t.Errorf("aside not cleaned up: %v", asides)
	}
	if staged, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".octo.new.*")); len(staged) != 0 {
		t.Errorf("staging file not cleaned up: %v", staged)
	}
}

func TestRun_UpToDate(t *testing.T) {
	asRelease(t, "0.19.0", "abc1234")
	srv := fakeRelease(t, "0.19.0", "NEW", "")
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	err := Run(context.Background(), Options{TargetPath: target})
	if !errors.Is(err, ErrUpToDate) {
		t.Fatalf("err = %v, want ErrUpToDate", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Error("target modified on up-to-date")
	}
}

func TestRun_NewerThanLatest(t *testing.T) {
	asRelease(t, "0.20.0", "abc1234")
	srv := fakeRelease(t, "0.19.0", "NEW", "")
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	if err := Run(context.Background(), Options{TargetPath: target}); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("snapshot newer than latest should be up-to-date, got %v", err)
	}
}

func TestRun_DevRefusalAndForce(t *testing.T) {
	asRelease(t, "0.19.0-dev", "abc1234")
	srv := fakeRelease(t, "0.19.0", "NEW", "")
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	err := Run(context.Background(), Options{TargetPath: target})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("dev build should refuse with a --force hint, got %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatal("target modified by refused run")
	}

	if err := Run(context.Background(), Options{TargetPath: target, Force: true}); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "NEW" {
		t.Error("forced run did not install")
	}
}

func TestRun_NoLdflagsBuildRefuses(t *testing.T) {
	asRelease(t, "0.19.0", "") // plain version, empty commit = bare go build
	srv := fakeRelease(t, "0.20.0", "NEW", "")
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	if err := Run(context.Background(), Options{TargetPath: target}); err == nil {
		t.Fatal("empty-commit build should refuse")
	}
}

func TestRun_ChecksumMismatch(t *testing.T) {
	asRelease(t, "0.18.0", "abc1234")
	srv := fakeRelease(t, "0.19.0", "NEW", strings.Repeat("0", 64))
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	err := Run(context.Background(), Options{TargetPath: target})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Error("target modified despite checksum mismatch")
	}
}

func TestRun_MissingChecksumEntry(t *testing.T) {
	asRelease(t, "0.18.0", "abc1234")
	ver := "0.19.0"
	asset := AssetName(ver)
	archive := buildArchive(t, asset, binaryName(), "NEW")
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/releases/tag/v"+ver, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v"+ver+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/releases/download/v"+ver+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("deadbeef  some_other_asset.tar.gz\n"))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	err := Run(context.Background(), Options{TargetPath: target})
	if err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("err = %v, want missing-entry error", err)
	}
}

func TestRun_MissingAsset(t *testing.T) {
	asRelease(t, "0.18.0", "abc1234")
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/releases/tag/v0.19.0", http.StatusFound)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	pointAt(t, srv)
	target := writeTarget(t, "OLD")

	if err := Run(context.Background(), Options{TargetPath: target}); err == nil {
		t.Fatal("expected error on 404 asset")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Error("target modified despite download failure")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.18.0", "0.19.0", -1},
		{"0.19.0", "0.19.0", 0},
		{"0.20.0", "0.19.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.19.0-dev", "0.19.0", 0}, // prerelease handled by Eligible, not here
		{"v0.19.0", "0.19.0", 0},
		{"0.19.1", "0.19.0", 1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestAcquireLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "octo.upgrade.lock")

	unlock, err := acquireLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(lock); err == nil {
		t.Fatal("second acquire should fail while held")
	}
	unlock()
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("unlock did not remove the lock file")
	}

	// Stale lock is stolen.
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-lockStaleAfter - time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err = acquireLock(lock)
	if err != nil {
		t.Fatalf("stale lock should be stolen: %v", err)
	}
	unlock()
}

func TestSweepStale(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "octo")
	for _, n := range []string{target + ".old.1", target + ".old.2", filepath.Join(dir, ".octo.new.99")} {
		if err := os.WriteFile(n, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sweepStale(target)
	left, _ := filepath.Glob(target + ".old.*")
	stagedLeft, _ := filepath.Glob(filepath.Join(dir, ".octo.new.*"))
	if len(left)+len(stagedLeft) != 0 {
		t.Errorf("sweep left %v %v", left, stagedLeft)
	}
}
