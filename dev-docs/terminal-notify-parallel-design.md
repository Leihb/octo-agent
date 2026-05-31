# Terminal 完成通知 + 多后台进程并发（design）

> 给后台命令（`terminal background:true`）加「完成即推送」的通知，取代当前的纯轮询；
> 并让 agent 能同时跑多个后台任务、跑的过程中继续工作、每个完成时被推送回收。
> 实现寄生在 [tui-input-modes-design.md](tui-input-modes-design.md) 引入的原语上，**实现需等那条方案先落地**（见 §0）。

---

## 0. 前置依赖（重要）

本方案**不独立可实现**。它的通知注入与事件投递完全依赖 `tui-input-modes` 引入的三个原语：

- **inbox 缓冲 + 迭代开始 drain**（tui §5.1）——通知就是往 inbox 塞一条。
- **agent loop 跑在后台 goroutine + `AgentEvent`→`tea.Msg` 事件总线**（tui §3/§5）。
- **`ViewSink`**（tui §4）——事件出 `Emit`、请求-响应 `Ask`。

没有这些，后台完成就只能退回今天的纯轮询（模型自觉调 `terminal_output`，`internal/tools/terminal.go:203`）。

**落地顺序**：`tui-input-modes` 合并 → 本方案在其原语上实现。本文档先把设计定下来，便于那条分支落地时同步收掉。

---

## 1. 目标与范围

### 解决的问题

后台进程完成时**没有任何主动通知**。机制是纯拉取：

- `BackgroundManager` 的 waiter goroutine 在 `cmd.Wait()` 返回后调 `bgProcess.finish(err)`（`internal/tools/background.go:40`），**只 latch `done`/`exitErr` 内部状态，不向 agent 发任何信号**。
- agent 只有主动调 `terminal_output`（`terminal.go:203` → `BackgroundManager.Read` → `readNew`，`background.go:50`）才读到 `[status: exited: 0]` 和增量输出。

后果：模型若不自觉回来轮询，**永远不知道后台任务结束了**。也因此「同时跑多个后台任务再逐个收割」这种并行工作流，今天靠纯轮询很别扭。

### 目标

1. **通知**：后台进程完成时，把「退出状态 + 退出时未读的增量输出」**主动推**给 agent（注入对话）和 UI（后台区渲染）。
2. **并行**：让 agent fire-and-forget 多个后台任务并发跑，跑的过程中继续别的工作，每个完成时被推送回收——**进程级并行、回合级串行**。

### 范围内

- `BackgroundManager` 增加完成出口（回调/事件），由 turn-core 注入。
- 新 `AgentEvent` 类型 `BgExited`，经事件总线投递。
- 通知**复用 inbox 缓冲**注入对话：回合运行中走迭代开始 drain、idle 走下一回合前置。
- 通知形态为 `<system-reminder>` text block（环境事件，非用户指令）。
- UI 后台区渲染在跑/已完成的后台进程。
- `terminal_output` 拉取保留（与推送去重共存）。

### 范围外（见 §8）

- **把批内并行 dispatch 扩展到 mutating 工具**——另一条并行轴。注意：**只读批的并行 dispatch 现状已存在**（`dispatchTools` 的 `canParallelize`，`internal/agent/agent.go:705`，只读白名单 `readOnlyTools` `agent.go:602`：read_file/glob/grep/web_fetch/web_search/launch_agent，>1 个时开 goroutine 并发）。本方案不做的是把它扩到会写/会 shell-out 的工具（terminal、edit_file…），那侵入大（写冲突、gate 串行化、结果对齐，见 §8）。
- **多回合并行**——沿 tui-input-modes §12 否决，始终一次一个在途回合。
- 桌面/系统级通知；inbox 持久化。

---

## 2. 决策记录（grill 已定）

| # | 决策 | 选择 | 理由 |
|---|---|---|---|
| 1 | 通知注入路径 | **复用 inbox 缓冲**，不新建通道 | bg 完成 = 系统自动生成的一条 inbox 条目，与用户输入共用 tui §5.1 的注入点 |
| 2 | 注入形态 | 包成 `<system-reminder>` text block，并入尾部 `tool_result` 消息 | 让模型当**环境事件**读而非用户指令；复用 octo 已有的 system-reminder 约定（memory nudge 同款）；不破 user/assistant 交替不变量 |
| 3 | 生效时机 | 回合运行中→**下一个迭代开始**（同 inbox）；idle→暂存，**下一回合开始前**前置注入 | 不浪费在途工作；idle **不自动起回合**（守 tui §12「无自动回合」） |
| 4 | 通知是否进 UI | 是，bg 事件同时 `Emit` 给 ViewSink，渲染进「后台区」 | 用户可见，配合并行多任务的可观测性 |
| 5 | 并行主轴 | **多后台进程并发**（`BackgroundManager` 已支撑），不引入多回合并行 | fire-and-forget + 推送回收；守住 tui §12 |
| 6 | 批内并行工具 dispatch | 只读批**已并行**（现状，`canParallelize`）；扩展到 mutating 工具**不做**（§8） | 只读批天生无副作用、不流式，已安全并发；写工具有写冲突/gate 串行化/结果顺序三个硬问题，另案 |
| 7 | 同周期多个完成 | 同一 drain 周期内多个 bg 完成**合并成一条** system-reminder | 避免事件风暴 |
| 8 | interrupt 与后台 | Esc 只停当前回合，**不杀后台进程、不清 bg inbox** | 后台任务独立生命周期；完成通知在后续回合照常送达 |
| 9 | 拉取共存 | 保留 `terminal_output`；`readNew` 游标推进天然去重 | 推过的不会被再拉到；中途输出仍可主动查 |

---

## 3. 架构总览

```
 BackgroundManager.finish(err)         ← waiter goroutine (background.go:124-128)
        │  新增：finish 后调 onExit
        ▼
   BgExit{id, command, status, newOutput}
        │
        ├───────────────▶ ViewSink.Emit(BgExitedEvent)     → TUI 后台区渲染 (决策#4)
        │
        └───────────────▶ Agent.Inbox.Enqueue(item{kind: bg})
                               │  复用 tui §5.1 的线程安全 inbox 缓冲
                               ▼
              runLoop 迭代开始 drain → 并入 tool_result 的 system-reminder block
              （idle 时：turn-core 在下个回合开始前前置注入）
```

唯一新增的数据流是 `finish()` 多一个出口；下游（inbox、事件总线、ViewSink、注入点）全是 tui-input-modes 已建的原语。

---

## 4. 通知机制

### 4.1 BackgroundManager 增完成出口

`bgProcess.finish`（`background.go:40`）今天只 latch `done`/`exitErr`。改为 finish 后触发一个由 turn-core 注入的 `onExit` 回调，回调里调 `readNew()`（`background.go:50`，拿退出时仍未读的增量 + 最终 status），打包投出：

```go
// 新类型
type BgExit struct {
    ID, Command string
    Status      string // 复用 readNew 的 "exited: 0" / "exited: <err>"
    NewOutput   string // 退出时仍未被 Read 消费的尾部输出
}

// Start() 注入回调（background.go:90 内）：
p := &bgProcess{ id: id, command: command, cancel: cancel, onExit: m.onExit }

// waiter goroutine（background.go:124）：
err := cmd.Wait()
_ = pw.Close()
p.finish(err)
if p.onExit != nil {
    out, st := p.readNew()           // 推进游标 → 与 terminal_output 去重 (决策#9)
    p.onExit(BgExit{p.id, p.command, st, out})
}
```

- `onExit` 由 turn-core 建 manager 时注入：TTY 视图 → 包成 `AgentEvent` 投事件总线；headless 视图 → 一个纯文本打印（或 no-op）的回调，`--no-tui`/mswe-eval 行为可控。
- 线程安全：回调在 waiter goroutine 触发，写的是 tui §5.1 的线程安全 inbox 缓冲；`History` 本身也有 `sync.RWMutex` 兜底（`internal/agent/history.go`）。

### 4.2 注入形态（决策#2）

inbox 条目带 `kind`：`typed`（用户输入）/ `bg`（后台完成）。drain 时按 kind 渲染：

- `typed` → 裸 text block（用户的话，tui 原方案行为不变）。
- `bg` → 包成 system-reminder：

  ```
  <system-reminder>Background process bg_1 (`go test ./...`) exited: 0.
  Output since last check:
  ok  github.com/Leihb/octo-agent/internal/tools  1.2s
  </system-reminder>
  ```

并入尾部 `tool_result` 消息的 blocks（与 tui §5.1 inbox 注入同一处，不新起 user 消息）。

### 4.3 两种时机（决策#3）

- **回合运行中**：bg 完成 → inbox → `runLoop` 下次迭代开始 drain 注入。模型在干活途中即得知「后台起的 server 已就绪 / 测试已跑完」。
- **idle**（无在途回合）：bg 条目暂存 inbox，**不自动起回合**；turn-core 在下一个回合**开始前**把累积的 bg 条目作为前置 system-reminder 注入首条消息。UI 后台区即时显示。
- 这两条与 tui §5.1（inbox）/ §5.2（降级为后续回合）是同一套消费逻辑，bg 只多一个 kind。

> 取舍：idle 且用户再不发起回合 → 模型不会看到通知。可接受——没有在途工作要它反应；UI 已显示，且 `terminal_output` 仍可主动查。不为此引入「自动起回合」（破 tui §12）。

### 4.4 与拉取共存（决策#9）

`terminal_output`（`terminal.go:203`）保留。`readNew` 游标在 push 时已推进，所以推送过的增量不会被再拉到（自动去重）。模型仍可主动查**未完成**进程的中途输出。

### 4.5 中断 / 退出（决策#8）

- Esc interrupt 只取消当前回合 `turnCtx`（tui §5.2），**不杀后台进程、不清 bg inbox**——后台任务独立存活，完成通知在后续回合照常送达。
- 会话退出 `KillAllBackground()`（`background.go:175`）不变。

---

## 5. 多后台进程并发（决策#5）

**主轴：进程并行、回合串行。**

`BackgroundManager` 本就是 `map[id]*bgProcess`、每进程两个独立 goroutine（reader `background.go:116` + waiter `background.go:124`），天然支持 N 个并发。配合 §4 的推送，agent 的并行工作流：

1. agent 在一个或多个工具批次里发起 N 个 `terminal background:true`（编译、测试、起服务…），各拿一个 `bg_k`，立即返回不阻塞。
2. agent 继续做别的（读码、改文件、回应用户）。
3. 每个 bg 完成 → 下次迭代开始以 system-reminder 注入 → 模型按**完成顺序**逐个反应。

这就是「并行运行」：进程并行、回合串行——不碰 tui §12 否决的多回合并行。

**配套增强**（低成本，建议一并做）：

- `terminal_output` 支持一次读多个 id，或新增「列出所有后台进程及状态」的能力，让模型能盘点在跑的并行任务。
- inbox 注入 system-reminder 时带「还有 M 个后台进程在跑」摘要，模型好决定要不要等。

---

## 6. UI（后台区）

复用 tui §7 的常驻分区思路，队列区旁/内增「后台区」：

```
├─ 后台 (2 running, 1 done) ─────────────────────────┤
│  bg_1  ⟳ running   go test ./...                   │
│  bg_2  ⟳ running   npm run dev                      │
│  bg_3  ✓ exited:0  go build ./...   (已通知)        │
├─ 队列 (1) ─────────────────────────────────────────┤
│  1. [queue] 跑一下 lint                            │
```

- `running`/`exited` 状态来自 `readNew` 的 status；完成的标「已通知」表示已注入对话。
- 具体键位（如查看某个 bg 全量输出、kill）tech-design 细化。

---

## 7. 测试（stdlib + httptest + mock ViewSink，沿用 tui §9 路线）

- **BackgroundManager `onExit`**：mock 回调，断言 finish 后收到 `BgExit{status, newOutput}` 且游标已推进（后续 `Read` 报 no new output → 去重）。
- **inbox `bg` kind 渲染**：断言注入成 `<system-reminder>` block（非用户裸 text block）、不产生连续两条 user role。
- **时机**：mock Sender 产 tool_use → 回合运行中 bg 完成走下个迭代开始注入；idle → 暂存 → 下个回合前置注入、**不自动起回合**。
- **并发**：起 N 个 bg、令其乱序完成，断言通知按完成序、同 drain 周期内多个完成合并成一条（决策#7）。
- **interrupt 存活**：cancel `turnCtx`，断言后台进程未被杀、bg inbox 仍被后续回合消费（决策#8）。
- **headless 回归**：`onExit` 走纯文本回调，`--no-tui`/mswe-eval/管道行为可控、既有测试绿。

---

## 8. 不做 / 未来

- **把批内并行 dispatch 扩展到 mutating 工具**——独立 tech-design。**现状**：`dispatchTools`（`internal/agent/agent.go:635`）对**全只读**的批次（`canParallelize` `agent.go:705` + `readOnlyTools` `agent.go:602`，>1 个调用）已经开 goroutine 并发；含任何会写/会 shell-out 的工具则退回串行（保流式进度），权限 gate 始终先串行跑一遍（`agent.go:636`）避免抢 stdin。把并行扩到 mutating 工具的三个硬问题：① 写冲突（并发 `edit_file` 同文件 / `terminal` 改 cwd）需并发策略；② 多工具抢 tui §6 的单权限模态需 Ask 排队；③ `tool_result` blocks 须按 `tool_use_id` 对齐而非完成序。
- **多回合并行**——沿 tui §12 否决，始终一次一个在途回合，inbox 串行消费。
- **桌面/系统级通知**——CLI 内事件足够。
- **bg inbox 持久化**——瞬态，会话退出即丢（同 tui §12）。
