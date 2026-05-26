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
- **AgentEvent structured event stream** — `Agent.RunStream` takes an `EventHandler` that receives typed events (`text_delta`, `tool_started`, `tool_done`, `tool_error`, `turn_done`). Tool events carry `ToolID` + `ToolName` + `Input`; `Output` is truncated to 512 bytes for UI/IM previews while the agent's conversation history keeps the full result. The REPL wraps the handler as a text-only printer so its behaviour is unchanged.
