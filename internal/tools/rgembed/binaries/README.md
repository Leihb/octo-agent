# Embedded ripgrep binaries

This directory holds the ripgrep (rg) binary that gets embedded into the octo
binary via `go:embed`. It is populated at **build time** by the Makefile —
never checked into git.

## Build-time flow

1. `make build` detects `GOOS`/`GOARCH`.
2. Downloads the matching release asset from
   `https://github.com/BurntSushi/ripgrep/releases`.
3. Extracts the `rg` (or `rg.exe`) binary into this directory.
4. `go:embed binaries/rg` bakes it into the final binary.

## Supported platforms

| GOOS   | GOARCH | Asset suffix                        |
|--------|--------|-------------------------------------|
| darwin | amd64  | `x86_64-apple-darwin.tar.gz`        |
| darwin | arm64  | `aarch64-apple-darwin.tar.gz`       |
| linux  | amd64  | `x86_64-unknown-linux-musl.tar.gz`  |
| linux  | arm64  | `aarch64-unknown-linux-gnu.tar.gz`  |
| windows| amd64  | `x86_64-pc-windows-msvc.zip`        |

Windows uses the MSVC build (smaller, no extra runtime dependencies).

## Runtime fallback

If the user already has `rg` on PATH, the embedded copy is ignored and the
system binary is used directly.
