# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This project is a hard fork of [clacky-ai/openclacky](https://github.com/clacky-ai/openclacky) at upstream **1.1.6**, renumbered as **0.10.0** in this fork's own SemVer line. Only changes made in this fork (0.11.x and later) are tracked here. For history prior to the fork, see the upstream repository.

## [Unreleased — 0.12.0-dev] — Go rewrite in progress

Octo is being rewritten in Go. The Ruby line ended at `v0.11.2-final-ruby` (preserved on the `archive/ruby` branch); from here, `main` mixes the frozen Ruby tree with the in-progress Go tree until Go reaches feature parity.

### Added
- **Go module scaffolding** — `go.mod` at module path `github.com/Leihb/octo`, `cmd/octo/main.go` entrypoint wiring up `version`/`help`, `internal/version` with `-ldflags`-overridable `Version` and `Commit` variables (PR #28).
- **Go CI matrix** — `.github/workflows/go.yml` runs `go vet`, `gofmt -l`, `go build`, `go test -race` against Go 1.22 and 1.23 on Linux, macOS, and Windows — including the first proof that the Go tree builds and tests on native Windows.
- **Makefile** — `make build / test / cover / vet / fmt / fmt-check / tidy / install / clean` targets. `VERSION` and `COMMIT` are injected at build time via `-ldflags`; release builds set `VERSION` explicitly (e.g. `VERSION=0.12.0 make build`).
- **`octo chat` subcommand** — single-turn Anthropic Messages call via `internal/agent` + `internal/provider/anthropic`. Reads `ANTHROPIC_API_KEY` from env. Flags: `--model`, `--system`, `--max-tokens` (PR #30).
- **`ANTHROPIC_BASE_URL` env var** — same name the official Anthropic SDK uses. Lets `octo chat` target any Anthropic-protocol-compatible third party (DeepSeek `https://api.deepseek.com/anthropic`, Kimi K2 `https://api.moonshot.cn/anthropic`, OpenRouter Anthropic-shim, etc.) without code changes.

### Changed
- **Ruby implementation frozen** at `v0.11.2-final-ruby` (PR #27). README / README_CN now carry a 🚧 callout above the fold. `.octorules` instructs future contributors to keep new Ruby features off `main`.
- **Tag `v0.11.2-final-ruby`** added on top of `v0.11.2`, and **branch `archive/ruby`** created pointing at the same commit — the canonical access points for the Ruby line going forward.

### Removed
- Nothing yet. The Ruby tree under `lib/`, `scripts/`, `spec/` stays on `main` until the Go rewrite reaches parity. A future PR will excise it in one step alongside the Ruby CI workflow.

## [0.11.2] - 2026-05-25

### Fixed
- Web UI: Skills panel (`/#skills`) could not scroll when the list of skill cards exceeded viewport height. Changed `#skills-body` from `overflow: hidden` to `overflow-y: auto` to match the Channels and Trash panels.
- Documentation: corrected Web UI port references from `7070` to `8888` in `what-is-octo.md` and `faq.md`.
- Documentation: fixed gem install commands from `gem install octo` to `gem install octo-agent` in `installation.md` and `windows-installation.md`.
- Documentation: updated CLI reference to include `--no-caching` and `--no-skill-evolution` flags.
- Documentation: removed stale `deploy` skill reference from `writing-tips.md`.
- `product-help` skill: fixed a Ruby rescue bug where `Gem::MissingSpecError` (inherits from `ScriptError`, not `StandardError`) caused the `rescue` modifier to fail when running from source. Switched to `begin/rescue Exception` with `Dir.pwd` fallback.

## [0.11.1] - 2026-05-25

### Added
- Web UI: show a unified diff for the `edit` tool inline in its tool card
- Agent: take a checkpoint snapshot right after `think()` so the Time Machine has a clean pre-tool-use anchor

### Fixed
- Web UI: a `diff` event emitted during `show_tool_preview` (before the matching `tool_call`) used to overwrite the previous tool card's stdout — typically a `read` card — making `Read(...)` look like it rendered a diff. The diff is now buffered until its owning `tool_call` creates the correct card.
- Web UI: tool card rendering, terminal error display, and todo progress consistency
- Web UI: keep tool groups expanded by default
- Terminal tool: pass `handle_id` into the `run_sync` polling loop so the right task is observed
- Terminal tool: drop the circular `max_duration` default that broke `bundle exec` startup on Ruby 3.3
- Providers: remove fake `octo` / `octoai-sea` provider stubs and update test fixtures accordingly

### Changed
- Renamed the gem from `octo` to `octo-agent` (the `octo` name is already taken on RubyGems by an unrelated project). Repository, author, and email metadata updated to the Leihb fork.
- Attributed the upstream `clacky-ai/openclacky` project in `LICENSE.txt` (stacked copyright) and in both READMEs (fork notice under the language switcher).
- Documentation: README, `.octorules`, and gemspec description aligned around the "three interfaces (CLI / Web / IM), three native protocols (Anthropic Messages / OpenAI / AWS Bedrock)" framing.

### Removed
- Stopped tracking accidentally-committed gem-unpack artifacts (`data.tar.gz`, `metadata.gz`, `checksums.yaml.gz`); added anchored `.gitignore` entries so `gem unpack` at the repo root will not re-pollute the tree.

## [0.11.0] - 2026-05-25

First release of this fork. The hard fork happened because of philosophical disagreements with upstream over how the agent should treat human-in-the-loop interactions and long-running work; the three "headline" features below are the concrete expression of that disagreement and the reason this fork exists as a separate project. The rest of the release is rebranding and excising upstream subsystems that don't fit a non-commercial, single-brand tool.

### Added
- **Non-interrupting message inbox.** New user messages that arrive while the agent is mid-run land in a per-session inbox (`@inbox` in `agent.rb`) and are drained at the top of the next iteration, instead of preempting the in-flight tool call. This keeps tool execution atomic and ends the "user types a follow-up and the model loses its current goal" failure mode.
- **Next-message suggestion (ghost text).** After each agent turn, the agent emits a suggested next user prompt rendered as the textarea placeholder; pressing Tab on an empty input accepts it. Bias is toward "what would a power user type now?", not toward chatty continuation.
- **Background task notifications subsystem.** Long-running terminal tasks can be launched as background jobs; the agent is notified by the registry when they finish and resumes the conversation with the result, rather than blocking the run loop while a build/test/deploy executes.
- Command history for both CLI and Web UI
- `dedup_key` on background-terminal tasks to prevent the agent from spawning duplicates
- Customizable cancel reason for background tasks
- System-prompt guidance pushing the agent to STOP after starting an async task, and to refine async-task behavior (stop when blocked, continue when independent)
- Forceful anti-polling instruction in the terminal "still-running" status prompt
- Octo logo, channels panel styles, and other first-party visual assets to replace removed brand assets

### Fixed
- Anthropic thinking blocks now correctly extracted as `reasoning_content`
- `reasoning_content` converted to Anthropic thinking blocks when emitted through third-party endpoints, so the round-trip is lossless
- `bin/octo` entry point, banner methods, and lingering `{{BRAND_NAME}}` placeholders after rebrand
- Terminal tool: `__CLACKY_DONE__` marker renamed to `__OCTO_DONE__`
- Web UI: user image upload showing `[object Object]` and Analyzing-indicator ordering
- Web UI: shared CSS that was accidentally removed during the brand/creator cleanup restored
- CLI: logo typo corrected from `OOTO` to `OCTO`
- Server: broadcast the background-task count immediately after a terminal kill so badges stay in sync
- Test suite: resolved pre-existing RSpec failures in terminal and input-area specs

### Removed
- Upstream `openclacky` provider (replaced wholesale with the `octo` provider)
- Brand module and creator hub (this fork is not a commercial / multi-brand product)
- Billing module and the dead frontend code that fed it
- Cost-tracking pipeline (token-usage display in the UI is preserved)

## [0.10.0] - upstream baseline

Hard-fork point. This version corresponds to `clacky-ai/openclacky` at upstream **1.1.6**, renumbered to 0.10.0 to start a clean SemVer line in this fork. No changes from this project are included in 0.10.0; see the upstream repository for prior history.
