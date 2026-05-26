# octo-agent

[![Go CI](https://img.shields.io/github/actions/workflow/status/Leihb/octo-agent/go.yml?label=ci&style=flat-square)](https://github.com/Leihb/octo-agent/actions)
[![Website](https://img.shields.io/badge/website-octo--agent.dev-4f46e5?style=flat-square)](https://octo-agent.dev)
[![Go](https://img.shields.io/badge/go-%3E%3D%201.22-00ADD8?style=flat-square)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-lightgrey?style=flat-square)](LICENSE.txt)

<p align="center">
  <a href="README.md">English</a> · <a href="README_CN.md">简体中文</a>
</p>

A functionality-first AI agent, distributed as a single Go binary. Speaks two native API protocols — **Anthropic Messages** and **OpenAI Chat Completions** — and works against any compatible third party (DeepSeek, Kimi, Bailian, OpenRouter, vLLM, …). Aims for three equal interfaces: **CLI**, **Web**, and **IM**.

## Status

> **Pre-1.0, active rewrite.** The Ruby implementation has been retired (preserved on the `archive/ruby` branch). This repository is now the Go rewrite, starting at `0.1.0-dev`. CLI is functional today; Web UI and IM bridges land in later milestones — see [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md).

## Install

Until tagged releases ship, build from source:

```bash
git clone https://github.com/Leihb/octo-agent.git
cd octo-agent
make build       # produces ./octo
```

Or install directly from Go:

```bash
go install github.com/Leihb/octo-agent/cmd/octo@latest
```

## Quick start

```bash
export ANTHROPIC_API_KEY=sk-ant-...      # or OPENAI_API_KEY=...

# Single-shot
octo chat "Explain ring buffers in 100 words"

# Interactive REPL (multi-turn, session auto-saved)
octo chat

# Resume a previous session
octo chat --list-sessions
octo chat -c <session-id>

# Streaming on by default; turn off with --stream=false
octo chat --stream=false "..."

# OpenAI / DeepSeek / Bailian (OpenAI-compatible)
octo chat --provider openai --model gpt-4o-mini "..."

# Anthropic-compatible third parties (DeepSeek, Kimi, etc.)
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
  octo chat --model deepseek-chat "..."

# Enable the terminal tool (LLM can run shell commands)
octo chat --tools
```

## What's implemented

| Milestone | Status | Description |
|-----------|--------|-------------|
| M1   | done | Go scaffold (cmd/octo, Makefile, CI matrix Linux/macOS/Windows) |
| M1.2 | done | Anthropic Messages provider, single-turn `octo chat` |
| M2   | done | Streaming SSE, OpenAI Chat Completions provider, `--provider` flag |
| M3   | done | Interactive REPL, session persistence (`~/.octo/sessions/`), `/cost`, `/save`, `/sessions` |
| M4   | done | Tool calling (agentic loop), `terminal` tool |
| M5–M10 | planned | See [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md) |

## Architecture

Layered, one-directional dependency graph:

```
cmd/octo/          CLI entry (chat, REPL, sessions, slash commands)
   ↓
internal/agent/    History, sessions, content blocks, Sender interface,
                   Agent.Turn / TurnStream / Run (tool-calling loop)
   ↓
internal/provider/ Provider interface + concrete implementations
                   ├─ anthropic/   x-api-key, system top-level, content[].text
                   └─ openai/      Bearer auth, system in messages[0]
   ↓
internal/tools/    ToolExecutor implementations (currently `terminal`)
```

Each provider implements both **buffered** (`Send`) and **streaming** (`SendStream`) variants. The agent layer mirrors with `Sender` / `StreamingSender` / `ToolSender` / `ToolStreamingSender` — interfaces are added incrementally so non-streaming providers still work.

## Development

```bash
make build         # ./octo
make test          # go test -race ./...
make vet           # go vet ./...
make fmt-check     # gofmt -l . must be empty
```

See [`CLAUDE.md`](CLAUDE.md) for the project guide intended for AI coding agents working in this repo, and [`CONTRIBUTING.md`](CONTRIBUTING.md) for the human PR workflow.

## License

MIT. See [`LICENSE.txt`](LICENSE.txt).

The Ruby implementation (frozen at `v0.11.2-final-ruby`, retained on the `archive/ruby` branch) was originally a hard fork of [clacky-ai/openclacky](https://github.com/clacky-ai/openclacky); the Go rewrite is a clean reimplementation.
