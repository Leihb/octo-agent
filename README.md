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

> **Pre-1.0.** All three interfaces are live: the CLI (an interactive TUI in a terminal, a headless agentic one-shot everywhere else), a local web server (`octo serve`), and an IM bridge (`octo channel`, WeChat iLink). On top of the agent loop there are skills, MCP clients, OS-level sandboxing, persistent memory, sub-agents, and a task graph for autonomous multi-step goals.

## Install

**Prebuilt binary (no Go toolchain needed).** Grab the archive for your OS/arch
from the [latest release](https://github.com/Leihb/octo-agent/releases/latest),
unpack it, and put `octo` on your `PATH`:

```bash
# macOS (Apple Silicon) example — swap the asset name for your platform
curl -sSL https://github.com/Leihb/octo-agent/releases/latest/download/octo_<version>_darwin_arm64.tar.gz | tar xz
sudo mv octo /usr/local/bin/
octo version
```

Archives ship for linux / darwin / windows on amd64 + arm64; `checksums.txt`
in each release verifies the download.

**From Go:**

```bash
go install github.com/Leihb/octo-agent/cmd/octo@latest
```

**From source:**

```bash
git clone https://github.com/Leihb/octo-agent.git
cd octo-agent
make build       # produces ./octo
```

## Quick start

```bash
export ANTHROPIC_API_KEY=sk-ant-...      # or OPENAI_API_KEY=...

# One-time setup: save your default provider/model (skip the export above next time)
octo config

# Headless one-shot (claude -p style): one prompt → full agentic tool loop → exit.
# Built-in tools (shell, read/edit files, search), MCP servers, and skills are
# all on by default, so a single message can actually do work.
octo chat "Add a --json flag to 'octo config show' and run the tests"

# The prompt can also come from a pipe or a file — handy for scripts / CI:
echo "Summarise what changed in the last commit" | octo chat
octo chat --prompt-file ./task.md

# Interactive multi-turn: run octo in a terminal with no message to get the TUI
# (rich tool cards, session auto-saved). Resume a previous session with -c.
octo chat
octo chat --list-sessions
octo chat -c <session-id>

# Streaming on by default; --stream=false buffers and prints only the final
# reply text (clean for capturing into a file).
octo chat --stream=false "..."

# OpenAI / DeepSeek / Bailian (OpenAI-compatible)
octo chat --provider openai --model gpt-4o-mini "..."

# Anthropic-compatible third parties (DeepSeek, Kimi, etc.)
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
  octo chat --model deepseek-chat "..."

# Extended reasoning: set the intensity (Anthropic thinking / OpenAI reasoning_effort)
# and stream the dimmed thinking trace. --show-reasoning=false hides the trace.
octo chat --reasoning-effort high "..."

# Plain chat with no tools / MCP / skills
octo chat --no-tools "..."

# Sandbox the tool commands: confine the terminal tool to the project dir + tmp, no network
octo chat --sandbox "..."

# Generate a .octorules guide for this repo
octo init

# List discovered skills
octo chat --list-skills

# Web server + dashboard (binds localhost by default)
octo serve --addr 127.0.0.1:8080

# IM bridge (WeChat iLink): scan-to-login, then run the daemon
octo channel login
octo channel start

# Autonomous long-horizon goal: plan into a subtask DAG and run it to completion
octo conduct "Add a --json flag to octo config show"
octo conduct status <id>
```

## Configuration

Octo composes its system prompt from several optional layers (later overrides earlier):

- `~/.octo/soul.md` — agent identity & behavior, an openclaw/hermes-style persona.
- `~/.octo/user.md` — who you are; a profile injected into every session.
- `~/.octo/octorules.md` — your global, cross-project rules and preferences.
- `.octorules` — per-repo conventions, committed with the project. Generate one with `octo init` (or `/init` in the TUI).
- `--system "..."` — a one-off override for a single run.

The identity and rule files support `@include path/to/fragment.md` to pull in shared content.

### Reasoning

Reasoning models can deliberate before answering. Two knobs control it, both available as CLI flags and as `octo config` defaults:

- `--reasoning-effort low|medium|high` — the intensity. OpenAI-protocol backends receive it as `reasoning_effort`; Anthropic-protocol backends map it to an extended-thinking token budget. Empty (the default) means off.
- `--show-reasoning` (default on) — stream the thinking trace to the terminal, dimmed. `--show-reasoning=false` keeps reasoning enabled but hides the trace.

This unifies Anthropic `thinking` blocks and OpenAI `reasoning_content` behind one pair of controls.

### Defaults (`octo config`)

`octo config` saves your default provider, model, (optionally) base URL, and reasoning settings to `~/.octo/config.yaml`, so a bare `octo chat` works without re-typing `--provider`/`--model` every time:

```bash
octo config        # interactive wizard
octo config show   # print the effective settings + where each comes from
octo config path   # print the file location
```

Precedence is **CLI flag > env var > `~/.octo/config.yaml` > built-in default**. API keys are read from `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` first; the wizard can store one in the file (mode `0600`), but the env var is recommended.

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

At session start Octo lists each skill's name and description in the system prompt; the model loads a skill's full instructions on demand (via the `skill` tool) when a task matches. You can also trigger one explicitly — `octo chat --list-skills` to see what's discovered, then `/skills` to list and `/<name>` (e.g. `/review`) to run one in the TUI.

## Sandboxing

`--sandbox` confines the `terminal` tool to the project directory plus temp, with no network, enforced by the OS (macOS Seatbelt, Linux Landlock + seccomp). It's off by default and fails closed when the OS mechanism is unavailable.

```bash
octo chat --sandbox                              # confine, deny network
octo chat --sandbox --sandbox-allow-net          # allow network
octo chat --sandbox --sandbox-write ./build      # extra writable dir (repeatable)
octo chat --sandbox --sandbox-read /opt/data     # extra readable dir (repeatable)
```

## What's implemented

| Area | Status | Description |
|------|--------|-------------|
| Core CLI | done | Headless agentic one-shot (`claude -p` style) + interactive TUI, streaming, session persistence (`~/.octo/sessions/`), `/cost` `/save` `/sessions` |
| Providers | done | Anthropic Messages + OpenAI Chat Completions, plus any compatible third party |
| Reasoning | done | Unified extended thinking (Anthropic) / `reasoning_content` (OpenAI), `--reasoning-effort`, `--show-reasoning` |
| Tools | done | `terminal` (+ background), file read/write/edit, glob, grep, web fetch/search |
| Agentic loop | done | Multi-step tool calling, permission gating, history compaction, graceful Ctrl-C |
| Memory & config | done | `~/.octo/octorules.md`, `.octorules`, `octo init`, `@include` |
| Skills | done | Claude Code-compatible SKILL.md loader (`--list-skills`, `/skills`, `/<name>`) |
| Sandbox | done | OS-enforced `--sandbox` (macOS / Linux) |
| MCP client | done | `mcp.json` stdio + Streamable HTTP servers, tools/resources/prompts, device-flow OAuth |
| Memory | done | Persistent cross-session memory under `~/.octo/memory/`, auto extract/consolidate |
| Sub-agents | done | `launch_agent` fan-out, async + resumable (`send_message`, `agent_status`, `kill_agent`) |
| Orchestration | done | `octo conduct` — plan a goal into a subtask DAG, run it via sub-agents, resume after crash |
| Web server | done | `octo serve` — REST + SSE, embedded dashboard UI (bind localhost) |
| IM bridge | done | `octo channel` — WeChat iLink adapter (QR login, per-user sessions, slash commands) |

## Architecture

Layered, one-directional dependency graph:

```
cmd/octo/          CLI entry (chat one-shot + TUI, serve, channel, conduct, mcp, slash commands)
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
internal/permission/  allow/deny/ask rule engine gating every tool call
internal/mcp/      MCP client (stdio + HTTP, OAuth)
internal/server/   octo serve — HTTP REST + SSE + embedded dashboard
internal/channel/  IM bridge — adapter interface + WeChat iLink adapter
internal/conductor/  octo conduct — subtask DAG planner + scheduler
```

Each provider implements both **buffered** (`Send`) and **streaming** (`SendStream`) variants. The agent layer mirrors with `Sender` / `StreamingSender` / `ToolSender` / `ToolStreamingSender` — interfaces are added incrementally so non-streaming providers still work.

## Development

```bash
make build         # ./octo
make test          # go test -race ./...
make vet           # go vet ./...
make fmt-check     # gofmt -l . must be empty
```

Project conventions live in [`.octorules`](.octorules) (the human-facing rules); [`CLAUDE.md`](CLAUDE.md) expands them with the operational detail AI coding agents need in this repo. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the human PR workflow.

## License

MIT. See [`LICENSE.txt`](LICENSE.txt).
