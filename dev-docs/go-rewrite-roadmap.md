# Octo Go Rewrite — Roadmap

> 本文档记录 octo Go 重写版本的架构决策和里程碑计划。
> 目标：三界面平等（CLI / Web / IM）、100% 兼容 Claude Code Skill 格式、竞品级 agent harness。

---

## 已完成里程碑

| 里程碑 | 内容 | PR |
|--------|------|-----|
| M1   | Go 脚手架（go.mod / cmd/octo / CI 矩阵 / Makefile）                 | #28 |
| M1.2 | `octo chat` 单轮 CLI，Anthropic Provider                            | #30 |
| M2   | 流式输出（`--stream`），OpenAI Provider，`--provider` / `--model`   | #32 / #34 |
| M3   | 交互式 REPL，Session 持久化（`~/.octo/sessions/`），`-c` 续话       | #36 |
| M4   | Tool Calling 核心（agentic loop + terminal tool），四接口 Sender    | #38 |
| M5   | AgentEvent 结构化事件流（替换 `onChunk`，为 Web/IM 透出工具调用）   | #43 |
| M6   | 核心工具集（read/write/edit/glob/grep/web_fetch/web_search）        | #46 |

---

## 架构决策（M1–M4 形成的约定）

### Sender 接口层级

```
Sender                    — 基础：SendMessages
  StreamingSender         — + StreamMessages(onChunk)
    ToolSender            — + SendMessagesWithTools
      ToolStreamingSender — + StreamMessagesWithTools(tools, onChunk)
```

`providerSender`（cmd/octo/chat.go）同时实现全部四个接口，通过 type-assert 按能力降级。

### ContentBlock 联合类型

`internal/agent/content.go` 中的 `ContentBlock` 是 text / tool_use / tool_result 的统一载体。
Message.Blocks 为 nil 时使用 Message.Content（字符串），向后兼容旧 Session JSON。

### Provider 归一化

- OpenAI `finish_reason: "tool_calls"` → 归一化为 `"tool_use"`
- OpenAI tool result 序列化为独立 `role:"tool"` 消息（Wire 格式差异，agent 层不感知）
- Anthropic `apiMessage.Content` 为 `json.RawMessage`，兼容 string 和 block array

---

## M5 — AgentEvent 流式事件重构

**问题**：当前 `onChunk func(string)` 只传文本 delta，工具调用过程（开始/结束/报错）对
Web UI 和 IM 完全不透明。

**目标**：把流式通道从 `func(string)` 升级为结构化事件流，不破坏现有 REPL 行为。

### 事件类型

```go
// internal/agent/event.go
type EventKind string

const (
    EventTextDelta   EventKind = "text_delta"
    EventToolStarted EventKind = "tool_started"
    EventToolDone    EventKind = "tool_done"
    EventToolError   EventKind = "tool_error"
    EventTurnDone    EventKind = "turn_done"
)

type AgentEvent struct {
    Kind    EventKind
    // text_delta
    Text    string
    // tool_*
    ToolID  string
    ToolName string
    Input   map[string]any // tool_started
    Output  string         // tool_done
    Err     string         // tool_error
    // turn_done
    Reply   *Reply
}

type EventHandler func(AgentEvent)
```

### 接口变更

```go
// RunStream 签名升级
func (a *Agent) RunStream(
    ctx context.Context,
    userInput string,
    tools []ToolDefinition,
    executor ToolExecutor,
    handler EventHandler,   // 替换 onChunk func(string)
) (Reply, error)
```

REPL 兼容层：`repl.go` 把 `EventHandler` 包装为只打印 `EventTextDelta.Text`，不影响现有体验。

### 验收

- `EventToolStarted` 在调用工具前 fire，携带工具名和 input
- `EventToolDone` 在工具返回后 fire，携带 output（截断到 512 bytes 用于展示）
- REPL 行为与 M4 完全一致（事件处理器只打文本 delta）

---

## M6 — 核心工具集

**目标**：补齐 Claude Code 级别的文件系统 + 网络工具，达到自举能力（能改自己的代码）。

### 工具清单

| 工具名 | 功能 | 关键约束 |
|--------|------|----------|
| `read_file` | 读文件内容，支持 offset/limit（行号） | 单次最多 2000 行 |
| `write_file` | 写/覆盖文件 | 先读再写，防意外覆盖 |
| `edit_file` | old_string → new_string 精确替换 | 必须唯一匹配；replace_all 模式 |
| `glob` | 文件模式匹配，按 mtime 排序 | 返回路径列表 |
| `grep` | ripgrep 封装，content/files/count 三模式 | 支持 -A/-B/-C context |
| `web_fetch` | HTTP GET 转 Markdown | Jina reader 代理（`r.jina.ai/`） |
| `web_search` | 网络搜索 | 默认零 key 直抓 HTML（DDG + Bing 兜底），可选付费 API |

### web_search 实现

**设计哲学：**
- **零启动成本是默认值** —— 开源用户拉下来即可用，不强制注册 key
- 付费 API 是 _可选升级_，env 设了就优先用，不设也能跑

**后端优先级（运行时按这个顺序尝试）：**

| 优先级 | 后端 | 触发条件 | 备注 |
|--------|------|----------|------|
| 1 | Brave Search API | `BRAVE_SEARCH_API_KEY` 已设 | 独立索引，2000 次/月免费 |
| 2 | Tavily API | `TAVILY_API_KEY` 已设 | AI agent 专用，自带 LLM 摘要 |
| 3 | Serper.dev | `SERPER_API_KEY` 已设 | Google 代理，2500 次免费试用 |
| 4 | **DuckDuckGo HTML**（默认兜底）| 无条件 | `html.duckduckgo.com/html/?q=` |
| 5 | **Bing HTML**（最后兜底）| 无条件 | `cn.bing.com/search`（国内直连） |

> Bing Search **API** 已于 2025-08 停服。Bing HTML 抓取通道仍可用，但不是同一个东西。

### HTML 抓取通道的实战细节

```go
// internal/tools/web_search.go
// 关键约定：
// 1. 5 个真实浏览器 UA 轮播（Mac/Windows/Linux × Chrome 122/124）
// 2. 完整 browser header 集：Sec-Fetch-Dest=document / Mode=navigate /
//    Site=none / Upgrade-Insecure-Requests=1 / Accept-Language=zh-CN,zh;q=0.9,en;q=0.8
// 3. *不设* Accept-Encoding —— 一旦发 gzip，Bing 会返回 ~39KB 的 JS-only
//    骨架页而不是 ~120KB 的真 HTML（实测踩坑）。
// 4. cn.bing.com 在非中国 IP 上会 302 到 www.bing.com，必须跟 redirect
//    （最多 2 跳）。
// 5. Bing 的真实链接藏在 bing.com/ck/a?...&u=a1<URL-safe base64>&ntb=1
//    解码：去掉 "a1" 前缀 → base64 URL-safe decode → 真实 URL
//    decode 失败时直接返回原 URL，不要让一条坏链接打掉整次搜索。
// 6. DuckDuckGo 单次失败后冷却 10 分钟，期间跳过它直接走 Bing。
// 7. 超时：12s 整体超时（搜索是同步阻塞，agent 等不起）。
//    分阶段 open/read 超时在 Go net/http 需要 custom Transport.DialContext，
//    工程量不值，统一用 client.Timeout。
```

### 归一化输出

```go
type Result struct {
    Title    string
    URL      string
    Snippet  string
}

type Response struct {
    Query    string
    Results  []Result
    Count    int
    Provider string  // 实际用了哪个后端，"brave" / "bing" / "ddg" / ...
    Error    string  // 失败时填，永远不 panic / 不 return Go error 出工具边界
}
```

`Provider` 字段把"实际走了哪条路"暴露给 agent，避免 LLM 拿着零结果以为是搜索结果为零（而其实是后端全挂）。

### 目录结构

```
internal/tools/
  bash.go        (M4, 已有)
  read_file.go
  write_file.go
  edit_file.go
  glob.go
  grep.go
  web_fetch.go
  web_search.go
  registry.go    (DefaultTools() 返回所有工具列表)
```

### 验收

- `octo chat --tools` 下 LLM 能读/写/编辑当前目录文件
- `web_search "Go generics tutorial"` 默认无任何 env 设置即可返回 5 条结果（走 DDG / Bing HTML）
- 设置 `BRAVE_SEARCH_API_KEY` 后再跑同一查询，`Provider` 字段切到 `"brave"`
- 所有后端全挂时返回 `Error` 字段非空、`Results` 为空，绝不 panic

---

## M6.5 — 工具安全 + 权限层（M8 / M9 的硬前置）

**背景**：M6 完成后我们对比了 Claude Code / Codex 的同类实现，发现 octo 工具层缺一整套**权限/沙箱机制**。Claude Code 每个工具 1/3 代码量在做 allow/deny/ask 规则、UNC 路径防护、secret 扫描、preapproved host 等检查；Codex 有独立的 `sandboxing` / `network_approval` 模块。我们目前**裸跑 `Execute`**。

**为什么是 M8/M9 的硬前置**：

| 里程碑 | 不加权限层的暴露面 |
|--------|-------------------|
| M8 Web Server | `octo serve` 起 HTTP server，LLM 经工具调用直接执行 `terminal` → **公网 RCE** |
| M9 WeChat Bridge | 任何加 bot 的微信用户都能让 LLM 跑任意 shell 命令 → **RCE** |

⚠️ **M6.5 落地前，M8/M9 只能跑在 localhost / 完全可信环境**。这点在 M8/M9 验收里要写死。

### 范围

1. **权限规则系统**（核心）
   - 配置文件：`~/.octo/permissions.yml`（YAML）
   - 规则维度：`tool name × input pattern × decision { allow | deny | ask }`
   - 优先级：deny > ask > allow（命中即停）
   - 示例：
     ```yaml
     terminal:
       - deny: "rm -rf"
       - deny: "git push --force"
       - ask:  "git push"
       - ask:  "sudo"
       - allow: "ls"
       - allow: "cat"
       # 未命中默认 ask
     web_fetch:
       - deny:  hostname: ["10.*", "192.168.*", "127.*", "localhost"]   # 内网
       - allow: hostname: ["github.com", "stackoverflow.com", "*.dev"]
     write_file / edit_file:
       - deny:  path: ["~/.ssh/*", "/etc/*", "**/.env*"]
       - allow: path: ["$CWD/**"]
     ```

2. **代码层 hooks**
   - 每个 `ToolExecutor.Execute` 之前插入 `CheckPermission(ctx, name, input)` 步骤
   - 返回 `{ allow, deny, ask }`；ask 在 CLI 弹交互窗口（用 AskUserQuestion 模式），HTTP/IM 模式由调用方决定怎么呈现
   - 决定后缓存到 session 级别（"this turn" / "this session" / "always"）
   - 拒绝结果以 `EventToolError` 形式回到 agent 循环，让 LLM 自己处理

3. **read-before-write 强制**
   - `write_file` / `edit_file` 检查目标路径是否在本 session 的 `readFileState` 里
   - 不在 → 返回 `"File has not been read yet. Read it first."`（Claude Code 原文）
   - 在但 mtime > readTimestamp → 返回 `"File has been modified since read..."` 强制重读
   - 防 LLM 半凭印象覆盖最新代码

4. **terminal 工具安全检查**（对标 Claude Code BashTool 的子集）
   - 危险命令警告：`rm -rf`、`git push --force`、`chmod -R 777` 等命中后必须 `ask`，不能 `allow`
   - 路径污染检测：命令中包含 `~/.ssh`、`/etc/passwd`、`/etc/shadow` 等敏感路径时 `ask`
   - Plan mode 只读：未来 plan mode 引入时，只允许只读命令（cat/ls/grep/find/git status/git log）
   - Sed-edit 拦截：检测 `sed -i` / `sed -e ... > file` 模式，让它走 edit_file 而非绕过权限

5. **其他对标遗漏**（搭车一起做，单独做不划算）
   - `read_file` UNC 路径防护（`\\` / `//` 前缀直接拒绝，避免 Windows NTLM 凭据泄漏）
   - `write_file` secret 扫描：写入内容如果像 AWS key / SSH 私钥 / `.env` 形式，要求 `ask`
   - `web_fetch` cross-host redirect 检测：3xx 跨主机时不自动跟，返回引导 LLM 重新发起请求

### 实现切片

| 子任务 | 大约工作量 | 备注 |
|--------|-----------|------|
| `internal/permission/` 包搭骨架（规则解析、匹配引擎、CheckPermission API）| 2d | 核心 |
| 每个工具加 CheckPermission hook | 1d | 8 个工具×几行 |
| `~/.octo/permissions.yml` 加载 + 默认规则模板 | 0.5d | |
| CLI ask 交互（直接复用现有 AskUserQuestion 模式）| 0.5d | |
| read-before-write 跟踪 + readFileState | 1d | |
| Terminal 危险命令检测 + secret 扫描 | 1d | 黑名单 + 简单 regex |
| 测试 + 文档 | 1d | |

**总计 ~7d** 单人；这是 M8 之前不可省略的工作。

### 验收

- `~/.octo/permissions.yml` 不存在时使用安全默认（terminal: 全部 ask、文件读写: 限 CWD、网络: 限 github/stackoverflow 等公认安全域名）
- LLM 想跑 `rm -rf` 时，CLI 弹 ask；选 deny 后 LLM 收到 `EventToolError` 并能自适应
- 写 `~/.ssh/known_hosts` 直接被拒绝（deny 规则匹配）
- 写一个文件之前没读过 → 拒绝；mtime 改了之后再写 → 拒绝
- `web_fetch https://github.com/...` 直接 allow（preapproved）；`web_fetch http://10.0.0.1/...` 直接 deny
- `octo serve` 起来后，curl 一个 prompt 触发 `terminal: "ls"`，正确返回结果且无权限弹窗（ls 在默认 allow 表里）

### 已交付的 M6.5 子集

- `read_file` 二进制扩展名 + 设备文件拒绝（PR #46 fixup）
- `edit_file` CRLF 归一化（PR #46 fixup）
- `grep --max-columns 500` 防上下文淹没（PR #46 fixup）

剩余项目仍待做。

---

## M7 — Skill 加载器（100% Claude Code 兼容）

**目标**：实现与 Claude Code `/` 斜杠命令完全兼容的 Skill 系统。
用户的 `~/.claude/skills/` 目录下的 skill 可以直接在 octo 里使用。

### Skill 格式（沿用 Claude Code 标准）

```
~/.octo/skills/<name>/
  SKILL.md       # frontmatter + 指令正文
```

SKILL.md frontmatter（YAML）：
```yaml
---
name: my-skill
description: 一句话描述，触发检测用
triggers:
  - /my-skill
  - keyword1
  - keyword2
---
# Skill 指令正文（注入 system prompt）
...
```

### 加载流程

```
用户输入 "/foo arg1 arg2"
  │
  ├─ SkillLoader.Match("/foo") → 找到 skill
  │     读取 SKILL.md → 注入 system prompt
  │     args 作为首条 user 消息
  │     启动新 RunStream（可带 M6 工具）
  │
  └─ 未命中 → 普通 REPL 消息
```

### 兼容性要求

- SKILL.md 格式与 `~/.claude/skills/*/SKILL.md` 完全相同
- 用户可把 `~/.claude/skills/` 软链到 `~/.octo/skills/` 直接复用
- 搜索顺序：`~/.octo/skills/` → `~/.octo/default_skills/`（内置）

### 内置 Skill 候选

| Skill | 功能 |
|-------|------|
| `/help` | 列出可用 skill 和工具 |
| `/cost` | 当前会话 token 和估算费用 |
| `/sessions` | 最近 10 个 session |
| `/compact` | 触发对话压缩（M7.5 再做） |

### 验收

- `~/.octo/skills/hello/SKILL.md` 定义后，REPL 输入 `/hello` 触发
- `--list-skills` 标志列出所有可用 skill
- 与 Claude Code 同格式 SKILL.md 能直接使用（不修改）

---

## M8 — Web Server + Agent Dashboard

**目标**：HTTP server 提供 REST + SSE 接口，配套 Web UI 展示 agent 执行过程。

> ⚠️ **依赖 M6.5**。M6.5（工具权限层）落地前，M8 只能监听 `127.0.0.1` 或 Unix socket，禁止暴露到公网/局域网——否则任何能命中 HTTP 端点的人都能通过 LLM 调用 `terminal` 工具拿 RCE。M8 启动时如果检测到 `--bind` 不在本机，必须报错退出。

### API 设计

```
POST /api/chat           # 创建新对话（返回 session_id）
POST /api/chat/:id/turn  # 发送消息（SSE 流式响应）
GET  /api/sessions       # 列出历史 session
GET  /api/sessions/:id   # 获取 session 详情
GET  /api/tools          # 列出可用工具
GET  /api/skills         # 列出可用 skill
```

### SSE 事件格式（对应 M5 AgentEvent）

```
data: {"kind":"text_delta","text":"Hello"}
data: {"kind":"tool_started","tool_name":"bash","input":{"command":"ls"}}
data: {"kind":"tool_done","tool_name":"bash","output":"file1\nfile2"}
data: {"kind":"turn_done","reply":{"content":"...","input_tokens":100}}
```

### Web UI 功能（Agent Dashboard）

- 对话界面：左侧 session 列表，右侧消息流
- Tool 调用可视化：工具名 + input/output 可折叠卡片
- Token/Cost 实时显示（对接 M5 EventTurnDone）
- Skill 触发记录

### 技术选型

- HTTP server：Go 标准库 `net/http`（不引入框架）
- 前端：单文件 HTML + Alpine.js + SSE EventSource（不引入构建工具）
- 打包：`go:embed` 嵌入静态文件，单 binary 发布

### 验收

- `octo serve` 启动 HTTP server（默认 :8080）
- 浏览器打开 `localhost:8080` 能对话，能看到 tool 调用过程
- Agent Dashboard 显示实时 token 消耗

---

## M9 — WeChat iLink Bridge

> ⚠️ **依赖 M6.5**。M6.5（工具权限层）落地前不要部署 M9。任何加 bot 的微信用户都能通过对话让 LLM 跑 `terminal` 工具，等价于把 shell 开放给陌生人。M9 配置必须强校验 `permissions.yml` 存在且 `terminal` 工具不在 default-allow 列表。

**技术背景**（协议于 2026-03 开放）：

| 属性 | 值 |
|------|-----|
| 官方名称 | 微信 iLink Bot（隶属于 Tencent **OpenClaw** 多渠道 agent 网关项目） |
| 官方插件 | npm `@tencent-weixin/openclaw-weixin`（Tencent 官方发布，Node 实现） |
| API host | `https://ilinkai.weixin.qq.com` |
| 接入协议 | HTTP Long Polling |
| 关键端点 | `GET /get_bot_qrcode` · `GET /get_qrcode_status` · `POST /getupdates` · `POST /sendmessage` · `POST /sendtyping` · `POST /getuploadurl` |
| 媒体 CDN | `https://novac2c.cdn.weixin.qq.com/c2c`（`/upload`、`/download`） |
| 认证 | 扫码 QR → `bot_token`，请求头 `Authorization: Bearer <token>` + `AuthorizationType: ilink_bot_token` |
| 消息类型 | 文本 / 图片 / 语音 / 文件 / 视频 |
| 群聊支持 | ⚠️ 官方文档未明确说明，待实测确认（保守按"仅 1:1 私聊"实现，后续再扩展） |

**设计决策**：依赖第三方 Go SDK [`corespeed-io/wechatbot/golang`](https://github.com/corespeed-io/wechatbot)（436⭐，MIT，多语言 SDK 互验，自带文档站 [wechatbot.dev](https://www.wechatbot.dev/zh/protocol)）。理由：
- 协议反向工程已完成且有 Node/Python/Go/Rust 四语言互证
- 我们要的能力（扫码、收发文本、媒体）SDK 都已封装好
- 不引入 Node/Python runtime，仍是纯 Go 依赖
- 备选：[`SpellingDragon/wechat-robot-go`](https://github.com/SpellingDragon/wechat-robot-go)（22⭐，更轻量）作为兜底，若 corespeed-io 出现 license 或维护问题再切换

### 架构

```
corespeed-io/wechatbot/golang
  ├─ bot.Login(ctx)               // 扫码登录
  └─ bot.OnMessage(handler)       // 注册消息回调
        ↓
Message Router (octo 自己写)
  │  映射 IncomingMessage.UserID → Session (per-user 对话历史)
  ↓
Agent.RunStream(ctx, text, tools, executor, handler)
  │  M5 EventHandler
  │  文本 delta 累积
  ↓
bot.Reply(ctx, msg, replyText)    // SDK 内部 POST /sendmessage
```

### 配置

```yaml
# ~/.octo/config.yml
wechat:
  enabled: true
  bot_token: "xxx"          # 扫码后写入
  max_session_idle: "30m"   # 空闲超时清理 session
```

### 命令

```
octo wechat login    # 展示 QR 码，扫码后保存 bot_token
octo wechat start    # 启动 long poll 守护进程
octo wechat status   # 查看连接状态
```

### 验收

- `octo wechat login` 展示 QR，扫码后控制台确认"已登录"
- 给 Bot 发文字消息，收到 AI 回复
- 每个微信用户维护独立 session（对话历史隔离）
- 发送消息 / 收到回复全程不需要在电脑前操作
- 群聊场景：先实测 SDK 是否能接收群消息事件，再决定是否开启群聊支持

---

## M10 — Sub-Agent Tool

**目标**：LLM 在对话中可以调度子代理并行执行任务，与 Claude Code `Agent` 工具对标。

### 工具定义

```go
// internal/tools/agent.go
// Tool name: "launch_agent"
// Parameters:
//   description: string   -- 短标题（日志展示用）
//   prompt:      string   -- 子代理任务描述
//   tools:       []string -- 允许的工具名列表（nil = 继承父代理）
//   model:       string   -- 模型（默认 haiku/gpt-4o-mini）
```

### 并行执行

- 父代理一次 `tool_use` 可包含多个 `launch_agent` 调用
- 每个子代理在独立 goroutine 中跑，共享同一 Provider（不开新连接）
- 子代理不能递归调用 `launch_agent`（`forbidden_tools` 自动注入）
- 所有子代理完成后，结果作为 tool_result 批量返回给父代理

### 验收

- 父代理说"并行调研 A 和 B"→ 两个子代理同时跑 → 结果合并回复
- 子代理调用 `launch_agent` 被拒绝（递归防护）
- token 计数合并到父 session（`/cost` 显示总账单）

---

## M11 — 自主目标层（autonomous task orchestration）

**目标**：跨会话、跨小时的复杂目标自主推进。`octo goal` 起一个任务图（task
graph），把目标分解成子任务，每个子任务派给独立 sub-agent（M10）跑在**独立
context** 里；主控只跟任务图状态对话，**不**把累计对话送给评估器。

### 动机（为什么不照搬 Claude Code 的 `/goal`）

Claude Code 的 `/goal` 是 "Stop hook + 评估器读整段对话判断目标是否达成"，
**架构上和"任务复杂度"成反比**——对话越长越没法用。一旦任务涉及大段 diff、长设
计讨论、跨多 PR，评估器的 prompt 上限就先爆掉，自驱循环卡死。

Codex 走的是另一条路（也是其 memories 机制里同一思路）：**任务分解 + 独立
sub-agent + 全局锁**。每个子任务独立 context，主控只看任务图和结果，跟对话累计
量解耦——这才是为"跨小时/跨天的复杂任务"设计的架构。

octo 走 Codex 路。

### 设计骨架

- **`internal/taskgraph`**：`Task{ID, Goal, Status, Subtasks, ResultRefs}`，状态机
  `pending → running → done | failed`。持久化在 `~/.octo/tasks/<id>.json`，
  支持跨进程恢复。
- **`octo goal <command>`**：`start <goal>` 创建任务；`status [<id>]` 看进度；
  `resume <id>` 接续；`cancel <id>`。
- **分解步骤**：start 时先跑一次规划 side-call（独立 system prompt，类似 C9 的
  extract 模式），LLM 输出子任务 DAG → 写入 task graph。
- **执行**：调度器跑 ready 节点（依赖满足的 pending），每个节点 = 一次 M10
  sub-agent 调用，独立 context，结果写回任务图。可并行（依赖图允许时）。
- **进度跟踪**：主控不读对话历史，只读任务图。`status` 输出节点状态 + 关键结果
  摘要。完成条件 = 所有节点 done（或显式 success 节点 done）。
- **可中断/可恢复**：每个节点结束都 fsync 任务图；进程崩了下次 `octo goal resume`
  从最新状态继续。
- **`/goal`（TUI）与 `octo goal`（CLI）的关系**：两者是同一 taskgraph 调度器的两个入口。
  `octo goal` 是命令行入口；TUI 内 `/goal` 走 plan → 确认 → 前台执行，复用同一个规划器
  （`PlanTask`）和调度器（`taskgraph.Scheduler`），并共享 REPL 已装配的 spawner。

### 依赖与时序

依赖 M10（sub-agent tool）。和 M8 Web Server 互补（Web UI 显示任务图是自然
配套），但不强耦合。M11 可在 M10 之后任何时间做。

### 验收

- `octo goal start "做完 C9 Phase 2 daemon"` → 自动分解 + 推进 + 状态持久化
- `octo goal status <id>` 准确反映 DAG 状态，**不依赖**会话对话历史
- 进程 kill 后 `octo goal resume <id>` 从中断处继续
- 跨多小时的任务在长 context 失效场景下不崩

---

## 里程碑时序

```
M5 AgentEvent     ←── 基础，影响所有后续
  │
  ├── M6 Core Tools  ←── M7/M9 依赖工具能力
  │     │
  │     ├── M6.5 工具安全/权限层  ←── M8/M9 的硬前置（公网暴露不可省）
  │     │     │
  │     │     ├── M7 Skill Loader  ←── 可并行 M8
  │     │     │
  │     │     ├── M8 Web Server    ←── 必须 M6.5 落地后才能跑非 localhost
  │     │     │
  │     │     └── M9 WeChat iLink  ←── 必须 M6.5 落地后才能上线
  │     │
  │     └── (M6.5 旁路：M7 仅做 SKILL.md 加载，与 M6.5 解耦，可并行)
  │
  ├── M10 Sub-Agent  ←── 单会话 fan-out
  │
  └── M11 Autonomous Task ←── 跨会话任务图，依赖 M10
```

---

## 差异化原则（不为追赶而丢失）

1. **单 binary，无安装依赖** — 所有功能打进一个 Go binary，不依赖外部语言 runtime
2. **三界面平等** — CLI / Web / WeChat 是同等头等公民，不是"CLI 主，其他适配"
3. **Skill-first** — 优先用 SKILL.md 扩展能力，工具数量保持最小化
4. **100% Claude Code Skill 兼容** — 用户的 `~/.claude/skills/` 可直接在 octo 使用
5. **隐私优先** — 对话历史默认只存本地（`~/.octo/sessions/`），不上传任何云端

---

## 更新规则

每完成一个里程碑：
1. 在状态表（如建立）对应行更新 ✅ + PR 号
2. 在该节末尾追加 `**完成**：日期 / PR#xx / 关键决策`
3. 有架构变更时更新本文档对应节
