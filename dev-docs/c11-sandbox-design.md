# OS-level command sandbox

An opt-in `--sandbox` confines the `terminal` tool's commands at the OS layer:
even a permission-allowed command can't write/read outside an allowed set of
roots or open a network connection. Defense-in-depth under the permission
engine — not an absolute boundary against kernel exploits.

## 1. Threat model & goals

The permission engine (`internal/permission`) gates `terminal` at the **policy
layer** — allow/deny/ask on the command *string* (substring + glob rules). That
is bypassable: `python -c '…'`, `bash -c '…'`, a novel binary, or any command
whose dangerous effect isn't captured by a substring rule can slip through an
`allow`.

**Threat.** An LLM-issued or prompt-injected command the engine allows (or that
evades its rules) writes/deletes files outside the project (`~`, `/etc`), reads
secrets (`~/.ssh`, `~/.aws`, `~/.config`), or exfiltrates over the network.

**Goal.** As enforced by the OS (not string matching), even an *allowed* command
cannot write outside an allowed root set, read outside an allowed root set, or
open an IP-network connection (deny by default).

**Non-goals.** Full container isolation, complete syscall filtering, defeating
local privilege escalation. The boundary is the arbitrary-command surface: the
`terminal` tool (foreground + background).

The sandbox is **opt-in** (`--sandbox`, default off); the permission engine +
interactive mode is the always-on backstop. It is not default-on because the network
story is an all-or-nothing toggle (§5), so a default sandbox would break every
network-needing command (`go mod download`, `git fetch`).

## 2. Integration point & abstraction

Both command sites — `internal/tools/terminal.go` (foreground) and
`internal/tools/background.go` (background) — build `sh -c command` and route
through `internal/sandbox`:

```go
type Policy struct {
    ReadRoots    []string // absolute roots readable (cwd, tmp, OS system roots)
    WriteRoots   []string // absolute roots writable (cwd, $TMPDIR)
    AllowNetwork bool     // false → deny IP networking
}

// Command wraps `sh -c command` so it runs confined to p. Errors when
// sandboxing was requested but is unavailable on this host (see §6).
func Command(ctx context.Context, command string, p Policy) (*exec.Cmd, error)

// Available reports whether this OS/kernel can enforce a sandbox.
func Available() bool

// DefaultPolicy builds the standard policy for a working directory.
func DefaultPolicy(cwd string) Policy
```

`DefaultPolicy(cwd)`:

- **ReadRoots**: `cwd`, `$TMPDIR`/`/tmp`, and OS-specific read-only system roots
  (`systemReadRoots()` — `/usr /bin /lib /etc …` plus per-OS additions:
  `/sbin /var /private /Library` on darwin, `/sbin /proc` on linux). Generous on
  read so normal tooling works; credential paths stay excluded.
- **WriteRoots**: `cwd`, `$TMPDIR`/`/tmp`.
- **AllowNetwork**: false.

Explicitly outside ReadRoots: `~/.ssh`, `~/.aws`, `~/.config`, `~` in general
(only cwd + tmp are reachable under the home tree).

`TerminalTool` and `BackgroundManager` are stateless values, so the active policy
lives package-level (mirroring `defaultBg`), set once at startup:

```go
// internal/tools
var activeSandbox *sandbox.Policy // nil → no sandbox (default)
func SetSandbox(p *sandbox.Policy) { activeSandbox = p }
```

`cmd/octo` calls `tools.SetSandbox(&policy)` when `--sandbox` is passed. Both
exec sites consult `activeSandbox`: nil → plain `exec.CommandContext`; non-nil →
`sandbox.Command(ctx, command, *activeSandbox)`.

## 3. macOS mechanism — Seatbelt (SBPL)

Wrap as `sandbox-exec -p <profile> sh -c <command>`. A full `(deny default)`
profile aborts most binaries (dyld / `/dev` / mach lookups it can't predict), so
the profile uses an **`allow default` base** and tightens just the axes we care
about:

```scheme
(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath "<write-root>") …)   ; cwd, tmp
(allow file-write* (literal "/dev/null") …)        ; device nodes
(deny file-read*  (subpath "<HOME>"))              ; confine reads within $HOME …
(allow file-read* (subpath "<read-root>") …)       ; … but re-allow the roots
(deny network*)                                    ; unless AllowNetwork
```

Writes are a strict allowlist (deny-all then re-allow roots; the later, more
specific rule wins). Reads are confined **within `$HOME`**: everything under
`$HOME` is denied except the read roots, protecting every home secret while
system paths (`/usr`, `/System`) stay readable via `allow default`. This is the
one place macOS is looser than Linux: Linux's Landlock enforces a *full* read
allowlist; macOS confines reads to `$HOME`.

`sandbox-exec` is Apple-deprecated but has no replacement and remains functional;
it's the mechanism other agent tools use on macOS. `Available()` checks the
binary exists.

## 4. Linux mechanism — Landlock + seccomp re-exec shim

Landlock and seccomp must apply to the **child** after fork and before exec —
`os/exec` gives no hook there, so octo re-execs itself: the hidden subcommand
`octo __sandboxed-exec` (`sandbox.ShimMain`, dispatched in `cmd/octo/main.go`) receives
the policy and the real command, and before running anything else:

1. applies a **Landlock** ruleset granting read/read-exec on ReadRoots and
   read-write on WriteRoots — via **raw Landlock syscalls on
   `golang.org/x/sys/unix`** (`SYS_LANDLOCK_CREATE_RULESET` / `ADD_RULE` /
   `RESTRICT_SELF`); no third-party library, no cgo;
2. if `AllowNetwork=false`, installs a minimal **seccomp** BPF filter (also via
   `x/sys/unix` `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER)`) that denies
   `socket(2)` for `AF_INET`/`AF_INET6` (filtering on the domain arg) while
   allowing `AF_UNIX`;
3. `syscall.Exec`s `sh -c command`.

So `sandbox.Command` on Linux returns `exec.Command(self, "__sandboxed-exec", …)`
with `self = os.Executable()`. `Available()` probes Landlock via
`LANDLOCK_CREATE_RULESET_VERSION` (ABI ≥ 1 → supported; ENOSYS/EOPNOTSUPP →
kernel < 5.13 or Landlock disabled).

Notes:

- Network deny via seccomp socket-domain filtering is unprivileged and portable;
  a network *namespace* would need a user namespace (often restricted). DNS is
  also blocked (needs `AF_INET`), the intended outcome under `AllowNetwork=false`.
- seccomp inspects scalar syscall args (domain) but not memory; filtering on
  `socket`'s domain is sufficient and pointer-safe.
- Landlock is path-based and unprivileged; no namespaces required. A known gap:
  32-bit/compat syscalls can bypass a seccomp filter written for the native ABI.

## 5. Network: all-or-nothing

The network story is the `--sandbox-allow-net` toggle, not a per-host allowlist.
Per-host filtering isn't feasible as a hard OS boundary: Linux seccomp can't
inspect `connect()`'s destination (sockaddr is a pointer), and macOS SBPL filters
by IP/port, not hostname. The only routes are a local HTTP(S) proxy (a *soft*
allowlist, bypassable by raw-socket tools) or a network-namespace + NAT setup
(heavy, needs user namespaces, still IP-only on macOS). The toggle covers the
practical need: "this task needs the network: on; otherwise off."

## 6. Availability, fallback, Windows

`--sandbox` with `Available()==false` (kernel < 5.13, Landlock off, `sandbox-exec`
missing, Windows) **fails closed**: refuse to run `terminal` with a clear message
— the user asked for a guarantee we can't provide.

Windows has no lightweight equivalent (Job Objects / AppContainer are heavyweight
and awkward): `--sandbox` errors at startup. Default-off means normal use is
unaffected.

## 7. CLI

`--sandbox` plus the policy extenders, on both `octo chat` and `octo init`:

- `--sandbox-allow-net` — permit IP networking.
- `--sandbox-write <dir>` — add a writable root (repeatable).
- `--sandbox-read <dir>` — add a readable root (repeatable).

## 8. Dependencies

No third-party sandbox library. Landlock and the seccomp filter are hand-rolled
on `golang.org/x/sys/unix` (already a dependency); macOS shells out to the
system `sandbox-exec`. The single-binary ethos holds.

## 9. Testing

OS-specific, not fully coverable in one CI runner:

- `//go:build darwin` / `linux` / `other` split implementations; a portable
  `Available()`-gated path.
- Capability-gated integration tests that run only where the mechanism works:
  "write inside cwd succeeds", "write outside cwd (`$HOME/x`) fails", "read
  `~/.ssh/known_hosts` fails", and "a TCP connect fails" under `AllowNetwork=false`.
- Windows / old-kernel: the **fail-closed** branch (`Available()==false` →
  `Command` errors / startup refuses).
- The re-exec shim is exercised by spawning the test binary's own
  `__sandboxed-exec` path.
