# C9 Phase 3 — Hindsight (and other retrieval layers) integration

> 设计依据：`c9-memory-design.md` §8。PR #118 落地了 hook 机制；本文是
> "怎么把 Hindsight（或任何检索层）接进来" 的实操手册。

C9 Phase 1+2 给了 octo **typed 写入 + 整合 + summary 注入**，但没有
**检索**。设计上检索就不进 harness 核心——它经 Phase 3 的 hook 插件机制叠加，
不装 Hindsight 仍能用 typed memory，装了多一路召回。

## 1. Hook surface 简要

PR #118 在 REPL 的每一 turn 边界各暴露一个 hook：

| Hook | 触发 | 输入 (stdin) | 输出 (stdout) | 作用 |
|---|---|---|---|---|
| `OCTO_HOOK_PRE_TURN` | 用户每次输入 → RunStream 之前 | `{"user_input": "..."}` | 文本 *或* `{"additional_context": "..."}` JSON | stdout 内容拼到 user message 后面（带分隔符），模型当上下文使用 |
| `OCTO_HOOK_POST_TURN` | RunStream 成功返回后 | `{"user_input": "...", "assistant_reply": "..."}` | 忽略 | 把刚结束的 turn 喂给检索层做 retain |

约束：

- 串行调用，跑在 turn 的临界路径上。默认 5s timeout（`OCTO_HOOK_TIMEOUT`
  可调，上限 30s）。
- Pre 失败不阻断 turn，只在 stderr 打 `↳ pre-turn hook: <err>` 一行。
- Post 只在 turn 成功后跑（err/中断时跳过，避免污染 retention index）。
- 用 `sh -c <cmd>` 执行，所以脚本路径可以带 pipe / 重定向。Windows 需要
  PATH 里有 sh-compatible runner（Git Bash / WSL）—— 同 `terminal` 工具的
  约束。

## 2. Hindsight 简介

[Hindsight](https://github.com/anthropics/hindsight) 是 Anthropic 出的本地
记忆增强层。它在 Claude Code 里靠两个 hook 接入：

- **UserPromptSubmit hook**：prompt 前先经 Hindsight 召回；结果作
  `additionalContext` 注入。
- **post-response hook**：自动 retain 这次对话。

Hindsight 自己跑一个本地 process（默认 `127.0.0.1:7777`）+ SQLite/embedding
存储。装上后 CLI 暴露 `hindsight recall` 和 `hindsight retain` 两个子命令——
我们用的就是这两个。

## 3. 接入 octo 的脚本（推荐写法）

把下面两个脚本放到 `~/.octo/hooks/`（路径任意），然后用环境变量指给 octo：

### `~/.octo/hooks/pre-turn.sh`

```sh
#!/bin/sh
# Pre-turn: 读 stdin 拿到 user_input，调 hindsight recall，把结果
# wrap 成 additional_context JSON 返还给 octo。
set -e

input_json=$(cat)
user_input=$(echo "$input_json" | jq -r '.user_input')

# Hindsight recall: 返回相关的过往对话片段。--limit 控制召回多少条；
# --format text 让它直接出可读文本。--quiet 抑制进度日志。
recall=$(hindsight recall --limit 5 --format text --quiet -- "$user_input" || true)

if [ -z "$recall" ]; then
    # 没有相关内容；不返还 additional_context 即可（空 stdout）。
    exit 0
fi

# 用结构化 JSON 返还，让 octo 走 JSON path（更稳）。
jq -n --arg ctx "$recall" '{additional_context: $ctx}'
```

### `~/.octo/hooks/post-turn.sh`

```sh
#!/bin/sh
# Post-turn: 把刚结束的 user+assistant 对喂给 hindsight 做 retain。
# Fire-and-forget——失败也只是这次 turn 没存进去，下次还会有机会。
set -e

input_json=$(cat)
user_input=$(echo "$input_json" | jq -r '.user_input')
assistant_reply=$(echo "$input_json" | jq -r '.assistant_reply')

hindsight retain \
    --user-message "$user_input" \
    --assistant-message "$assistant_reply" \
    --source octo \
    --quiet
```

记得 `chmod +x ~/.octo/hooks/*.sh`。

### 接线（shell profile）

```sh
# ~/.zshrc or ~/.bashrc
export OCTO_HOOK_PRE_TURN=~/.octo/hooks/pre-turn.sh
export OCTO_HOOK_POST_TURN=~/.octo/hooks/post-turn.sh
# Hindsight recall 通常 <1s；2s 给本地慢盘留余地。
export OCTO_HOOK_TIMEOUT=2s
```

`octo chat` 启动时会 `LoadFromEnv()` 一次，之后每个 turn 自动触发。

## 4. 数据流

```
                       ┌─────────────────────────────────────┐
        user types →   │ REPL turn loop                       │
                       │                                      │
                       │  1. pre-turn.sh                      │
                       │     stdin:  {"user_input": "..."}    │
                       │     ↓                                │
   hindsight recall ←──┤     reads → calls Hindsight          │
                       │     ↓                                │
                       │     stdout: {"additional_context":...│
                       │                                      │
                       │  2. InjectContext()                  │
                       │     <user input>                     │
                       │     ---                              │
                       │     Additional context (from hook):  │
                       │     <recall result>                  │
                       │                                      │
                       │  3. RunStream(turnInput, …)          │
                       │     ↓                                │
                       │     model reply                      │
                       │     ↓                                │
                       │  4. post-turn.sh                     │
                       │     stdin: {user_input, reply}       │
                       │     ↓                                │
   hindsight retain  ←─┤     reads → calls Hindsight          │
                       │                                      │
                       └─────────────────────────────────────┘
```

C9 Phase 1+2 的 `memory_summary.md` injection 仍然走 `prompt.Compose`
（system prompt 冻结 prefix）；hook 注入是 **per-turn 用户消息附加**，
两者正交。Hindsight 召回的内容只影响当前 turn，不进 system prompt
缓存——所以 cache key 不动，prompt 缓存仍有效。

## 5. 调试

- **看 hook 是否真的跑了**：临时把脚本第一行加 `set -x` 并改 `OCTO_HOOK_PRE_TURN`
  指向一个 wrapper script，wrapper 里 `tee /tmp/hook-stdin > /dev/null` 抓输入。
- **hook 错误**：octo 在 stderr 打 `↳ pre-turn hook: <err>`（含 stderr tail，
  截断到 200 字符）。timeout 会显示成 `timed out after 2s`。
- **hook 把 turn 拖慢了**：调小 `OCTO_HOOK_TIMEOUT`（最低 1s）；或在
  pre 脚本里加 fail-fast 短路。
- **Hindsight 不在线**：脚本里 `|| true` 让 recall 失败时返空 stdout，
  octo 当无召回处理，turn 正常推进。

## 6. 不止 Hindsight：其它检索层

Hook 是协议无关的——任何能 shell 调的检索层都能接：

| 检索源 | pre-turn 脚本 |
|---|---|
| 本地 SQLite FTS | `sqlite3 ~/.octo/notes.db "SELECT body FROM notes WHERE notes MATCH '$(cat | jq -r .user_input)'"` |
| Qdrant / Chroma | `curl -sX POST .../search -d "{\"query\": \"$(cat | jq -r .user_input)\"}" | jq -r '.results'` |
| Obsidian Smart Connections | `obsidian-smart-connections-cli search "$(cat | jq -r .user_input)"` |
| 公司内 RAG endpoint | `curl -s "$RAG_ENDPOINT?q=$(cat | jq -r .user_input)"` |

约束就两条：1) 在 timeout 内返回；2) stdout 是给模型看的纯文本或
`{additional_context: ...}` JSON。

## 7. MCP client 路径（未排期）

c9-memory-design.md §8 留了另一条路：让 octo 实现 MCP client，直接调
Hindsight 的 `agent_knowledge_*` tools。优点是协议化、能复用其他 MCP
server；代价是 MCP client 本身是更大的独立工作。

当前 hook 路径已经够用——如果未来有强需求（比如同时接多个 MCP server），
再做 client。这俩不冲突：可以并存，配置文件里指定每个 turn 走哪条路。

## 8. 不做（boundary）

- **Hindsight 不进 typed memory 的写入路径**。`remember` 工具、boundary
  extraction、`memoryd` 处理的是 octo 自己的 typed memory（feedback /
  user / project / reference 四类，进 system prompt 注入）。Hindsight
  处理的是**全文检索 / 语义召回**的 turn-level context。两者目的不同，
  存储不同，调用时机不同。
- **Hindsight 失败不影响 typed memory**。即使 hook 超时 / 失败，
  `remember` 还在工作，下次 session 启动还会 inject summary。
- **不替换 Phase 1/2**。Phase 3 是叠加层。`OCTO_HOOK_PRE_TURN` 不设
  → 走纯原生路径。

## 9. Roadmap

- ✅ Hook 机制（PR #118）
- ✅ 本文档（PR #119）
- ⏳ 真实 Hindsight 实例跑通端到端（依赖用户本地装 Hindsight）
- ⏳ 可选：layered config (`~/.octo/hooks.yml`) 取代纯 env vars。
- ⏳ 可选：MCP client（另开 milestone）
