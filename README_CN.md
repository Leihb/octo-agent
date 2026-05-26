# octo-agent

[![Go CI](https://img.shields.io/github/actions/workflow/status/Leihb/octo-agent/go.yml?label=ci&style=flat-square)](https://github.com/Leihb/octo-agent/actions)
[![Website](https://img.shields.io/badge/website-octo--agent.dev-4f46e5?style=flat-square)](https://octo-agent.dev)
[![Go](https://img.shields.io/badge/go-%3E%3D%201.22-00ADD8?style=flat-square)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-lightgrey?style=flat-square)](LICENSE.txt)

<p align="center">
  <a href="README.md">English</a> · <a href="README_CN.md">简体中文</a>
</p>

以功能为先的 AI Agent，单一 Go 二进制分发。原生支持两种 API 协议 —— **Anthropic Messages** 和 **OpenAI Chat Completions**，并能对接任何兼容这两种协议的第三方（DeepSeek、Kimi、百炼、OpenRouter、vLLM 等）。目标是 **CLI**、**Web**、**IM** 三种平等的交互界面。

## 状态

> **Pre-1.0，处于重写期。** Ruby 实现已退役（保留在 `archive/ruby` 分支）。本仓库现在是 Go 重写版，版本号从 `0.1.0-dev` 起步。CLI 已可用，Web UI 和 IM 桥接在后续里程碑实现 —— 见 [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md)。

## 安装

正式 release 发布前先从源码构建：

```bash
git clone https://github.com/Leihb/octo-agent.git
cd octo-agent
make build       # 产物 ./octo
```

或者直接 `go install`：

```bash
go install github.com/Leihb/octo-agent/cmd/octo@latest
```

## 快速上手

```bash
export ANTHROPIC_API_KEY=sk-ant-...      # 或 OPENAI_API_KEY=...

# 单轮
octo chat "用 100 字解释一下环形缓冲区"

# 进入交互 REPL（多轮，自动保存 session）
octo chat

# 恢复历史 session
octo chat --list-sessions
octo chat -c <session-id>

# 默认流式输出；关掉用 --stream=false
octo chat --stream=false "..."

# OpenAI / DeepSeek / 百炼（OpenAI 兼容）
octo chat --provider openai --model gpt-4o-mini "..."

# Anthropic 协议兼容的第三方（DeepSeek、Kimi 等）
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
  octo chat --model deepseek-chat "..."

# 启用 terminal 工具（LLM 可执行 shell 命令）
octo chat --tools
```

## 已实现

| 里程碑 | 状态 | 内容 |
|--------|------|------|
| M1   | 完成 | Go 脚手架（cmd/octo、Makefile、Linux/macOS/Windows CI 矩阵） |
| M1.2 | 完成 | Anthropic Messages Provider，单轮 `octo chat` |
| M2   | 完成 | SSE 流式输出，OpenAI Chat Completions Provider，`--provider` 标志 |
| M3   | 完成 | 交互式 REPL，Session 持久化（`~/.octo/sessions/`），`/cost`、`/save`、`/sessions` |
| M4   | 完成 | Tool Calling（agentic loop），`terminal` 工具 |
| M5–M10 | 规划中 | 见 [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md) |

## 架构

分层、单向依赖：

```
cmd/octo/          CLI 入口（chat / REPL / sessions / 斜杠命令）
   ↓
internal/agent/    历史、Session、ContentBlock、Sender 接口、
                   Agent.Turn / TurnStream / Run（工具调用循环）
   ↓
internal/provider/ Provider 接口 + 具体实现
                   ├─ anthropic/   x-api-key，system 顶级字段，content[].text
                   └─ openai/      Bearer 认证，system 放在 messages[0]
   ↓
internal/tools/    ToolExecutor 实现（目前只有 `terminal`）
```

每个 Provider 同时实现**缓冲式** (`Send`) 和**流式** (`SendStream`) 变体。Agent 层对应有 `Sender` / `StreamingSender` / `ToolSender` / `ToolStreamingSender` —— 接口分层添加，不支持流式的 Provider 也能跑。

## 开发

```bash
make build         # ./octo
make test          # go test -race ./...
make vet           # go vet ./...
make fmt-check     # gofmt -l . 必须为空
```

[`CLAUDE.md`](CLAUDE.md) 是给 AI 编程助手在本仓库工作时看的指南；[`CONTRIBUTING.md`](CONTRIBUTING.md) 是人类 PR 流程。

## 许可

MIT。见 [`LICENSE.txt`](LICENSE.txt)。

Ruby 实现（`v0.11.2-final-ruby` 冻结，保留在 `archive/ruby` 分支）最初是从 [clacky-ai/openclacky](https://github.com/clacky-ai/openclacky) 硬分叉而来；Go 重写版是从零干净重写。
