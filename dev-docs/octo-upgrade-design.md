# Binary Upgrade (`octo upgrade`)

octo upgrades itself: one CLI command or one click in the web UI downloads
the latest GitHub release, verifies its SHA-256 against the release's
`checksums.txt`, swaps the binary on disk, and — for a running server — the
self-restart machinery respawns the worker from the new binary.

## Goals

- **One step from release to release.** `octo upgrade` in a terminal, or
  badge → Upgrade → Restart in the web UI (amber dot, live log, restart
  button — version.js drives the whole flow).
- **Verified download.** SHA-256 of the archive checked against the same
  release's `checksums.txt`, both fetched over GitHub TLS. No release
  pipeline changes.
- **All three platforms.** Including Windows, where a running executable
  can be renamed but not deleted.
- **Safe failure.** Any error before the final rename leaves the installed
  binary untouched; a failed final swap rolls back.

## Non-goals

- **Signature verification.** The trust anchor is GitHub's TLS plus the
  release assets' integrity; a compromised GitHub account could serve a
  malicious release and this design would install it. Documented in
  SECURITY.md as a residual risk; cosign/minisign can be layered on later
  without changing this flow.
- **Auto-update.** Upgrades run only on explicit user action. The version
  *check* is automatic (web badge), the *install* never is.
- **Arbitrary versions / downgrade.** Latest release only.
- **Upgrading the supervisor process.** After a swap + restart, the worker
  runs the new binary; the supervisor parent keeps running the old code
  until the next full process restart. It is a ~100-line spawn-wait loop by
  design, so staleness there is acceptable (per the self-restart design).

## The release contract

goreleaser publishes, per tag `v{ver}`:

- `octo_{ver}_{os}_{arch}.tar.gz` (Windows: `.zip`), containing the `octo`
  (`octo.exe`) binary plus docs.
- `checksums.txt` — `sha256  filename` lines covering every archive.

`https://github.com/Leihb/octo-agent/releases/latest` redirects to
`…/releases/tag/v{ver}`; assets download from
`…/releases/download/v{ver}/{asset}`.

## Design

### `internal/upgrade` package

The logic lives in one package used by both the CLI and the server.

- **`Check(ctx) (latest string, err error)`** — issues a no-redirect GET to
  `releases/latest` and parses the version out of the `Location` header.
  No GitHub API, no JSON, no rate-limit coupling. The base URL is a
  package var so tests point it at `httptest`.

- **`Run(ctx, Options) error`** with
  `Options{Force bool, Log func(string), TargetPath string}` —
  `TargetPath` defaults to `os.Executable()` and exists so tests can swap a
  temp file instead of the running test binary:
  1. **Eligibility.** A `-dev`/`-snapshot` version or empty `Commit`
     refuses with a pointer to `--force` (a local build would be silently
     replaced by an older release otherwise). Already-at-latest refuses
     too (`--force` reinstalls).
  2. **Resolve** the asset name from `runtime.GOOS`/`GOARCH` and the latest
     version.
  3. **Download** the archive and `checksums.txt` to a temp dir, with
     `version.UserAgent()` and a context deadline.
  4. **Verify** the archive's SHA-256 against its `checksums.txt` line; a
     missing entry or mismatch aborts.
  5. **Extract** the binary member (`octo` / `octo.exe`) from the tar.gz /
     zip.
  6. **Swap.** Target is the executable path with symlinks resolved. A
     lock file (`<target>.upgrade.lock`, `O_CREATE|O_EXCL`, stale after 10
     minutes) excludes a concurrent CLI and server upgrade racing on the
     same path. The new binary is written next to the target (same
     filesystem, so renames are atomic), fsynced, chmod 0755; then
     `target → target.old.<unixtime>`, `new → target`. If the second
     rename fails, the first is reversed. The aside name is unique because
     Windows keeps the *running* image locked against delete and
     rename-over — a fixed `.old` name would make every upgrade after the
     first fail there. Stale `.old.*` siblings are swept best-effort on
     each run (the locked one survives the sweep until the process is
     gone).

  Every step reports through `Log` — the CLI prints lines, the server
  broadcasts them.

Version comparison is numeric SemVer on the `major.minor.patch` triple
(prerelease suffix ⇒ ineligible without `--force`), so a snapshot build
"newer than latest" doesn't prompt a downgrade.

### CLI: `octo upgrade`

- `octo upgrade` — check, download, verify, swap; progress to stdout. Ends
  with the installed version and a reminder that a running `octo serve`
  picks the new binary up on its next restart (`POST /api/restart` or the
  `restart_server` tool).
- `octo upgrade --check` — print current vs latest and whether an update
  exists; no changes.
- `octo upgrade --force` — proceed despite a dev build or already-latest.
  `--force` does not validate the install location: under `go run` the
  executable lives in a transient build dir and the "install" evaporates
  with it.

Exit codes follow the CLI contract: 0 success (including "already up to
date"), 1 failure, 2 flag errors.

### Server

- **`GET /api/version`** returns
  `{version, current, latest, needs_update, cli_command}` — `version` is
  kept for compatibility, `current`/`latest`/`needs_update` are what the
  web badge reads, `cli_command` is the constant `"octo"` (the endpoint is
  unauthenticated; the executable's filesystem path must not leak there).
  `latest` comes from `upgrade.Check` through a cache: results cached
  1 hour, failures 10 minutes, lookups single-flight with a short timeout.
  The cache is also the guard against a request flood becoming an
  outbound-request flood; on any check failure the response degrades to
  `latest == current, needs_update == false`.

  The update check is **opt-in via `server.Config.UpdateCheck`**, set only
  by `octo serve`. Everything else that constructs a `Server` — the whole
  test suite included — gets no outbound calls, which is what keeps the
  "no live network in `go test`" rule intact for every existing test that
  hits `/api/version`. Disabled means the degraded response.
- **`POST /api/version/upgrade`** (auth required, like restart) replaces
  the stub: it 202s and runs `upgrade.Run` in a goroutine, single-flight —
  a second POST while one runs gets 409. The goroutine holds
  `drain.begin()/end()` for the duration of the swap, the same gate turn
  execution holds: a restart drains behind an in-flight upgrade instead of
  exiting between the two renames (which would leave **no file at the
  target path** and make the supervisor respawn fail outright), and an
  upgrade attempted during a restart is refused. `Log` lines broadcast as
  `{type: "upgrade_log", line}` WS events; completion as
  `{type: "upgrade_complete", success}` — the events version.js handles.
  Every refusal past auth — dev build, already-latest, drain in progress —
  also ends in a broadcast `upgrade_complete{success:false}` carrying the
  reason as a log line, because the frontend ignores the POST's status
  code and only a completion event unwedges its popover; as part of this
  work `_startUpgrade` also gains a `res.ok` check so a synchronous 4xx
  (expired cookie) can't leave a permanent spinner. There is no force
  option over HTTP. After success the UI shows the restart button, and
  `/api/restart` → drain → exit 42 → supervisor respawn boots the new
  binary.

The unwired `handleVersionExtended` stub (whose `latest_version` field
never matched what version.js reads) is deleted along with the upgrade
stub; `TestHandleVersionUpgrade`, which asserts the stub's body, is
rewritten against the new contract.

### Failure modes

| Failure | Outcome |
|---|---|
| Network/404 during check or download | Error before any write; binary untouched |
| Checksum mismatch | Abort with both digests in the message; binary untouched |
| Target dir not writable (system installs, e.g. /usr/local/bin owned by root) | Swap fails cleanly; error suggests rerunning with appropriate permissions or manual install |
| Crash between the two renames | `target.old.<ts>` still holds the previous binary; the error path restores it, and even unrestored it remains on disk for manual recovery. In the server this window is drain-gated, so a restart cannot interleave with it |
| Restart requested mid-upgrade | The restart drain waits for the swap to finish (same gate as turns); the respawn then boots whichever binary the completed swap left in place |
| Server upgraded but never restarted | Old binary keeps running; badge stays in needs-restart state |

## Tests

No live network (repo rule): an `httptest` server fakes GitHub — the
`releases/latest` redirect, the archive (built by the test), and a matching
`checksums.txt` — with the package base-URL var pointed at it.

- `Check` parses the redirect; non-redirect and error responses fail.
- `Run` end-to-end against a temp "installed binary" (via
  `Options.TargetPath`): swap succeeds, old content preserved under
  `.old.*`, new content executable, lock file released.
- Checksum mismatch, missing checksums entry, missing asset → error and
  the target file's bytes are unchanged.
- Dev-version refusal; `--force` override; already-latest short-circuit.
- SemVer comparison table (incl. prerelease suffixes).
- Server: `/api/version` shape + cache (second request hits no upstream),
  upgrade endpoint single-flight 409, WS event shapes.

## Rollout

- CHANGELOG: `octo upgrade` + the web upgrade flow.
- README install section gains "upgrading: `octo upgrade`".
- SECURITY.md: note the update channel's trust anchor (GitHub TLS +
  checksums; no signatures yet).
- COMPATIBILITY.md: `upgrade` joins the documented (Stable) subcommands.
