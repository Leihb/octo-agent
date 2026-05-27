# C11 — OS-level command sandbox (design)

Status: design agreed 2026-05-27. Implements roadmap item **C11** (= M6.5 P1-2).

## 1. Threat model & goals

The permission engine (`internal/permission`) gates the `terminal` tool at the
**policy layer** — allow/deny/ask on the command *string* (substring + glob
rules). That is bypassable: `python -c '…'`, `bash -c '…'`, a novel binary, or
any command whose dangerous effect isn't captured by a substring rule can slip
through an `allow`.

**Threat.** An LLM-issued or prompt-injected command that the permission engine
allows (or that evades its rules) does one of:
- writes/deletes files **outside the project** (e.g. `~`, `/etc`),
- reads **secrets** (`~/.ssh`, `~/.aws`, `~/.config`),
- **exfiltrates** data over the network.

**Goal.** Even an *allowed* command cannot, as enforced by the OS (not by string
matching):
- write outside an allowed set of roots,
- read outside an allowed set of roots,
- open an IP-network connection (deny by default; allowlist later).

This is **defense-in-depth**, layered under the permission engine — not an
absolute boundary against kernel exploits.

**Non-goals (this milestone).** Full container isolation, complete syscall
filtering, defeating local privilege escalation, sandboxing our own controlled
read-only helpers (`grep`'s `rg`). The boundary we care about is the
arbitrary-command surface: the `terminal` tool (foreground + background).

## 2. Decisions (agreed)

- **Rollout: opt-in.** A `--sandbox` flag, default **off**. The existing strict
  permission mode remains the backstop. Flip to default-on in a later phase once
  proven. Does not block other work.
- **Linux mechanism: Landlock re-exec shim** (no external dependency, fits the
  single-binary ethos; kernel ≥ 5.13). `bwrap` is *not* a dependency.
- **Phase 1 scope: filesystem boundary AND network deny together** (not FS only).

## 3. Integration point & abstraction

Two call sites build the arbitrary command today, both
`exec.CommandContext(ctx, "sh", "-c", command)`:
- `internal/tools/terminal.go` (foreground)
- `internal/tools/background.go` (background)

Both route through a new package:

```go
// internal/sandbox
type Policy struct {
    ReadRoots    []string // absolute roots readable (incl. system: /usr /bin /lib /etc, cwd, tmp)
    WriteRoots   []string // absolute roots writable (cwd, $TMPDIR)
    AllowNetwork bool     // false → deny IP networking
}

// Command wraps `sh -c command` so it runs confined to p. Returns an error
// when sandboxing was requested but is unavailable on this host (see §6).
func Command(ctx context.Context, command string, p Policy) (*exec.Cmd, error)

// Available reports whether this OS/kernel can enforce a sandbox.
func Available() bool

// DefaultPolicy builds the standard policy for a working directory.
func DefaultPolicy(cwd string) Policy
```

`DefaultPolicy(cwd)`:
- ReadRoots: `cwd`, `$TMPDIR`/`/tmp`, and read-only system roots
  (`/usr`, `/bin`, `/lib`, `/lib64`, `/etc`, `/opt`, `/System` on macOS, the Go
  toolchain dir). Generous on read so normal tooling works; the credential
  paths are excluded so secrets stay unreadable.
- WriteRoots: `cwd`, `$TMPDIR`/`/tmp`.
- AllowNetwork: false.

Explicitly **outside** ReadRoots: `~/.ssh`, `~/.aws`, `~/.config`, `~` in
general (only cwd + tmp are reachable under the home tree).

### Plumbing the policy to the tools

`TerminalTool` and `BackgroundManager` are stateless values in `allTools`, so —
mirroring the existing `defaultBg` pattern — the active policy lives as a
package-level value in `internal/tools`, set once at startup:

```go
// internal/tools
var activeSandbox *sandbox.Policy // nil → no sandbox (default)
func SetSandbox(p *sandbox.Policy) { activeSandbox = p }
```

`cmd/octo` calls `tools.SetSandbox(&policy)` when `--sandbox` is passed. Both
exec sites consult `activeSandbox`: nil → today's plain `exec.CommandContext`;
non-nil → `sandbox.Command(ctx, command, *activeSandbox)`.

## 4. macOS mechanism — Seatbelt (SBPL)

Wrap as `sandbox-exec -p <profile> sh -c <command>`. Generated profile:

```scheme
(version 1)
(deny default)
(allow process*)
(allow sysctl-read)
(allow file-read-metadata)
(allow file-read*  (subpath "<read-root>") …)
(allow file-write* (subpath "<write-root>") …)   ; cwd, tmp
; AllowNetwork=false → no network allows, default-deny covers it
; AllowNetwork=true  → (allow network*)
```

`sandbox-exec` is marked deprecated by Apple but has no replacement and remains
functional; it's the mechanism other agent tools use on macOS. `Available()`
checks the binary exists.

## 5. Linux mechanism — Landlock + seccomp re-exec shim

Landlock and seccomp must be applied to the **child** *after* fork and *before*
exec — Go's `os/exec` gives no hook there. Standard solution: a **self re-exec
shim**.

- A hidden subcommand, `octo __sandboxed-exec`, receives the policy (JSON via an
  env var or a fd) and the real command.
- In that child, before running anything else, it:
  1. applies a **Landlock** ruleset (`github.com/landlock-lsm/go-landlock`,
     pure Go) granting read/read-exec on ReadRoots and read-write on WriteRoots;
  2. if `AllowNetwork=false`, installs a minimal **seccomp** BPF filter (pure
     Go via `golang.org/x/sys/unix` `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER)`,
     no cgo) that **denies `socket(2)` for `AF_INET`/`AF_INET6`** (filtering on
     the domain arg) while allowing `AF_UNIX`;
  3. `syscall.Exec`s `sh -c command`.

`sandbox.Command` on Linux therefore returns
`exec.Command(self, "__sandboxed-exec", …)` where `self = os.Executable()`.

`Available()` on Linux probes Landlock support (attempt to create a ruleset;
ENOSYS/EOPNOTSUPP → unavailable, i.e. kernel < 5.13 or Landlock disabled).

Notes / risks:
- Network deny via seccomp socket-domain filtering is unprivileged and portable;
  a network *namespace* would need a user namespace (often restricted), so
  seccomp is the primary route. DNS is also blocked (it needs `AF_INET`), which
  is the intended outcome under `AllowNetwork=false`.
- seccomp can inspect scalar syscall args (domain) but not memory; filtering on
  `socket`'s domain is sufficient and pointer-safe.
- Landlock is path-based and unprivileged; no namespaces required.

## 6. Availability & fallback

`Available()==false` (kernel < 5.13, Landlock off, `sandbox-exec` missing,
Windows):
- **`--sandbox` explicitly requested → fail-closed**: refuse to run `terminal`,
  with a clear message (the user asked for a guarantee we can't provide).
- **`--sandbox=auto` (future) → warn once, fall back** to permission-engine-only.

## 7. Windows

No lightweight equivalent (Job Objects / AppContainer are heavyweight and
awkward). Phase 1: **unsupported** — `--sandbox` on Windows errors at startup;
default-off means normal use is unaffected. Revisit later if needed.

## 8. Phasing

- **Phase 1 (this milestone):** `internal/sandbox` abstraction + both exec sites
  + **FS read/write roots + network deny**, on macOS (SBPL) and Linux
  (landlock+seccomp shim); `--sandbox` flag on `octo chat` and `octo init`;
  `Available()` probe + fail-closed; Windows unsupported.
- **Phase 2:** network **allowlist** (specific hosts) rather than all-or-nothing
  — SBPL host rules / seccomp+resolver or a proxy.
- **Phase 3:** default-on + configurable policy (e.g. a `sandbox:` block read
  near `.octorules`, extra roots, per-project network allowlist).

## 9. Testing

OS-specific and not fully coverable in one CI runner. Plan:
- `//go:build darwin` / `linux` / `other` split implementations; a portable
  `Available()`-gated path.
- Capability-gated integration tests that run **only** where the mechanism
  works (skip otherwise): assert "write a file **inside** cwd succeeds", "write
  **outside** cwd (e.g. `$HOME/x`) fails", "read `~/.ssh/known_hosts` fails",
  and (network) "`curl`/a TCP connect fails" under `AllowNetwork=false`.
- Windows / old-kernel: test the **fail-closed** branch (Available()==false →
  `Command` errors / startup refuses).
- The re-exec shim is exercised by spawning the test binary's own
  `__sandboxed-exec` path.

## 10. New dependencies

- `github.com/landlock-lsm/go-landlock` (pure Go, Linux only via build tags).
- seccomp filter hand-rolled on `golang.org/x/sys/unix` (no cgo).
- macOS/Windows pull in neither (build-tagged out).

## 11. Task breakdown (Phase 1)

1. `internal/sandbox`: `Policy`, `DefaultPolicy`, `Command`, `Available` + the
   build-tagged darwin/linux/other files.
2. macOS SBPL profile generator + `sandbox-exec` wrapper.
3. Linux landlock+seccomp + the `octo __sandboxed-exec` hidden subcommand.
4. `tools.SetSandbox` + both exec sites consult `activeSandbox`.
5. `--sandbox` flag on `chat` and `init`; build `DefaultPolicy(cwd)`; call
   `SetSandbox`; fail-closed when unavailable.
6. Tests (capability-gated integration + fail-closed unit).
