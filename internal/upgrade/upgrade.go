// Package upgrade replaces the running octo binary with the latest GitHub
// release: resolve the latest version from the releases/latest redirect,
// download the platform archive plus checksums.txt, verify the archive's
// SHA-256, extract the binary, and atomically swap it into place. The
// trust anchor is GitHub's TLS plus the release assets' integrity — there
// is no signature layer. See dev-docs/octo-upgrade-design.md.
package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/version"
)

// BaseURL is the release origin. A var so tests point it at httptest.
var BaseURL = "https://github.com/Leihb/octo-agent"

// maxArchiveSize caps the download; a release archive is ~15 MB, so this is
// generous headroom, not a real limit.
const maxArchiveSize = 512 << 20

// lockStaleAfter is how old a leftover .upgrade.lock must be before a new
// run steals it (a crashed upgrade never removed its lock).
const lockStaleAfter = 10 * time.Minute

// ErrUpToDate reports that the installed version is already the latest.
// Callers treat it as success, not failure.
var ErrUpToDate = errors.New("already up to date")

// Options configures Run.
type Options struct {
	// Force proceeds despite a dev build or an already-latest version.
	Force bool
	// Log receives one human-readable progress line per step. Nil is fine.
	Log func(string)
	// TargetPath overrides the binary to replace. Defaults to
	// os.Executable() (symlinks resolved); tests point it at a temp file.
	TargetPath string
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(format, args...))
	}
}

// Check resolves the latest released version (no leading "v") by following
// none of the releases/latest redirect: the target tag is in the Location
// header. No GitHub API, so no rate-limit coupling.
func Check(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("releases/latest: expected redirect, got %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("releases/latest: unexpected redirect target %q", loc)
	}
	tag := strings.TrimPrefix(loc[i+len("/tag/"):], "v")
	if tag == "" {
		return "", fmt.Errorf("releases/latest: empty tag in %q", loc)
	}
	return tag, nil
}

// Eligible reports whether the running build may be auto-upgraded: a
// release build carries a plain SemVer version AND a commit SHA. Dev
// builds (-dev/-snapshot suffix from make, or a bare `go build` with no
// ldflags at all) refuse so a local build isn't silently replaced by an
// older release.
func Eligible() error {
	if strings.Contains(version.Version, "-") {
		return fmt.Errorf("this is a dev build (%s); use --force to replace it with the latest release", version.Version)
	}
	if version.Commit == "" {
		return fmt.Errorf("this build carries no release metadata (built without ldflags); use --force to replace it with the latest release")
	}
	return nil
}

// CompareVersions returns -1/0/+1 for a<b, a==b, a>b on the numeric
// major.minor.patch triple. Non-numeric parts compare as 0.
func CompareVersions(a, b string) int {
	pa, pb := parseTriple(a), parseTriple(b)
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func parseTriple(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		out[i] = n
	}
	return out
}

// AssetName returns the release archive for this platform and version.
func AssetName(ver string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("octo_%s_%s_%s.%s", ver, runtime.GOOS, runtime.GOARCH, ext)
}

// binaryName is the archive member to install.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "octo.exe"
	}
	return "octo"
}

// Prepared holds a verified, extracted new binary ready to install. The
// split from Run exists so the server can do the slow part (download)
// outside the restart drain gate and hold the gate only for the
// seconds-long Install.
type Prepared struct {
	// Latest is the version Install will put in place.
	Latest string

	opts   Options
	newBin string
	target string
	tmpDir string
}

// Prepare checks eligibility, resolves the latest release, downloads and
// verifies the archive, and extracts the binary — everything except
// touching the installed file. ErrUpToDate means nothing needs doing.
// Callers must Close the result.
func Prepare(ctx context.Context, opts Options) (*Prepared, error) {
	if !opts.Force {
		if err := Eligible(); err != nil {
			return nil, err
		}
	}

	latest, err := Check(ctx)
	if err != nil {
		return nil, fmt.Errorf("check latest release: %w", err)
	}
	current := strings.TrimPrefix(version.Version, "v")
	opts.log("current %s, latest %s", version.String(), latest)
	if !opts.Force && CompareVersions(current, latest) >= 0 {
		return nil, ErrUpToDate
	}

	target := opts.TargetPath
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		target = exe
	}

	tmpDir, err := os.MkdirTemp("", "octo-upgrade-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	asset := AssetName(latest)
	opts.log("downloading %s", asset)
	archivePath := filepath.Join(tmpDir, asset)
	if err := download(ctx, releaseURL(latest, asset), archivePath); err != nil {
		cleanup()
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	sumsPath := filepath.Join(tmpDir, "checksums.txt")
	if err := download(ctx, releaseURL(latest, "checksums.txt"), sumsPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("download checksums.txt: %w", err)
	}

	opts.log("verifying SHA-256")
	if err := verifyChecksum(archivePath, sumsPath, asset); err != nil {
		cleanup()
		return nil, err
	}

	opts.log("extracting %s", binaryName())
	newBin := filepath.Join(tmpDir, binaryName())
	if err := extractBinary(archivePath, binaryName(), newBin); err != nil {
		cleanup()
		return nil, fmt.Errorf("extract: %w", err)
	}

	return &Prepared{Latest: latest, opts: opts, newBin: newBin, target: target, tmpDir: tmpDir}, nil
}

// Install swaps the prepared binary into place. Fast (local renames
// only), so a caller holding a drain gate holds it for seconds, not for
// the download.
func (p *Prepared) Install() error {
	p.opts.log("installing to %s", p.target)
	if err := swap(p.newBin, p.target); err != nil {
		return err
	}
	p.opts.log("upgraded to %s", p.Latest)
	return nil
}

// Close removes the temp download dir.
func (p *Prepared) Close() {
	if p.tmpDir != "" {
		_ = os.RemoveAll(p.tmpDir)
	}
}

// Run upgrades the binary at opts.TargetPath (default: this executable) to
// the latest release: Prepare + Install in one call, for callers with no
// gate to scope (the CLI). ErrUpToDate means nothing was done because
// nothing needed doing.
func Run(ctx context.Context, opts Options) error {
	p, err := Prepare(ctx, opts)
	if err != nil {
		return err
	}
	defer p.Close()
	return p.Install()
}

func releaseURL(ver, asset string) string {
	return BaseURL + "/releases/download/v" + ver + "/" + asset
}

// download fetches url into path. The size cap guards against a hostile
// redirect target, not against GitHub.
func download(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxArchiveSize)); err != nil {
		return err
	}
	return f.Close()
}

// verifyChecksum compares the file's SHA-256 with its checksums.txt entry.
func verifyChecksum(archivePath, sumsPath, asset string) error {
	want, err := checksumFor(sumsPath, asset)
	if err != nil {
		return err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, want)
	}
	return nil
}

func checksumFor(sumsPath, asset string) (string, error) {
	f, err := os.Open(sumsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", asset)
}

// extractBinary pulls the named member out of a tar.gz or zip archive.
func extractBinary(archivePath, member, dest string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, member, dest)
	}
	return extractTarGz(archivePath, member, dest)
}

func extractTarGz(archivePath, member, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) != member || !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		return writeFile(dest, io.LimitReader(tr, maxArchiveSize))
	}
	return fmt.Errorf("archive has no %s member", member)
}

func extractZip(archivePath, member, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if filepath.Base(zf.Name) != member || zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeFile(dest, io.LimitReader(rc, maxArchiveSize))
	}
	return fmt.Errorf("archive has no %s member", member)
}

func writeFile(dest string, r io.Reader) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// swap installs newBin at target: write a sibling copy (same filesystem,
// so the renames are atomic), move the current binary aside under a unique
// .old name, and rename the copy into place. The aside name is unique
// because Windows keeps the running image locked against delete and
// rename-over — a fixed name would make every upgrade after the first
// fail there. A lock file excludes a concurrent upgrade racing on the
// same target.
func swap(newBin, target string) error {
	unlock, err := acquireLock(target + ".upgrade.lock")
	if err != nil {
		return err
	}
	defer unlock()

	sweepStale(target)

	staged := filepath.Join(filepath.Dir(target), fmt.Sprintf(".%s.new.%d", filepath.Base(target), os.Getpid()))
	if err := copyFile(staged, newBin); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	defer os.Remove(staged) // no-op after the successful rename

	aside := fmt.Sprintf("%s.old.%d.%d", target, time.Now().Unix(), os.Getpid())
	if err := os.Rename(target, aside); err != nil {
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		// Put the old binary back so the install stays usable.
		if rbErr := os.Rename(aside, target); rbErr != nil {
			return fmt.Errorf("install new binary: %w (rollback also failed: %v; previous binary is at %s)", err, rbErr, aside)
		}
		return fmt.Errorf("install new binary: %w (rolled back)", err)
	}
	// Best-effort: on Windows a still-running old image survives this and
	// is swept by the next upgrade once the process is gone.
	_ = os.Remove(aside)
	return nil
}

// copyFile writes src's bytes to dest (0755, fsynced).
func copyFile(dest, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// Explicit chmod: the OpenFile mode is masked by umask, and a 077
	// umask would otherwise install a 0700 binary into a shared dir.
	if err := out.Chmod(0o755); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// acquireLock takes <target>.upgrade.lock exclusively, stealing it when
// stale (a crashed upgrade never cleaned up). Stealing goes through a
// rename so two processes finding the same stale lock can't both win —
// only one rename succeeds.
func acquireLock(path string) (func(), error) {
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			// Not contention — most likely an unwritable install dir
			// (system path owned by root, read-only mount).
			return nil, fmt.Errorf("create lock %s: %w (is the install directory writable?)", path, err)
		}
		if attempt > 0 {
			return nil, fmt.Errorf("another upgrade appears to be running (%s exists)", path)
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) < lockStaleAfter {
			return nil, fmt.Errorf("another upgrade appears to be running (%s exists)", path)
		}
		// Stale — claim it via rename (single winner) and retry once.
		stale := path + ".stale"
		if err := os.Rename(path, stale); err != nil {
			return nil, fmt.Errorf("another upgrade appears to be running (%s exists)", path)
		}
		_ = os.Remove(stale)
	}
}

// sweepStale removes leftovers from previous upgrades: .old.* asides and
// orphaned .new.* staging files (a crash between stage and rename). The
// aside a running process still occupies refuses deletion (Windows) and
// survives until that process exits. Runs under the upgrade lock, so a
// concurrent upgrade's files can't be swept.
func sweepStale(target string) {
	for _, pattern := range []string{
		target + ".old.*",
		filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".new.*"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}
