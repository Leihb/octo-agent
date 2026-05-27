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

> **Pre-1.0.** CLI is functional today; Web UI and IM bridges land in later milestones — see [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md).

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

# Enable the built-in tools (LLM can run shell commands, read/edit files, search, …)
octo chat --tools

# Sandbox those commands: confine the terminal tool to the project dir + tmp, no network
octo chat --tools --sandbox

# Generate a .octorules guide for this repo
octo init

# List discovered skills
octo chat --list-skills
```

## Configuration

Octo composes its system prompt from several optional layers (later overrides earlier):

- `~/.octo/octorules.md` — your global, cross-project rules and preferences.
- `.octorules` — per-repo conventions, committed with the project. Generate one with `octo init` (or `/init` in the REPL).
- `--system "..."` — a one-off override for a single run.

Both rule files support `@include path/to/fragment.md` to pull in shared content.

## Skills

Skills are reusable instruction sets in Claude Code's SKILL.md format, discovered from:

- `~/.octo/skills/<name>/SKILL.md` — user-level, across all projects.
- `.octo/skills/<name>/SKILL.md` — project-level (takes precedence over user-level).

The format is identical to Claude Code's, so you can symlink `~/.claude/skills` to `~/.octo/skills` and reuse what you already have. Each `SKILL.md` is YAML frontmatter plus a markdown body:

```markdown
---
name: review
description: Review the current diff for correctness and style
---
Walk the diff hunk by hunk and flag correctness bugs first, then style.
```

At session start Octo lists each skill's name and description in the system prompt; the model loads a skill's full instructions on demand (via the `skill` tool) when a task matches. You can also trigger one explicitly — `octo chat --list-skills` to see what's discovered, then `/skills` to list and `/<name>` (e.g. `/review`) to run one in the REPL.

## Sandboxing

`--sandbox` confines the `terminal` tool to the project directory plus temp, with no network, enforced by the OS (macOS Seatbelt, Linux Landlock + seccomp). It's off by default and fails closed when the OS mechanism is unavailable.

```bash
octo chat --tools --sandbox                              # confine, deny network
octo chat --tools --sandbox --sandbox-allow-net          # allow network
octo chat --tools --sandbox --sandbox-write ./build      # extra writable dir (repeatable)
octo chat --tools --sandbox --sandbox-read /opt/data     # extra readable dir (repeatable)
```

## What's implemented

| Area | Status | Description |
|------|--------|-------------|
| Core CLI | done | Single-turn + interactive REPL, streaming, session persistence (`~/.octo/sessions/`), `/cost` `/save` `/sessions` |
| Providers | done | Anthropic Messages + OpenAI Chat Completions, plus any compatible third party |
| Tools | done | `terminal` (+ background), file read/write/edit, glob, grep, web fetch/search |
| Agentic loop | done | Multi-step tool calling, permission gating, history compaction, graceful Ctrl-C |
| Memory & config | done | `~/.octo/octorules.md`, `.octorules`, `octo init`, `@include` |
| Skills | done | Claude Code-compatible SKILL.md loader (`--list-skills`, `/skills`, `/<name>`) |
| Sandbox | done | OS-enforced `--sandbox` (macOS / Linux) |
| Web UI / IM bridges | planned | See [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md) |

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
internal/tools/    ToolExecutor implementations — terminal (+ background),
                   file read/write/edit, glob, grep, web fetch/search, skill
internal/skills/   SKILL.md discovery + system-prompt manifest
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
