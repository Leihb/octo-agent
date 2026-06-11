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

> **Pre-1.0。** 三种界面均已上线：CLI（终端里是交互式 TUI，其余场景是 headless 的 agentic 单发）、本地 Web 服务（`octo serve`）、IM 桥接（随 `octo serve` 运行；微信 iLink、飞书、钉钉、企微、Discord、Telegram）。在 agent 循环之上还有 skills、MCP 客户端、操作系统级沙箱、持久化记忆、子代理，以及用于自主多步目标的任务图。

## 安装

**预编译二进制（无需 Go 工具链）。** 从 [最新 release](https://github.com/Leihb/octo-agent/releases/latest)
下载对应 OS/架构的压缩包，解压后把 `octo` 放进 `PATH`：

```bash
# macOS (Apple Silicon) 示例 —— 按你的平台替换文件名
curl -sSL https://github.com/Leihb/octo-agent/releases/latest/download/octo_<version>_darwin_arm64.tar.gz | tar xz
sudo mv octo /usr/local/bin/
octo version
```

提供 linux / darwin / windows × amd64 + arm64 的压缩包；每个 release 附带的
`checksums.txt` 可校验下载完整性。

**用 Go 安装：**

```bash
go install github.com/Leihb/octo-agent/cmd/octo@latest
```

**从源码构建：**

```bash
git clone https://github.com/Leihb/octo-agent.git
cd octo-agent
make build       # 产物 ./octo
```

## 快速上手

```bash
export ANTHROPIC_API_KEY=sk-ant-...      # 或 OPENAI_API_KEY=...

# 一次性设置：保存默认 provider/model（下次免去上面的 export）
octo config

# Headless 单发（claude -p 风格）：一个 prompt → 完整 agentic 工具循环 → 退出。
# 内置工具（shell、读写改文件、搜索）、MCP 服务、skills 全部默认开启，
# 所以一条消息就能真正干活。
octo "给 'octo config show' 加一个 --json 标志，然后跑测试"

# prompt 也可以来自管道或文件 —— 方便脚本 / CI：
echo "总结一下最近一次提交改了什么" | octo
octo --prompt-file ./task.md

# 交互多轮：在终端里不带消息直接运行 octo 进入 TUI（富工具卡片、自动保存
# session）。用 -c 恢复历史 session。
octo
octo --list-sessions
octo -c <session-id>

# 默认流式输出；--stream=false 改为缓冲、只打印最终回复文本（便于重定向到文件捕获）。
octo --stream=false "..."

# OpenAI / DeepSeek / 百炼（OpenAI 兼容）
octo --provider openai --model gpt-4o-mini "..."

# Anthropic 协议兼容的第三方（DeepSeek、Kimi 等）——自定义 base URL
# 走 *_compatible 万能 vendor（也只有它们接受自定义端点）
ANTHROPIC_COMPATIBLE_BASE_URL=https://api.deepseek.com/anthropic \
ANTHROPIC_COMPATIBLE_API_KEY=sk-... \
  octo --provider anthropic_compatible --model deepseek-chat "..."

# 扩展推理：设置思考强度（Anthropic thinking / OpenAI reasoning_effort），
# 并以暗色流式显示思考轨迹。--show-reasoning=false 可隐藏轨迹。
octo --reasoning-effort high "..."

# 纯聊天，关闭工具 / MCP / skills
octo --no-tools "..."

# 沙箱化工具命令：把 terminal 工具限制在项目目录 + 临时目录，禁网络
octo --sandbox "..."

# 为当前仓库生成 .octorules 指南
octo init

# 列出已发现的 skill
octo --list-skills

# Web 服务 + 仪表盘（默认绑定 localhost）
octo serve --addr 127.0.0.1:8080

# IM 桥接（微信 iLink）：扫码登录；渠道随 `octo serve` 一起运行
octo serve   # WeChat login: Channels panel in the web UI (scan QR)
```

## 配置

Octo 的系统提示由若干可选层叠加而成（后者覆盖前者）：

- `~/.octo/soul.md` —— agent 的身份与行为规范（openclaw/hermes 式 persona）。
- `~/.octo/user.md` —— 你是谁；每次会话都会注入的个人画像。
- `~/.octo/octorules.md` —— 你的全局、跨项目规则与偏好。
- `.octorules` —— 随项目提交的仓库级约定。用 `octo init`（或 TUI 里的 `/init`）生成。
- `--system "..."` —— 单次运行的一次性覆盖。

身份文件与规则文件都支持 `@include path/to/fragment.md` 来引入共享内容。

### 推理

推理模型可以在回答前先思考。两个开关控制它，都同时支持 CLI flag 和 `octo config` 默认值：

- `--reasoning-effort low|medium|high` —— 思考强度。OpenAI 协议后端作为 `reasoning_effort` 发送；Anthropic 协议后端映射成扩展思考的 token budget。留空（默认）即关闭。
- `--show-reasoning`（默认开）—— 以暗色把思考轨迹流式打到终端。`--show-reasoning=false` 保留推理但隐藏轨迹。

这把 Anthropic 的 `thinking` 块和 OpenAI 的 `reasoning_content` 统一到同一对开关之下。

### 默认值（`octo config`）

`octo config` 把默认 provider、model、（可选）base URL 和推理设置存到 `~/.octo/config.yaml`，这样裸跑 `octo` 就不必每次重敲 `--provider`/`--model`：

```bash
octo config        # 交互式向导
octo config show   # 打印当前生效设置及各项来源
octo config path   # 打印配置文件路径
```

优先级：**命令行 flag > 环境变量 > `~/.octo/config.yaml` > 内置默认**。API key 优先从 `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` 读取；向导可选择把 key 存进文件（权限 `0600`），但推荐用环境变量。

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

会话启动时 Octo 把每个 skill 的名字和描述列进系统提示；当任务匹配某个 skill 时，模型通过 `skill` 工具按需加载它的完整指令。你也可以显式触发 —— `octo --list-skills` 查看已发现的 skill，再在 TUI 里用 `/skills` 列出、`/<name>`（如 `/review`）运行某个。

## 沙箱

`--sandbox` 把 `terminal` 工具限制在项目目录加临时目录、禁网络，由操作系统强制执行（macOS Seatbelt、Linux Landlock + seccomp）。默认关闭；当操作系统机制不可用时 fail-closed（直接拒绝运行）。

```bash
octo --sandbox                              # 限制，禁网络
octo --sandbox --sandbox-allow-net          # 允许网络
octo --sandbox --sandbox-write ./build      # 额外可写目录（可重复）
octo --sandbox --sandbox-read /opt/data     # 额外可读目录（可重复）
```

## 已实现

| 领域 | 状态 | 内容 |
|------|------|------|
| 核心 CLI | 完成 | headless agentic 单发（`claude -p` 风格）+ 交互式 TUI，流式输出，Session 持久化（`~/.octo/sessions/`），`/cost` `/save` `/sessions` |
| Provider | 完成 | Anthropic Messages + OpenAI Chat Completions，以及任何兼容的第三方 |
| 推理 | 完成 | 统一的扩展思考（Anthropic）/ `reasoning_content`（OpenAI），`--reasoning-effort`、`--show-reasoning` |
| 工具 | 完成 | `terminal`（含后台），文件读/写/改，glob，grep，web 抓取/搜索 |
| Agentic loop | 完成 | 多步工具调用，权限门控，历史压缩，优雅 Ctrl-C |
| 记忆与配置 | 完成 | `~/.octo/octorules.md`、`.octorules`、`octo init`、`@include` |
| Skills | 完成 | 兼容 Claude Code 的 SKILL.md 加载器（`--list-skills`、`/skills`、`/<name>`） |
| 沙箱 | 完成 | 操作系统强制的 `--sandbox`（macOS / Linux） |
| MCP 客户端 | 完成 | `mcp.json` 的 stdio + Streamable HTTP 服务，tools/resources/prompts，device-flow OAuth |
| 记忆 | 完成 | `~/.octo/memories/` 下的跨会话持久化记忆，自动抽取/整合 |
| 子代理 | 完成 | `launch_agent` 并行扇出，异步 + 可恢复（`send_message`、`agent_status`、`kill_agent`） |
| Web 服务 | 完成 | `octo serve` —— REST + SSE，内嵌仪表盘 UI（绑定 localhost） |
| IM 桥接 | 完成 | 随 `octo serve` 运行 —— 微信 iLink/飞书/钉钉/企微/Discord/Telegram 适配器（web 扫码登录、按用户隔离 session、斜杠命令） |

## 架构

分层、单向依赖：

```
cmd/octo/          CLI 入口（chat 单发 + TUI / serve / channel / mcp / 斜杠命令）
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
internal/permission/  门控每次工具调用的 allow/deny/ask 规则引擎
internal/mcp/      MCP 客户端（stdio + HTTP，OAuth）
internal/server/   octo serve —— HTTP REST + SSE + 内嵌仪表盘
internal/channel/  IM 桥接 —— 适配器接口 + 微信 iLink 适配器
```

每个 Provider 同时实现**缓冲式** (`Send`) 和**流式** (`SendStream`) 变体。Agent 层对应有 `Sender` / `StreamingSender` / `ToolSender` / `ToolStreamingSender` —— 接口分层添加，不支持流式的 Provider 也能跑。

## 开发

```bash
make build         # ./octo
make test          # go test -race ./...
make vet           # go vet ./...
make fmt-check     # gofmt -l . 必须为空
```

项目约定写在 [`.octorules`](.octorules)（面向人类的规则）；[`CLAUDE.md`](CLAUDE.md) 在此基础上补充 AI 编程助手在本仓库工作所需的操作细节。[`CONTRIBUTING.md`](CONTRIBUTING.md) 是人类 PR 流程。

## 许可

MIT。见 [`LICENSE.txt`](LICENSE.txt)。
