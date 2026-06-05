# 统一会话引导(Unified Session Bootstrap)

> CLI、HTTP server、IM bridge 走同一套装配流程,只在传输层、交互性、会话生命周期三处因入口而异。

---

## 背景:三套并行的装配流程

今天有三个入口,每个都自己把"一个能跑工具的 agent"从头装一遍,结果能力参差、还各写了一份基础设施:

| 能力 | CLI (`cmd/octo` chat) | IM bridge (`cmd/octo` channel) | HTTP server (`internal/server`) |
|---|---|---|---|
| config.Load + provider 构造 | ✓ | ✓(共用 cmd/octo 的 buildProvider) | ✓ **自己又实现了一份** buildProvider/resolveBaseURL |
| MCP 连接 | ✓ | ✗ | ✗ |
| Tool Search | ✓ | ✗ | ✗ |
| 子 agent / spawner | ✓ | ✗ | ✗ |
| 任务 task store | ✓ | ✗ | ✗ |
| asker(向用户提问) | ✓ | ✗ | ✗ |
| 权限 gate | `cliPermissionGate`(ask→弹窗) | **完全没有 gate(裸跑工具)** | `serverPermissionGate`(ask→deny) |
| 工具列表 | `DefaultToolsFor(model)` | `DefaultTools()` | `DefaultTools()` |

两个具体的坏味道:

1. **IM bridge 没有任何权限 gate** —— 工具调用不经任何检查直接执行。
2. **server 重新实现了 provider 构造**,并因此 `import internal/provider`,直接违反了 `.octorules` 里"`cmd/octo` 是唯一允许直接 import `provider` 的包"这条单向依赖规则。

换句话说,缺的不只是功能,连依赖方向的约束都被这次复制破坏了。

## 目标

一个共享的 **bootstrap**:输入是配置和三个"因入口而异"的旋钮,输出是装配好的 agent + 工具执行器 + 工具列表函数 + 清理钩子。三个入口各自只负责"解析自己的传输 + 调 bootstrap + 跑自己的 loop"。

只有三处差异是入口固有的:

1. **传输层** —— 输入怎么来、输出/事件怎么渲染:stdin+TUI / HTTP+SSE / IM adapter。
2. **交互性** —— CLI 能向用户弹窗(`ask` → 提示;asker 可用);server/IM 非交互(`ask` → deny;无 asker)。一个 `Interactive bool` 旋钮即可表达。
3. **会话生命周期** —— CLI 持久化、可 resume;server 按连接;IM 按 chat。会话存储是入口各自的事,但 agent 的构造是共享的。

其余(provider/model 解析、skills、MCP、Tool Search、子 agent、tasks、system prompt、executor)全部下沉到 bootstrap。

## 架构

### 新包 `internal/app`

`internal/app` 成为**唯一**构造 provider 与装配 agent 的地方。`cmd/octo` 和 `internal/server` 都通过它装配,谁都不再直接 `import internal/provider`。`providerSender`(provider→agent.Sender 适配器)和 provider/baseURL 构造逻辑一并迁入。

```go
package app

// Options is everything an entry point supplies to stand up a session.
type Options struct {
	Config         config.Config       // already loaded
	Provider       string              // resolved vendor
	Model          string              // resolved model
	System         string              // user --system override (may be "")
	CWD            string

	// Interactive selects the permission interaction model: true → ask resolves
	// via Asker (a prompt); false → ask resolves to deny and Asker stays nil.
	Interactive    bool
	Asker          tools.Asker         // non-nil only when Interactive; transport-specific
	PermissionMode permission.Mode

	EnableTools    bool                // master switch (today's --tools / cfg.Tools)
	EnableMCP      bool                // connect MCP servers (background for TUI, eager otherwise)
}

// Session is the fully-wired result. Transports run their own loop against it.
type Session struct {
	Agent    *agent.Agent
	Executor agent.ToolExecutor
	// ToolsFor returns the model-aware tool list; re-call after MCP connects so
	// the list picks up the new surface (or its Tool Search bridge).
	ToolsFor func() []agent.ToolDefinition
	MCPBoot  *MCPBootstrap // non-nil when MCP connects in the background (TUI)
	Cleanup  func()        // KillAllBackground + CleanSpillFiles + SetMCPRegistry(nil) + subagent KillAll
}

func Bootstrap(ctx context.Context, opts Options) (*Session, error)
```

`Bootstrap` does, in one place, what the three entries do today piecemeal: build the provider+sender, `agent.New`, compose the system prompt, `SetSkills`, `SetToolSearchConfig`, wire the spawner + `SubAgentManager`, `SetTaskStore`, optionally connect MCP and `SetMCPRegistry`, build the executor, and install the permission gate.

### 统一权限 gate

今天三套 gate(弹窗 / deny / 无)收敛成一个,由 `Interactive` + `Asker` 参数化:

```go
type permissionGate struct {
	engine *permission.Engine
	asker  tools.Asker // nil → non-interactive: ask resolves to deny
}
```

- `asker != nil`(CLI):`ask` → 通过 asker 弹窗,支持"记住本会话"。
- `asker == nil`(server/IM):`ask` → deny(与 strict 模式一致)。
- gate 始终对 **Tool Search `tool_call` 解包后的真实工具名**鉴权(已有 `tools.ToolCallTarget`)。

迁移后 **IM bridge 第一次拥有权限 gate**(非交互、ask→deny),消除"裸跑工具"。

### 依赖方向(需同步修订 `.octorules`)

```
cmd/octo ─┐
          ├─► internal/app ─► internal/provider/*   (唯一的 provider 构造点)
internal/server ─┘            internal/agent, internal/tools, internal/mcp
```

`internal/server` 迁移后**删除**它对 `internal/provider` 的 import 和那份重复的 `buildProvider`/`resolveBaseURL`。`.octorules` 把"`cmd/octo` 是唯一直接 import provider 的包"改为"`internal/app` 是唯一构造 provider 的包;`cmd/octo` 与 `internal/server` 通过它装配"。净效果是 provider 的 import 点**减少**、且依赖方向重新单向化。

## 能力对齐(本次决定:完全对齐)

server 与 IM 经 `internal/app` 装配后,自动获得与 CLI 相同的 MCP、Tool Search、子 agent、tasks。唯一按入口而异的是交互性:

- CLI:`Interactive=true`,asker 走 TUI/stdin。
- server / IM:`Interactive=false`,asker 为 nil,`ask` → deny。

子 agent / tasks 这类"长会话"能力对 server/IM 同样开启(决定如此);若某入口的会话过短不适用,由该入口在自己的 loop 里决定是否使用,而非在装配层裁剪。

## 分阶段迁移

依赖:本重构消费 Tool Search PR(`DefaultToolsFor` + `tools.tool_search` 配置)。先让该 PR 落地,再起 PR1,避免 rebase 冲突。

- **PR1 —— 抽 `internal/app.Bootstrap` + 迁移 CLI。** 把 chat.go 的装配逻辑、`providerSender`、provider 构造迁入 `internal/app`;统一 gate 迁入。CLI 改为调用 `Bootstrap`。**行为零变化**,靠现有 cmd/octo 测试守护。
- **PR2 —— 迁移 server。** `internal/server` 改调 `app.Bootstrap(Interactive=false)`,删除自有 provider 构造与 `serverPermissionGate`。server 由此获得 MCP / Tool Search / 子 agent / tasks。
- **PR3 —— 迁移 IM bridge。** channel 路径改调 `app.Bootstrap(Interactive=false)`,获得同一套能力**和首次拥有的权限 gate**。

每个 PR 独立可 review、可回滚。

## 兼容性

- **CLI 行为不变**:PR1 是纯重构,TUI/headless 的可见行为不动。
- **session 持久化/resume** 仍是各入口自己的事(bootstrap 只产出 agent,不管存储)。
- **MCP 后台连接**(TUI 的 `mcpBoot` 异步 tea.Cmd)通过 `Session.MCPBoot` 暴露,非 TUI 入口可选择 eager 连接。

## 非目标

- 不统一会话存储 / resume 语义(各入口生命周期不同,属合理差异)。
- 不改 Tool Search 本身的机制(本重构只是让三入口都能用上)。
- 不改传输层(TUI 渲染、SSE 帧、IM adapter 各自保留)。
- 不在装配层按入口裁剪能力(对齐优先;能力是否使用由入口的 loop 决定)。

## 测试

- **PR1**:cmd/octo 现有测试全绿即证明行为不变;为 `app.Bootstrap` 加单测(给定 Options 产出预期 gate 类型 / 工具列表 / 清理钩子调用)。
- 统一 gate 的 `ask` 解析:`Interactive=true`+asker→prompt;`Interactive=false`→deny;`mcp_call` 解包后按真实名鉴权(从现有 gate 测试迁移)。
- **PR2/PR3**:server / channel 现有测试守护;新增"非交互入口的 ask 一律 deny"与"IM 现在会拦截危险工具"的断言。
- 遵循"go test 无 live network":provider 用 httptest,MCP 用假 server。
