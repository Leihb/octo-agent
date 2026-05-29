# 可续话 Sub-Agent 设计（send_message）

## 背景与目标

octo 当前的 sub-agent 是一次性的：父 agent 通过 `launch_agent` 工具起一个子 agent，子 agent 跑完返回一段 final text，然后被丢弃。子 agent 没有可寻址的句柄，父 agent 无法在它完成首个任务后再唤醒它继续对话。

本设计新增一个 `send_message` 工具，让**顶层父 agent** 在 `launch_agent` 起的子 agent 完成首个任务后，还能带着完整上下文再次唤醒它继续干活——对标 Claude Code 的 `Agent` + `SendMessage` 续话模型，而非现在的 Codex 式 fire-and-forget。

### Scope

| 项 | 决定 |
|---|---|
| 保活范围 | **in-memory only**，registry 生命周期 = 一个 REPL 会话，不跨进程持久化 |
| 影响面 | **只动 `launch_agent` 这条线**，taskgraph（`octo goal`）的 `spawnerExecutor` 不改 |
| 谁能续话 | **只有顶层父 agent**；子 agent 拿不到 `send_message`（与现有「子 agent 不能 spawn」对称） |
| 驱逐策略 | LRU 上限 8 个 + 30 分钟空闲 TTL |
| close 工具 | 不做，只靠 LRU/TTL 自动回收 |

## 关键前提：agent 核心层零改动

`internal/agent/` 不需要任何改动。`Agent.Run` 本身就是续话友好的——`runLoop` 把用户输入追加进**同一个** `a.History`，而非重置：

```go
// internal/agent/agent.go:411
a.History.Append(NewUserMessage(userInput))
```

`History` 是 goroutine-safe 的消息日志（`internal/agent/history.go`，`popLast` 等操作持 `h.mu`）。每次 `Run` 还重新发一份 `MaxTurns` 预算（`turnLimit`，`agent.go:536`）。

因此「子 agent 完成后续话」在 agent 层是天然支持的：**只要还持有那个 `child *agent.Agent`，再 `child.Run(ctx, 后续消息, tools, exec)` 一次，它就接着上一轮聊**。当前做不到，唯一原因是上层把 child 用完即弃。

## 当前实现的相关事实

| 事实 | 位置 |
|---|---|
| `agentSpawner.Spawn` 把 child 建成局部变量，`Run` 完即返回 `SpawnResult` 后被 GC | `cmd/octo/sub_agent.go:38-74` |
| child 构造：共享 parent 的 `Sender` / `System` / `MaxTokens` / `Gate` / `MaxCostUSD`，`MaxTurns` 设为 `childMaxTurns = 12` | `cmd/octo/sub_agent.go:35,46-55` |
| `SpawnResult` 字段：`Reply` / `InputTokens` / `OutputTokens`，**无 ID** | `internal/tools/launch_agent.go:44-48` |
| `Spawner` 接口只有 `Spawn(ctx, req) (SpawnResult, error)` | `internal/tools/launch_agent.go:17-19` |
| `SpawnRequest` 字段：`Description` / `Prompt` / `Tools` / `Model` | `internal/tools/launch_agent.go:21-39` |
| token 计费：`Spawn` 取 `child.SessionTokens()`（**累计值**）后 `parent.AccrueChildUsage(in, out)` | `cmd/octo/sub_agent.go:66`、`internal/agent/agent.go:776,785` |
| `filterChildTools` 给子 agent 过滤工具时丢弃 `launch_agent`（防递归） | `cmd/octo/sub_agent.go:79-98` |
| 递归防护：`WithSubAgentMarker(ctx)` 打标，`IsSubAgent(ctx)` 检测，`launch_agent` Execute 据此拒绝嵌套 | `internal/tools/launch_agent.go:78-87,138-143` |
| `DefaultTools` 对 `LaunchAgentTool` 按 `spawnerEnabled()` 门控 | `internal/tools/registry.go:121-123`、`internal/tools/launch_agent.go:66` |
| `launch_agent` 在 `readOnlyTools` 里为 `true`，允许一个 tool_use batch 并行起多个子 agent | `internal/agent/agent.go:625-632` |
| spawner 会话级接线：`SetSpawner(newAgentSpawner(a, …))` 每会话一次 | `cmd/octo/chat.go:329` |

## 方案设计

改动集中在 `cmd/octo/sub_agent.go`（新增 registry + 改造 `agentSpawner`）和一个新工具文件，外加 `internal/tools/launch_agent.go` 的接口与 `SpawnResult` 扩展。`internal/agent/` 只读不改。

### 1. SpawnResult 加 AgentID，Spawner 加 Continue

`internal/tools/launch_agent.go`：

```go
type SpawnResult struct {
	AgentID      string // 可寻址句柄；send_message 用它定位 child。Spawn 必填
	Reply        string
	InputTokens  int
	OutputTokens int
}

type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) (SpawnResult, error)
	// Continue 用 message 续跑一个仍存活的 child，返回它这一轮的新 reply。
	// agentID 未知或已被驱逐时返回错误，错误文案提示模型重新 launch_agent。
	Continue(ctx context.Context, agentID, message string) (SpawnResult, error)
}
```

taskgraph 的 `spawnerExecutor`（`cmd/octo/task.go:573`）只调 `Spawn`，加 `Continue` 不影响它。

### 2. childRegistry + liveChild（in-memory，session-scoped）

新增到 `cmd/octo/sub_agent.go`。registry 作为 `agentSpawner` 的字段，生命周期随 spawner（一个 REPL 会话）。

```go
type liveChild struct {
	agent       *agent.Agent          // 保活的 child，History 持续累积
	tools       []agent.ToolDefinition // 该 child 当初过滤后的 toolbelt（续话沿用）
	executor    agent.ToolExecutor
	mu          sync.Mutex            // 串行化对同一 child 的调用（坑①）
	lastUsed    time.Time             // LRU + TTL 依据
	lastAccrued struct{ in, out int } // 已计入 parent 的累计 token（坑②）
}

type childRegistry struct {
	mu  sync.Mutex
	m   map[string]*liveChild
	now func() time.Time // 注入便于测试
}

const (
	maxLiveChildren = 8                // LRU 上限
	childIdleTTL    = 30 * time.Minute // 空闲驱逐
)
```

registry 行为：

- **put(child, tools, exec) → id**：生成 8 位 hex id（与 `agent.Session.ShortID` / `taskgraph.shortTaskID` 同风格），写入 map，置 `lastUsed = now`。put 前先 `evict()`。
- **get(id) → \*liveChild, ok**：命中则刷新 `lastUsed`。先 `evict()` 再查，使过期项查不到。
- **evict()**（持 registry.mu）：先删 `now - lastUsed > childIdleTTL` 的项；再若 `len(m) > maxLiveChildren`，按 `lastUsed` 升序删到 8 个。被删项不需要额外清理（无文件、无连接，GC 即回收）。

### 3. agentSpawner 改造

`Spawn` 跑完**不丢弃 child**，而是入 registry 并把 id 回填到 `SpawnResult.AgentID`；新增 `Continue`：

```go
func (s *agentSpawner) Spawn(ctx context.Context, req tools.SpawnRequest) (tools.SpawnResult, error) {
	childTools := filterChildTools(s.toolsFn(), req.Tools)
	model := req.Model
	if model == "" {
		model = s.parent.Model
	}
	child := agent.New(s.parent.Sender, model)
	child.System = s.parent.System
	child.MaxTokens = s.parent.MaxTokens
	child.Gate = s.parent.Gate
	child.MaxTurns = childMaxTurns
	child.MaxCostUSD = s.parent.MaxCostUSD

	lc := &liveChild{agent: child, tools: childTools, executor: s.executor}
	id := s.reg.put(lc) // 先登记，拿到 id

	reply, err := s.runChild(ctx, lc, req.Prompt)
	if err != nil {
		return tools.SpawnResult{}, err
	}
	return tools.SpawnResult{AgentID: id, Reply: reply, /* 本轮增量 token */}, nil
}

func (s *agentSpawner) Continue(ctx context.Context, agentID, message string) (tools.SpawnResult, error) {
	lc, ok := s.reg.get(agentID)
	if !ok {
		return tools.SpawnResult{}, fmt.Errorf(
			"agent %s is no longer alive (idle-expired or evicted); launch a fresh sub-agent instead", agentID)
	}
	reply, err := s.runChild(ctx, lc, message)
	if err != nil {
		return tools.SpawnResult{}, err
	}
	return tools.SpawnResult{AgentID: agentID, Reply: reply}, nil
}
```

`runChild` 是 `Spawn` / `Continue` 共用的核心，负责**串行锁 + 递归 marker + 增量计费**：

```go
func (s *agentSpawner) runChild(ctx context.Context, lc *liveChild, prompt string) (string, error) {
	lc.mu.Lock()          // 坑①：串行化对同一 child 的调用
	defer lc.mu.Unlock()

	childCtx := tools.WithSubAgentMarker(ctx) // 坑④：续话仍要打标防递归
	reply, err := lc.agent.Run(childCtx, prompt, lc.tools, lc.executor)
	if err != nil {
		return "", err
	}

	// 坑②：SessionTokens 是累计值，只 accrue 自上次以来的增量
	in, out := lc.agent.SessionTokens()
	s.parent.AccrueChildUsage(in-lc.lastAccrued.in, out-lc.lastAccrued.out)
	lc.lastAccrued.in, lc.lastAccrued.out = in, out

	return reply.Content, nil
}
```

### 4. send_message 工具

新增 `internal/tools/send_message.go`，与 `launch_agent` 同样按 `spawnerEnabled()` 门控（在 `registry.go` 的 `DefaultTools` 里加一条 `SendMessageTool` 的 spawner 门控分支，紧挨现有 `LaunchAgentTool` 分支 `registry.go:121-123`）。

工具 schema：

```
name: send_message
description: 续跑一个之前用 launch_agent 起的、仍存活的 sub-agent，带上一段新消息，
            返回它这一轮的新回复。子 agent 记得它之前做的所有事。当一个子任务需要
            多轮往返、或要基于前一轮结果追加指令时使用。如果返回 "no longer alive"，
            改用 launch_agent 重新起一个。
parameters:
  agent_id (string, required): launch_agent 返回里的 agent id
  message  (string, required): 发给该 sub-agent 的新消息（它看不到本对话，消息要自包含）
```

Execute：与 `launch_agent` 同样先查 `spawnerEnabled()`、`IsSubAgent(ctx)`（子 agent 调用直接拒绝），再 `ActiveSpawner().Continue(ctx, agentID, message)`，空 reply 时返回 `(sub-agent <id> produced no reply)`（沿用 `launch_agent.go:167-172` 的处理）。

`send_message` **不进 `readOnlyTools`**（`agent.go:625`）。原因见坑①：若进了并行集合，模型一个 batch 里对同一 id 发两条会并发 `Run` 同一 child；不进则该工具整批串行执行，从调度层就避免了并发同一 child。per-child mutex 是第二层兜底。

### 5. 透出 AgentID 给父模型

`launch_agent` 的 Execute（`internal/tools/launch_agent.go:162-174`）返回文本前面带上 id，否则模型不知道往哪发：

```
[agent a3f9c2b1] <child reply>
```

`send_message` 的返回同样带 `[agent <id>] ` 前缀，保持模型对「同一个子 agent」的指代一致。

### 6. 强制「只有顶层父 agent 能用」（决策3）

`filterChildTools`（`cmd/octo/sub_agent.go:79-98`）当前丢弃 `launch_agent`，扩展为**同时丢弃 `send_message`**：

```go
if td.Name == "launch_agent" || td.Name == "send_message" {
	continue
}
```

子 agent 的 toolbelt 里没有 `send_message`，结构上保证它不能唤醒别的 child；`IsSubAgent(ctx)` 检查是第二层兜底（防模型幻觉出一个不在 schema 里的工具调用）。这与现有「子 agent 不能 spawn」完全对称。

## 四个坑的处理小结

| # | 坑 | 处理 |
|---|---|---|
| ① | 同一 child 并发 Run 破坏 History 交替 | `send_message` 不进 `readOnlyTools`（整批串行）+ `liveChild.mu` 串行化 |
| ② | `SessionTokens` 累计值被多轮重复 accrue 双计 | `liveChild.lastAccrued` 记上次累计，每轮只 accrue 增量 |
| ③ | 保活 child 连着 History 泄漏内存 | LRU 8 + 30min TTL，put/get 前 `evict()`；registry 随 spawner（会话）销毁 |
| ④ | 续话漏打递归 marker | `runChild` 统一 `WithSubAgentMarker(ctx)`，`Spawn` / `Continue` 共用 |

## 时序图

```mermaid
sequenceDiagram
    participant M as 父模型 (LLM)
    participant LA as launch_agent
    participant SP as agentSpawner
    participant REG as childRegistry
    participant C as child Agent

    M->>LA: tool_use launch_agent(prompt)
    LA->>SP: Spawn(req)
    SP->>C: agent.New + 配置
    SP->>REG: put(child) → id=a3f9
    SP->>C: runChild: Run(prompt) [lock a3f9]
    C-->>SP: reply1 (+token 增量)
    SP-->>LA: SpawnResult{AgentID:a3f9, reply1}
    LA-->>M: "[agent a3f9] reply1"

    Note over M: 父 agent 决定追加指令

    M->>SP: tool_use send_message(a3f9, msg2)
    SP->>REG: get(a3f9) → 命中, 刷新 lastUsed
    SP->>C: runChild: Run(msg2) [lock a3f9]
    Note over C: History 续上 reply1 之后
    C-->>SP: reply2 (+token 增量)
    SP-->>M: "[agent a3f9] reply2"

    Note over REG: 30min 空闲 / 超 8 个 → evict 回收
    M->>SP: send_message(a3f9, msg3) 若已驱逐
    SP->>REG: get(a3f9) → miss
    SP-->>M: error "no longer alive; launch fresh"
```

## 测试计划

放在 `cmd/octo/sub_agent_test.go`（扩展现有）和新增 `internal/tools/send_message_test.go`，沿用 stdlib + 现有 fake Sender/Spawner，无真实网络。

| 用例 | 断言 |
|---|---|
| Spawn 后 Continue 续话 | child History 累积；reply2 能看到 reply1 的上下文（用 fake Sender 回显历史长度验证） |
| token 不双计 | 两轮后 `parent.SessionTokens` == 两轮增量之和，而非 累计×2 |
| 并发 send_message 同一 id | per-child mutex 串行执行，无 data race（`-race` 下跑）；History 交替合法 |
| 未知 / 已驱逐 id | `Continue` 返回 "no longer alive" 错误 |
| LRU 驱逐 | put 第 9 个后最久未用的被删，`get` 旧 id miss |
| TTL 驱逐 | 注入 `now` 推进 31min，`get` miss |
| 子 agent 拿不到 send_message | `filterChildTools` 输出不含 `send_message` 和 `launch_agent` |
| 子 agent 调 send_message 被拒 | `IsSubAgent(ctx)` 为真时 Execute 返回错误 |
| spawner 未配置 | `send_message` 不在 `DefaultTools`，Execute 报 "not configured" |

## 兼容性

- **现有 `launch_agent` 行为**：不变。父模型不调 `send_message` 时，Spawn 的唯一新增副作用是把 child 留在 registry 里（最终被 LRU/TTL 回收），返回文本多了 `[agent <id>] ` 前缀。
- **taskgraph（`octo goal`）**：不涉及。`spawnerExecutor` 只调 `Spawn`；其 spawner 的 registry 没人调 `Continue`，留存的 child 随该次 `octo goal run` 进程退出一并释放。`octo goal` 是前台一次性进程，不存在跨会话泄漏。
- **session JSON 持久化**：不涉及。registry 是纯 in-memory，不写盘、不进 session 文件；进程退出即清空（决策1：不跨进程持久化）。
- **provider / wire 格式**：不涉及。child 复用 parent 的 `Sender`，续话只是对同一 `Sender` 多发几轮，与单 agent 多轮对话走同一路径。
- **权限门控**：不涉及新增。`send_message` 复用 `spawnerEnabled()` 门控；child 的 `Gate` 继承自 parent（`sub_agent.go:49`），续话沿用同一 Gate。

## 回滚

代码与数据可独立回滚——本特性无数据层改动（纯 in-memory）。回滚 = revert 代码：移除 `send_message` 工具后它从 `DefaultTools` 消失，模型不再看到该工具；`SpawnResult.AgentID` 和 `Spawner.Continue` 即便保留也无副作用（Spawn 路径与回滚前等价，唯一区别是 child 多停留在 registry 直到 GC）。无需数据迁移。

## 不做的事（明确排除）

- 不做跨进程 / 跨会话持久化续话（决策1）。需要时另设计：序列化 child History（可复用 `internal/agent/session.go` 机制）。
- 不改 taskgraph 让 subtask 之间续话（决策2）。DAG 仍靠 `Subtask.Result` 文本传递。
- 不给子 agent `send_message`（决策3）。
- 不做 `close_agent` 显式释放工具，只靠 LRU/TTL 自动回收。
- 不做 child 的事件流透出（与现状一致：父只拿 final string，不订阅 child 的 AgentEvent）。
