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

> **Pre-1.0。** CLI 已可用，Web UI 和 IM 桥接在后续里程碑实现 —— 见 [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md)。

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

# 启用内置工具（LLM 可执行 shell 命令、读写改文件、搜索等）
octo chat --tools

# 沙箱化这些命令：把 terminal 工具限制在项目目录 + 临时目录，禁网络
octo chat --tools --sandbox

# 为当前仓库生成 .octorules 指南
octo init

# 列出已发现的 skill
octo chat --list-skills
```

## 配置

Octo 的系统提示由若干可选层叠加而成（后者覆盖前者）：

- `~/.octo/octorules.md` —— 你的全局、跨项目规则与偏好。
- `.octorules` —— 随项目提交的仓库级约定。用 `octo init`（或 REPL 里的 `/init`）生成。
- `--system "..."` —— 单次运行的一次性覆盖。

两个规则文件都支持 `@include path/to/fragment.md` 来引入共享内容。

## Skills

Skill 是采用 Claude Code SKILL.md 格式的可复用指令集，从以下位置发现：

- `~/.octo/skills/<name>/SKILL.md` —— 用户级，跨所有项目。
- `.octo/skills/<name>/SKILL.md` —— 项目级（优先于用户级）。

格式与 Claude Code 完全相同，所以你可以把 `~/.claude/skills` 软链到 `~/.octo/skills` 直接复用现有 skill。每个 `SKILL.md` 是 YAML frontmatter 加 markdown 正文：

```markdown
---
name: review
description: Review the current diff for correctness and style
---
逐个 hunk 审查 diff，先标正确性 bug，再看风格。
```

会话启动时 Octo 把每个 skill 的名字和描述列进系统提示；当任务匹配某个 skill 时，模型通过 `skill` 工具按需加载它的完整指令。你也可以显式触发 —— `octo chat --list-skills` 查看已发现的 skill，再在 REPL 里用 `/skills` 列出、`/<name>`（如 `/review`）运行某个。

## 沙箱

`--sandbox` 把 `terminal` 工具限制在项目目录加临时目录、禁网络，由操作系统强制执行（macOS Seatbelt、Linux Landlock + seccomp）。默认关闭；当操作系统机制不可用时 fail-closed（直接拒绝运行）。

```bash
octo chat --tools --sandbox                              # 限制，禁网络
octo chat --tools --sandbox --sandbox-allow-net          # 允许网络
octo chat --tools --sandbox --sandbox-write ./build      # 额外可写目录（可重复）
octo chat --tools --sandbox --sandbox-read /opt/data     # 额外可读目录（可重复）
```

## 已实现

| 领域 | 状态 | 内容 |
|------|------|------|
| 核心 CLI | 完成 | 单轮 + 交互式 REPL，流式输出，Session 持久化（`~/.octo/sessions/`），`/cost` `/save` `/sessions` |
| Provider | 完成 | Anthropic Messages + OpenAI Chat Completions，以及任何兼容的第三方 |
| 工具 | 完成 | `terminal`（含后台），文件读/写/改，glob，grep，web 抓取/搜索 |
| Agentic loop | 完成 | 多步工具调用，权限门控，历史压缩，优雅 Ctrl-C |
| 记忆与配置 | 完成 | `~/.octo/octorules.md`、`.octorules`、`octo init`、`@include` |
| Skills | 完成 | 兼容 Claude Code 的 SKILL.md 加载器（`--list-skills`、`/skills`、`/<name>`） |
| 沙箱 | 完成 | 操作系统强制的 `--sandbox`（macOS / Linux） |
| Web UI / IM 桥接 | 规划中 | 见 [`dev-docs/go-rewrite-roadmap.md`](dev-docs/go-rewrite-roadmap.md) |

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
internal/tools/    ToolExecutor 实现 —— terminal（含后台）、
                   文件读/写/改、glob、grep、web 抓取/搜索、skill
internal/skills/   SKILL.md 发现 + 系统提示清单
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
