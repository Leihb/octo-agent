# Windows onboarding design

Make octo installable and usable by a non-developer Windows user: download one
file, double-click, and end up with `octo` permanently on `PATH` — then have the
agent know which developer tools (python / node / …) are actually present so it
stops blindly invoking ones that aren't.

Two independent parts:

- **A. Toolchain detection** — the agent learns, at context-build time, which
  common runtimes exist on the machine. Cross-platform, small, in-binary.
- **B. Windows installer** — a double-click `octo-setup.exe` that installs
  per-user, writes `PATH`, and registers an uninstaller. Touches the release
  pipeline.

## Goal / acceptance

A Windows user with no dev toolchain:

1. Downloads `octo-setup.exe` from the GitHub Release.
2. Double-clicks it. (Unsigned MVP: clicks through the SmartScreen "More info →
   Run anyway" prompt — documented.)
3. The installer copies `octo.exe` to `%LOCALAPPDATA%\Programs\octo`, adds that
   directory to the user `PATH`, creates a Start-menu shortcut, and registers an
   entry in "Add or remove programs". No UAC prompt.
4. Opens a **new** terminal, types `octo`, and it runs. First run launches the
   existing config wizard.
5. When a task needs a tool that isn't installed, the agent already knows it's
   missing (from part A) and uses the existing `ShellEnvNote()` guidance to walk
   the user through a `winget` install — instead of discovering the gap by
   failing a command.

## Current state

**Distribution.** goreleaser ships a per-platform archive; Windows gets a `.zip`
containing `octo.exe`, `start-octo.cmd`, and the docs. There is no installer.
The user either double-clicks `start-octo.cmd` — which puts octo on `PATH` for
that one PowerShell session only — or manually edits `PATH`. Typing `octo` in a
fresh terminal therefore fails until the user fixes `PATH` themselves. The
README's install section is bash/curl-centric with no Windows unzip+PATH steps.

**First run.** With no API key on an interactive terminal, octo auto-launches
the config wizard (`runConfigWizard`, `cmd/octo/config.go`). `start-octo.cmd`
prints "First run? type: octo config". The web UI has its own onboarding flow
(`internal/server/onboard_config_handlers.go`).

**Install guidance.** `internal/tools/envnote.go` (`ShellEnvNote()`) already
injects solid platform guidance into the session context — PowerShell dialect,
the mid-session `PATH`-refresh trap, the UAC dialog, `winget` vs. a download
link for novices, and the `npm.ps1` execution-policy trap (use `npm.cmd`). What
it lacks is knowledge of **what is actually installed** — so the agent gets
"here's how to install on Windows" but not "python is missing".

**Self-upgrade.** `octo upgrade` swaps the binary in place, SHA-256-verified
(#633). On Windows it side-names the locked running image as
`.old.<ts>.<pid>`.

## Part A — Toolchain detection

A new probe in `internal/tools` reports, presence-only, which common tools are
on `PATH`:

```go
// DetectToolchain reports which of a curated set of developer tools resolve on
// the current PATH. Presence-only: it uses exec.LookPath (a filesystem lookup,
// no subprocess), so it is safe to call on every context build — including the
// server's per-turn recompose. Versions are deliberately not probed; the agent
// runs `<tool> --version` on demand when it actually needs one.
func DetectToolchain() (present, missing []string)
```

Curated list (the tools an agent most often reaches for, kept tight to avoid
noise): `git`, `gh`, `node`, `npm`, `python`/`python3`, `pip`/`pip3`, `uv`,
`go`, `docker`, `make`. `python`/`python3` and `pip`/`pip3` collapse to one
reported name each (present if either resolves).

The CLI (`cmd/octo/envcontext.go`) and the server context builder
(`internal/server/server.go`) both already call `ShellEnvNote()`; the detection
block is injected in the same place, e.g.:

```
- Detected tools on PATH: git, node, npm, go.
- Not found (install on demand if a task needs one): python, pip, uv, docker.
```

Paired with the existing `ShellEnvNote()` install guidance, the agent now knows
both *what is missing* and *how to install it* on this platform.

Cost: `len(list)` filesystem lookups per context build, no subprocess. Cheap
enough that the server's per-turn recompose is unaffected — the reason versions
are excluded.

## Part B — Windows installer

### Tool & install mode

[Inno Setup](https://jrsoftware.org/isinfo.php). Script lives at
`packaging/windows/octo.iss` next to the existing `start-octo.cmd`.

Per-user, no administrator:

- `PrivilegesRequired=lowest` — no UAC elevation.
- `DefaultDirName={userpf}\octo` — `{userpf}` resolves to
  `%LOCALAPPDATA%\Programs`, the per-user program location.
- `PATH` is written to `HKCU\Environment` (user scope; no admin needed), with a
  `[Code]` `NeedsAddPath` check so re-running the installer doesn't duplicate
  the entry, and a `WM_SETTINGCHANGE` broadcast so already-open shells can pick
  it up (new shells get it regardless).
- A Start-menu shortcut that opens a terminal with octo ready.
- The standard Inno uninstaller: removes files, the `PATH` entry, and the
  shortcut, and appears in "Add or remove programs".

Per-user (not `Program Files`) is the deliberate choice: it avoids UAC entirely
and keeps the install directory user-writable, so **`octo upgrade` can overwrite
the binary in place with no elevation** — the installer owns first install,
`octo upgrade` owns updates, and they don't fight over a read-only location.
Re-running a newer `octo-setup.exe` (same Inno `AppId`) also updates in place.

Payload is just `octo.exe` (ripgrep is already embedded) plus `LICENSE.txt`.

### CI integration

A `windows-installer` job is added to `.github/workflows/release.yml`:

- `needs: goreleaser`, runs on `windows-latest`.
- Downloads the just-published `octo_<version>_windows_amd64.zip` from the
  release (`gh release download`) and extracts `octo.exe` — reusing the
  goreleaser-built binary (with its embedded ripgrep) rather than rebuilding.
- Installs Inno Setup (it is **not** preinstalled on current runner images) via
  the [Inno Setup Action](https://github.com/Minionguyjpro/Inno-Setup-Action)
  or `choco install innosetup`, then compiles `packaging/windows/octo.iss` with
  the tag version passed as `/DAppVersion=<version>`.
- Uploads `octo-setup.exe` back to the same release via `gh release upload`.

MVP ships the **amd64** installer only; it runs on Windows on ARM through x64
emulation. An arm64-native installer is a later addition.

### Signing (deferred)

The MVP installer is **unsigned**. Double-clicking it raises Windows SmartScreen
("Windows protected your PC" → "More info" → "Run anyway"); the README and the
release notes document this click-through. The workflow leaves a clearly marked
signing step as a no-op hook so a later change can drop in **Azure Trusted
Signing** (~$10/mo) without restructuring the job.

### Docs

The README "Install" section gains a Windows subsection: download
`octo-setup.exe`, double-click, click through SmartScreen, open a new terminal,
run `octo`. The bash/curl path stays for macOS/Linux.

## Out of scope

- **Code signing** — deferred; hook left in the workflow.
- **winget channel** — complementary and planned next: a winget manifest points
  `winget install Leihb.octo` at this same `octo-setup.exe`. Not this round.
- **arm64-native installer** — amd64 installer covers ARM via emulation for now.
- **macOS `.pkg` / Linux `.deb`/`.rpm`** — the symmetric idea for other
  platforms; not this round.
- **Windows OS sandbox** — explicitly deferred; the permission engine remains
  the safety layer on Windows.

## Testing

- **Toolchain detection** — unit test with a temp directory placed on `PATH`
  holding fake executables, asserting the present/missing split; assert the env
  context string contains the detection block.
- **Installer** — `iscc packaging/windows/octo.iss` compiles in CI (build-time
  validation). Manual acceptance on a Windows VM: install → `octo` resolves in a
  fresh shell → `octo upgrade` still works against the per-user dir → uninstall
  removes the binary, the `PATH` entry, and the Add/Remove-Programs entry.
