# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased — 0.1.0-dev]

### Added
- **Go module scaffold** at `github.com/Leihb/octo-agent` (`cmd/octo` entry, `internal/{agent,provider,tools,version}`, Makefile, Go 1.22 CI matrix on Linux / macOS / Windows).
- **`octo chat` CLI** — single-turn and interactive REPL modes. Streams by default; `--stream=false` opts back into buffered output.
- **Anthropic Messages provider** — `x-api-key` auth, `anthropic-version` header, `content[].text` block parsing. `ANTHROPIC_BASE_URL` env override targets compatible third parties (DeepSeek, Kimi, OpenRouter Anthropic shim, etc.).
- **OpenAI Chat Completions provider** — Bearer auth, `system` carried as `messages[0]`, `choices[0].message.content` parsing. `OPENAI_BASE_URL` env override targets compatible third parties (DeepSeek, Bailian/Qwen, vLLM, etc.).
- **Streaming SSE** — native aggregators for both protocols (`content_block_delta`/`message_delta` for Anthropic, `chat.completion.chunk` for OpenAI with `[DONE]` sentinel tolerated). `Provider.SendStream` + agent-level `StreamingSender` / `Agent.TurnStream`.
- **Tool calling (agentic loop)** — `Agent.Run` / `Agent.RunStream`, normalized `ContentBlock` (`text` / `tool_use` / `tool_result`), provider-side tool-call decoding (OpenAI `finish_reason:"tool_calls"` normalised to `"tool_use"`).
- **`terminal` tool** — first concrete tool; runs `sh -c <command>` with a 30s timeout, returns combined stdout+stderr, surfaces non-zero exits as `[exit: ...]` annotations rather than Go errors so the LLM can read and adapt.
- **Session persistence** — JSON sessions under `~/.octo/sessions/<YYYYMMDD-HHMMSS>.json`, resume via `octo chat -c <id>`, list via `--list-sessions`, opt out with `--no-save`.
- **REPL slash commands** — `/help`, `/cost` (token + USD estimate, per-model pricing), `/save`, `/sessions`, `/exit`, `/quit`.
- **AgentEvent structured event stream** — `Agent.RunStream` takes an `EventHandler` that receives typed events (`text_delta`, `tool_started`, `tool_progress`, `tool_done`, `tool_error`, `turn_done`). Tool events carry `ToolID` + `ToolName` + `Input`; `Output` is truncated to 512 bytes for UI/IM previews while the agent's conversation history keeps the full result. `tool_progress` fires zero-or-more times between started and done, carrying incremental output (`Chunk` field) from tools that implement `StreamingToolExecutor`.
- **StreamingToolExecutor interface** — optional extension to `ToolExecutor` letting tools emit `tool_progress` events as their output arrives. `dispatchTools` type-asserts and routes to `ExecuteStream` when the executor supports it AND the agent has a handler; otherwise falls back to non-streaming `Execute`.
- **`terminal` tool streams stdout+stderr line-by-line** — merges both streams through an `io.Pipe`, forwards each line to the progress callback as it arrives, and returns the full aggregated output. Non-zero exits still surface as `[exit: ...]` annotations in the result, not Go errors.
- **Core tool suite** — seven new built-in tools available under `octo chat --tools`:
  - `read_file` — cat-style line-numbered read with offset/limit, capped at 2000 lines per call. Refuses binary extensions (`.exe` / `.zip` / `.png` / `.pdf` / `.sqlite` / …) and blocking device files (`/dev/random`, `/dev/tty`, etc) by extension before opening, so the LLM doesn't blow context on a multi-MB binary or hang reading `/dev/urandom`. `/dev/null` is permitted (returns EOF).
  - `write_file` — write/overwrite with `mkdir -p` semantics on the parent path.
  - `edit_file` — exact substring replacement; requires a unique match unless `replace_all=true`. Windows files with CRLF line endings are editable: matching is done in normalized LF space (an `old_string` copied from `read_file` output works), and the result is written back with CRLF preserved if the original used it.
  - `glob` — file pattern matching with `**` support, sorted by mtime descending, skips `.git`/`node_modules`/`vendor`/`.venv`, capped at 200 results.
  - `grep` — `ripgrep` wrapper with `content` / `files_with_matches` / `count` modes, supports `-A`/`-B`/`-C` context and include-glob filtering. Sets `--max-columns 500` so a hit on a minified JS bundle or base64 blob doesn't flood the LLM's context with one line.
  - `web_fetch` — URL → Markdown via the Jina Reader proxy, capped at ~200 KB with a clear truncation marker.
  - `web_search` — five-backend chain with priority `BRAVE > TAVILY > SERPER > DuckDuckGo HTML > Bing HTML`. Default is **zero-key**; paid backends opt in via env vars. The response `Provider` field tells the LLM which backend produced the results so it can reason about result quality. Inherits the hard-won HTML-scraping rules (no `Accept-Encoding` on Bing, browser-shaped header set, Bing `ck/a?u=a1<base64>` link decoding, DDG 10-minute cooldown on failure).
- **Tool registry** — new `internal/tools/registry.go` houses `DefaultRegistry` and `DefaultTools()` as a slice-driven dispatcher. Adding a new built-in tool means one entry in `allTools`.
