# Tool Search for MCP 设计

> 用三个桥工具替换全量上传的 MCP 工具 schema,按需检索 / 加载 / 调用,削减每轮请求的 token 开销。

---

## 背景与问题

每个连上的 MCP server 会把它的**每一个**工具的完整 JSON Schema 注入模型可见的 tools 列表,而这份列表每一轮请求都会重发。一两个工具多的 server(几十个工具各带参数 schema)就能占掉几万 token,且与对话内容无关。后果有两个:

1. **成本**:schema 每轮重发,长会话里累计可观;在 anthropic 协议下还会撑大需要缓存的 tools 前缀。
2. **准确率**:工具一多,模型在无关工具间"决策瘫痪",选错工具的概率上升。

octo-agent 的工具装配链路:

```
tools.DefaultTools()  →  agent.Request.Tools  →  provider 序列化  →  wire
```

`tools.DefaultTools()`(`internal/tools/registry.go`)在末尾无条件 `append(mcpToolDefs()...)`;`mcpToolDefs()`(`internal/tools/mcp.go`)为每个 live connection 的每个工具合成完整 `agent.ToolDefinition`。Tool Search 在这条链路上做延迟加载。

## 设计取舍:进程内桥,provider 无关

octo-agent 同时支持 anthropic 协议(真 Anthropic、Kimi `/coding`、DeepSeek `/anthropic`)和 openai 协议(DeepSeek 默认、DashScope、Kimi)。Anthropic 平台原生的 `defer_loading` + `tool_search_tool_regex_20251119` 只在 anthropic 协议下有效,对 openai 协议后端无能为力,而后者是日常与 MSWE 评测的主力。

因此 Tool Search 实现在 **tools 层的进程内桥**:对所有后端透明,provider 序列化零改动,桥工具数量固定(3 个)天然缓存友好。代价是检索由 octo 自己用 BM25 完成,而非模型受过专门训练的服务端检索——准确率收益需实测,**主要确定收益是 token / 成本下降与缓存友好**。

anthropic 协议下的原生 `defer_loading` 是一个独立、可叠加的后续优化,不在本设计范围。

## 三个桥工具

激活后,`DefaultTools()` 不再吐出 N 个 MCP 工具,而是吐出三个桥工具;MCP 工具退入"延迟目录",仅可经桥访问。

| 工具 | 入参 | 行为 |
|---|---|---|
| `mcp_search` | `query`, `limit?` | BM25 检索延迟目录,返回命中工具的 `name` + 一行描述(**不含 schema**) |
| `mcp_describe` | `name` | 返回该工具的完整 JSON Schema |
| `mcp_call` | `name`, `arguments` | 真正调用:内部转发到 `executeMCP(name, arguments)`。仅代理 `mcp__` 前缀的工具——内置工具(sub_agent、read_file 等)直接按名调用,不走这里 |

三个桥工具刻意用 `mcp_` 前缀:让模型一眼看出它们是 MCP 专属的发现/调用通道,不会把 `mcp_call` 误当成通用调用器、把内置工具(典型如 `sub_agent`)塞进去。

核心工具(`terminal`、`read_file`、`grep`、`glob`、`web_*`、`launch_agent`、`task_*` 等)**永不延迟**,始终直接出现在列表里。只有 MCP 工具进入延迟目录。桥工具的描述里写清三步协议(search → describe → call),引导模型正确使用。

## 延迟目录与 BM25 索引

- **数据源**:`ActiveMCPRegistry().Connections()` 中每个 `mcp.Tool` 的 `name`、`description`、参数名。
- **算法**:纯 Go 的小 BM25,无三方依赖;命中为空(zero-IDF)时退化为子串匹配,对齐 Anthropic 行为。
- **生命周期**:每轮无状态重建。工具数量在几十量级,重建成本可忽略,且避免 registry 变化导致的目录漂移。
- **检索粒度**:`tool_search` 默认返回 `search_default_limit`(5)条,上限 `max_search_limit`(20)。

## 派发

`DefaultRegistry.Execute`(`internal/tools/registry.go`)在 MCP 路由之前先认桥工具:

```go
switch name {
case "mcp_search":   return r.toolSearch(input)
case "mcp_describe": return r.toolDescribe(input)
case "mcp_call":     return executeMCPBridge(ctx, input) // {name, arguments} → executeMCP
}
```

`mcp_call` 落到既有的 `executeMCP`,后者按 `mcp__` 前缀工作,无需改动——延迟目录里的工具名本身就是 `mcp__<server>__<tool>`。

### 权限粒度

权限 gate(`agent.PermissionGate`)在 agent loop 中对每个 tool_use 调用前检查。桥工具下,模型实际发起的 tool_use 名是 `mcp_call`,真实工具名在 `arguments.name` 里。gate 必须**对内层真实工具名**做判定,否则审批粒度退化为"是否允许 mcp_call"这一个粗粒度开关。loop 在见到 `mcp_call` 时解包 `arguments.name` 再交给 gate。同理 hooks / approval prompt 都跑在真实名上。

## 激活:auto 阈值

`tools.DefaultTools()` 本身 model-agnostic,而阈值判断需要 model 的上下文窗口。引入 `tools.DefaultToolsFor(model string)`:

- `DefaultTools()` == `DefaultToolsFor("")`,等价于关闭延迟(全量),保持所有现有调用点向后兼容。
- 知道 model 的调用点(`cmd/octo/chat.go`、`cmd/octo/channel.go`、sub-agent spawner)改调 `DefaultToolsFor(a.Model)`。

判定逻辑(`enabled: auto`):

```
估算 MCP schema tokens ≈ Σ(每个延迟工具 schema 的字节数) / 4
若  estTokens ≥ threshold_pct% × contextWindow(model)  →  启用桥
否则                                                    →  直接全量透传(零开销)
```

`contextWindow(model)` 已存在于 `internal/agent/compaction.go`,直接复用。阈值以下不引入任何桥,小工具集场景行为与现状一致。

## 配置

挂在 `~/.octo/config.yaml`(`internal/config/config.go` 的 `Config` 下):

```yaml
tools:
  tool_search:
    enabled: auto          # auto(默认) | on | off
    threshold_pct: 10      # auto 模式的激活阈值,占上下文窗口百分比
    search_default_limit: 5
    max_search_limit: 20
```

- `off`:现状,MCP 工具全量上传。
- `on`:只要有 MCP 工具就强制走桥,忽略阈值。
- `auto`:按上面的阈值决定。

## 时序

```
模型                      DefaultRegistry              MCP Registry
 │  mcp_search("create issue")    │                          │
 ├──────────────────────────────►│  BM25 over catalog       │
 │  [{name, one-line desc}, …]    │◄─────────────────────────┤
 │◄──────────────────────────────┤                          │
 │                                │                          │
 │  mcp_describe("mcp__gh__…")    │                          │
 ├──────────────────────────────►│  schema lookup           │
 │  full JSON Schema              │                          │
 │◄──────────────────────────────┤                          │
 │                                │                          │
 │  mcp_call("mcp__gh__…", {…})   │                          │
 ├──────────────────────────────►│  executeMCP(name, args)  │
 │                                ├─────────────────────────►│
 │  tool result                   │◄─────────────────────────┤
 │◄──────────────────────────────┤  (gate checks real name) │
```

## 兼容性

- **双协议**:桥在 tools 层,anthropic / openai 序列化均透明,无 provider 改动。
- **sub-agent**:spawner 持有 `tools.DefaultTools` 函数值;改传 `DefaultToolsFor(model)` 后子代理同样受益(子代理连同一套 MCP)。
- **`mcp__` 路由**:`mcp_call` 复用既有 `executeMCP`,零改动。
- **MCP 非文本结果**:现有 `formatToolResult` 已将 image/audio 压成占位符,桥不改变这点(独立 gap,不在范围)。

## 已知取舍

- **准确率收益不保证复刻官方数字**:Anthropic 公布的 49%→74%(Opus 4)/ 79.5%→88.1%(Opus 4.5)是在**原生**、模型受训的 tool-search 上测得。本设计是自建桥,确定收益是 token / 成本与缓存,准确率以实测为准。
- **多一次往返**:search → describe → call 比直接调多 1–2 个 tool turn;对只用少量 MCP 工具的任务,延迟略增,但省下的每轮 schema 重发通常更划算。

## 非目标

- 不实现 anthropic 原生 `defer_loading`(后续可叠加的独立优化)。
- 不延迟核心内置工具,只延迟 MCP(及未来非核心插件)工具。
- 不改变 MCP 非文本结果的处理。
- 不引入持久化索引或跨会话缓存——目录每轮无状态重建。

## 落地切片

1. BM25 索引 + 三个桥工具定义 + `DefaultRegistry` 派发(纯 tools 层,可独立测)。
2. 权限 gate 解包 `mcp_call.arguments.name` 后按真实工具名检查。
3. `DefaultToolsFor(model)` + auto 阈值 + 配置项 + 调用点迁移。

## 测试

- BM25 命中与排序、zero-IDF 子串回退。
- 三桥派发:`mcp_search` 返回不含 schema、`mcp_describe` 返回完整 schema、`mcp_call` 闭环到 `executeMCP`。
- auto 阈值边界(刚好低于 / 高于 `threshold_pct`)。
- gate 对内层真实名生效。
- 用 httptest 起假 MCP server 验证 `mcp_call` → `executeMCP` 全链路,遵循"go test 无 live network"约定。
