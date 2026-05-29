# TUI 输入模式 — Queue / Steer / Interrupt（design）

> 给交互式 REPL 引入「回合运行时也能输入」的能力：排队（queue）、中途引导（steer）、打断（interrupt）。
> 实现手段是把交互式 TTY 路径迁到 bubbletea 事件循环，非-TTY 保留纯文本路径。

---

## 1. 目标与范围

### 解决的问题

当前 REPL 主循环是**单 goroutine 同步阻塞**的：读一行 → `RunStream` 跑完整个 agentic 回合（全程阻塞）→ 再读下一行（`cmd/octo/repl.go:146` 的 for 循环，回合在 `:271`）。回合运行期间没人读 stdin，用户键入的字符只是停在终端缓冲里，等回合结束才被当作**下一条 prompt** 读走。

后果：用户在 agent 忙的时候**完全无法表达意图**——既不能排队下一件事，也不能在 agent 跑歪时中途纠偏。唯一能影响在途回合的手段是 Ctrl-C（`repl.go:109-132` 的信号 goroutine 取消 `turnCtx`），即已有的 interrupt。

### 目标

在交互式 TTY 上，回合运行时用户可以：

- **Steer**（裸 Enter）：键入的话注入到**正在跑的回合**，在下一个工具批次边界生效，引导 agent 改变方向而不打断它。
- **Queue**（Alt+Enter）：键入的话排进队列，当前回合**完整结束后**作为新回合自动执行。
- **Interrupt**（Esc，无模态时）：停掉当前在途回合（保留已有的 Ctrl-C 语义并对齐到 Esc）。

### 范围内

- 交互式 TTY 的 REPL 渲染层迁到 bubbletea。
- 抽出与渲染解耦的 **turn-core**，TTY 视图与非-TTY 纯文本视图共享。
- permission gate / `ask_user_question` 改为 channel 请求-响应 + TUI 模态。
- pending 缓冲（queue/steer 共用）、常驻队列区 UI。
- `mswe-eval` 从「piped REPL 驱动」迁到专用 headless 入口。
- `--no-tui` 兼容性回退。

### 范围外（见 §11）

Web/IM 端的等价能力、pending 缓冲持久化、队列消息的富文本编辑、steer 的「立即打断重发」语义。

---

## 2. 决策记录（grill 已定）

| # | 决策 | 选择 | 理由 |
|---|---|---|---|
| 1 | 行为选择方式 | 单输入框、按键决定 | 三种行为在一个框里靠键区分，不引入有状态模式；对齐 Claude Code |
| 2 | Steer 生效时机 | 下一个工具批次边界注入 | 不浪费在途工作；「手头这步做完听我一句再继续」 |
| 3 | Steer 注入机制 | 文本作为额外 text block 并入尾部 `tool_result` 消息 | Messages API 官方支持的多-block 形态，不破 user/assistant 交替不变量 |
| 4 | 终端输入层 | 全面上 bubbletea | line-based readline 无法在流式输出时后台读键；长期最稳，且已在 charmbracelet 生态（lipgloss） |
| 5 | TTY/非-TTY 共存 | `stdinIsTTY()` 分流 + 共享 turn-core + 双视图；顺带迁 mswe-eval | bubbletea 需 TTY；piped/测试/CI 必须保留纯文本路径 |
| 6 | gate / asker | channel 请求-响应 + TUI 模态，共用一套；模态 Esc = 取消单个工具、回合继续 | bubbletea 处理「后台工作需用户输入」的标准范式 |
| 7 | 键位 | 裸 **Enter = steer**、**Alt+Enter = queue**；Esc 上下文相关 | 默认即时引导；Alt+Enter 可靠（Ctrl/Shift+Enter 终端不可靠） |
| 8 | Steer 无边界可注入 | 自动降级为 queue | 消息永不丢；queue/steer 共享一个 pending 缓冲，仅消费时机不同 |
| 9 | Esc 打断与 pending | Esc 只停当前回合，pending 保留作后续回合 | interrupt 收窄到在途回合，队列独立生命周期 |
| 10 | 队列可见性 | 常驻队列区 + 逐条/全部撤回 | 配套 #9 的「pending 存活」，提供撤回安全阀 |
| 11 | 落地策略 | 单个大 PR | 用户选择；已知风险：review 难、难二分 |
| 12 | 回退 | 保留 `--no-tui` 强制走纯文本路径 | 兼容性兜底（dumb terminal/SSH/tmux/伪 TTY/屏幕阅读器），边际成本近零 |

---

## 3. 架构总览

```
                    ┌─────────────────────────────────────────┐
                    │            cmd/octo (CLI 层)              │
                    │                                           │
   stdinIsTTY()?    │   ┌──────────────┐    ┌───────────────┐  │
   ───────┬─────────┼──▶│  TTY 视图     │    │ headless 视图  │◀─┼── 非-TTY
          │         │   │ (bubbletea)  │    │ (纯文本/scanner)│  │   --no-tui
          │         │   └──────┬───────┘    └───────┬───────┘  │
          │         │          │  实现 ViewSink      │          │
          │         │          └─────────┬──────────┘          │
          │         │              ┌─────▼──────┐              │
          │         │              │  turn-core  │  ← 抽出的共享核心
          │         │              │ (无渲染依赖) │              │
          │         │              └─────┬──────┘              │
          └─────────┴────────────────────┼──────────────────────┘
                                          │ AgentEvent / 请求-响应
                          ┌───────────────▼────────────────┐
                          │     internal/agent (后台 goroutine) │
                          │  RunStream → runLoop（注入点在此） │
                          └────────────────────────────────┘
```

三层关键变化：

1. **turn-core**（新）：把现有 `runREPL` 里与渲染无关的回合编排逻辑（pre/post hook、auto-save、cost gate、memory nudge、turnCtx 生命周期）抽成一个不依赖具体渲染的核心，对外暴露一个 `ViewSink` 接口（事件出、请求-响应进）。TTY 视图和 headless 视图都驱动它。

2. **agent loop 移到后台 goroutine**：bubbletea 的 `tea.Program` 在主 goroutine 跑事件循环，agent 必须在 goroutine 里跑，通过 `tea.Msg` 把 `AgentEvent` 投递给 Model，通过 channel 接收 steer/queue/interrupt 信号。

3. **pending 缓冲 + 注入点**：queue/steer 共用一个线程安全的 pending 缓冲；`runLoop` 在每个工具批次边界 drain 它（steer 生效），回合自然结束时若仍有内容则作为下一个回合启动（queue / steer 降级）。

---

## 4. turn-core 抽象

现状：`runREPL`（`cmd/octo/repl.go:50`）把回合编排和终端渲染**糅在一起**——`replToolEventHandler`（`cmd/octo/repl.go:590`）直接 `fmt.Fprint` 到 stdout。要双视图共享，必须先解耦。

### ViewSink 接口（草案）

```go
// turn-core 通过 ViewSink 与渲染层通信，自身不知道是 bubbletea 还是纯文本。
type ViewSink interface {
    // 流式事件出口（替代当前直接 Fprint 的 replToolEventHandler）。
    Emit(ev agent.AgentEvent)

    // agent 需要用户给一个结构化答复时调用，阻塞直到 ViewSink 回填。
    // permission gate 与 ask_user_question 共用此通道（决策 #6）。
    Ask(ctx context.Context, req UserPrompt) (UserResponse, error)

    // 回合开始/结束的钩子，供视图清理输入区、刷新队列等。
    TurnStarted()
    TurnEnded(reply agent.Reply, err error)
}
```

- **TTY 视图**：`Emit` → 把事件包成 `tea.Msg` 投给 Program；`Ask` → 发模态请求、阻塞等 Model 回填。
- **headless 视图**：`Emit` → 复用现有 `replToolEventHandler` 的纯文本逻辑；`Ask` → 复用现有 `cliPermissionGate` 从 stdin 读。

turn-core 保留的现有逻辑（从 `runREPL` 平移，不改行为）：pre-turn hook（`repl.go:242-248`）、memory nudge（`repl.go:239-241`）、post-turn hook（`repl.go:290-294`）、auto-save（`repl.go:325-331`）、cost/turn 上限（已在 `runLoop`）、`turnCtx` 的创建与取消（`repl.go:224-283`）。

---

## 5. agent loop 的并发改造与注入点

`internal/agent/runLoop`（`internal/agent/agent.go:386`）是唯一的 agentic 循环。改造点有二：

### 5.1 steer 注入点（决策 #2/#3）

循环每次迭代在工具批次后是 `append(tool_use, assistant)` → `append(tool_result, user)` → `continue`（`agent.go:434-455`）。因此循环顶看到的 history 在多步 loop 中**必然以 `user(tool_result)` 结尾**——这正是 steer 要落脚的地方。

注入方式（决策 #3）：在 `append(NewToolResultMessage(...))` 之前 drain 一次 pending 缓冲；若有 steer 内容，把它作为**额外的 text block 拼进 tool_result 消息的 blocks**，而不是新起一条 user 消息（避免连续两条 user role 破坏交替不变量）。

```go
// runLoop 内，构造 tool_result 消息时：
resultBlocks := /* dispatchTools 产出 */
if steer := a.drainSteer(); steer != "" {
    resultBlocks = append(resultBlocks, TextBlock(steer))  // 并入同一条 user 消息
}
a.History.Append(NewToolResultMessage(resultBlocks))
```

- pending 缓冲由 cmd/octo 经一个 channel 或 `Agent` 上的线程安全方法注入；`runLoop` 在自己的 goroutine 里 drain，**无数据竞争**（`History` 本身也有 `sync.RWMutex` 兜底，见 `internal/agent/history.go`）。
- **降级（决策 #8）**：若回合走到终止（模型不再 tool_use，`agent.go:458-468`）时 pending 仍有内容，turn-core 在 `TurnEnded` 后把它作为下一个回合的 userInput 启动 —— 这就是 queue / steer-无边界 的统一兜底。

### 5.2 interrupt（决策 #9）

沿用现有机制：每回合独立 `turnCtx`，Esc（无模态）调用 `turnCancel()`，`runLoop` 在迭代边界检测 `ctx.Err()` 并走 `finishInterrupted`（`internal/agent/agent.go:492`）把 history 收尾成 well-formed。

**关键差异**：Esc 只取消 `turnCtx`，**不动 pending 缓冲**——pending 消息存活，turn-core 在 interrupt 收尾后照常消费它们作为后续回合。

---

## 6. permission gate / ask_user_question（决策 #6）

现状：`cliPermissionGate`（接到 `a.Gate`，`repl.go:138-144`）和 asker 在 `RunStream` 内部**同步**从共享 reader 读 stdin。bubbletea 接管后 stdin 归事件循环独占，且 agent 在后台 goroutine，必须改成跨 goroutine 的请求-响应。

```
agent goroutine                     bubbletea 主 goroutine
─────────────                       ──────────────────────
gate.Ask(req)                       Update: 收到 UserPrompt
  └─ send req → ViewSink.Ask        └─ 进入模态状态，View 渲染选项卡
       (阻塞在 respCh)               用户按键选择 / Esc
                                     └─ 写 respCh ← UserResponse
  ←── 解除阻塞，继续 dispatch
```

- gate 与 asker **共用** `ViewSink.Ask` 这一通道（两者本质都是「agent 暂停、等一个结构化答复」）。
- **模态 Esc = 取消这一个工具**（决策 #6）：`Ask` 返回一个「拒绝/取消」响应，`dispatchTools` 据此合成 `is_error` 的 tool_result，回合继续。这与「无模态 Esc = interrupt 整个回合」靠**有无模态**的上下文区分，不冲突。
- headless 视图的 `Ask` 退回现有 stdin 读法，行为不变。

---

## 7. 键位与 UX（决策 #7/#9/#10）

| 键 | idle（无回合） | 回合运行中（无模态） | 模态打开时 |
|---|---|---|---|
| 裸 **Enter** | 提交，开新回合 | **steer**：注入 pending，下一边界生效（无边界→降级 queue） | 确认当前选项 |
| **Alt+Enter** | （同 Enter） | **queue**：排进 pending，回合结束后作新回合 | — |
| **Esc** | 清空当前输入行 | **interrupt**：停当前回合，pending 保留 | **取消单个工具**，回合继续 |
| **Ctrl-C** | 保存并退出 | interrupt（同 Esc）；连按可退出 | 取消工具 |
| **Ctrl-D** | EOF 退出 | — | — |

修饰键选择：**Alt+Enter**（raw mode 下是 `ESC`+`CR`，bubbletea 可靠检测），不用 Ctrl+Enter / Shift+Enter（终端普遍发与 Enter 相同的码）。

### 常驻队列区（决策 #10）

```
┌─ 对话区（流式输出 + 工具事件 + card）──────────────┐
│ …assistant text…                                   │
│ ↳ terminal: go test ./...                          │
│ …                                                  │
├─ 队列 (2) ─────────────────────────────────────────┤
│  1. [queue] 跑一下 lint                            │
│  2. [steer] 顺便把错误分支也覆盖到                  │  ← Ctrl-X 删选中 / Ctrl-Shift-X 清空
├─ 输入区 ───────────────────────────────────────────┤
│ you> _                                             │
└────────────────────────────────────────────────────┘
```

- 队列区列出所有 pending，标注 `[queue]`/`[steer]`，显示消费顺序。
- 撤回：删最后一条 / 选中删 / 清空（具体键位 tech-design 细化）。
- pending 为**会话内瞬态**，不持久化——丢了可重打（见 §11）。

---

## 8. TTY / 非-TTY 分流与 mswe-eval（决策 #5/#12）

```go
// cmd/octo 选择视图：
switch {
case forceNoTUI:                 // --no-tui / OCTO_TUI=0
    view = newPlainView(...)     // 复用现有纯文本渲染
case stdinIsTTY(os.Stdin):       // lineread.go:121
    view = newTUIView(...)       // bubbletea
default:                         // 管道 / 测试 / CI
    view = newPlainView(...)
}
runTurnCore(turnCore, view)      // 同一 core，不同 sink
```

- **headless 路径必然保留**（mswe-eval、测试用 `strings.Reader`、`octo chat "msg"` 单发、管道）。`--no-tui` 只是让 TTY 也强制走它，兼容性兜底。
- **mswe-eval 迁移**：当前它靠「piped stdin 驱动 REPL」（`cmd/mswe-eval/main.go` 的 `runStdin`，喂 `octoPrompt+"\n"` + EOF），这条路依赖即将被 TUI 取代的输入路径，脆弱。改为一个**明确的 headless 入口契约**：单发一个 prompt、跑带工具的 agentic 循环、纯文本渲染、EOF/完成即退出（具体 flag 形态 tech-design 定，如 `octo chat --headless` 读 stdin 或位置参数）。这正是 `fix/mswe-first-run-hardening` 分支在做的 hardening，一并收掉。

---

## 9. 测试（stdlib + httptest，无外部框架）

- **turn-core**：用一个 mock `ViewSink` 断言事件序列、`Ask` 请求-响应、pending 消费时机；不依赖终端。这是双视图共享逻辑的主战场。
- **pending/注入**：构造一个会产 tool_use 的 mock Sender，断言 steer 文本被并进 tool_result 消息的 blocks（不产生连续两条 user role）；断言无边界时降级为后续回合。
- **interrupt 与 pending 存活**：cancel `turnCtx`，断言当前回合走 `finishInterrupted` 且 pending 仍被后续消费。
- **headless 路径回归**：现有 scanner-based REPL 测试（`strings.Reader` 喂 stdin、断言 stdout 序列）继续绿，保证 `--no-tui` / 非-TTY 行为不变。
- **bubbletea 视图**：用 `teatest`（charmbracelet 官方测试工具）驱动 Model，断言键位映射与模态状态机；不接真实 TTY。
- **gate/asker 请求-响应**：mock ViewSink 验证模态 Esc → 单工具取消 → 回合继续。

---

## 10. 新增依赖

- `github.com/charmbracelet/bubbletea` — TUI 事件循环。
- `github.com/charmbracelet/bubbles`（可选）— 现成的 textinput/viewport 组件。
- `github.com/charmbracelet/x/exp/teatest`（test-only）— Model 测试。
- 已有：`lipgloss`（渲染）、`go-isatty`（TTY 探测）。`chzyer/readline` 在 `--no-tui`/headless 仍保留，或评估能否退场（idle 行编辑由 bubbletea textinput 接管后）。

---

## 11. 任务拆分（单 PR，决策 #11）

虽然单 PR 落地，内部按以下顺序构建，便于自测：

1. **抽 turn-core**：把 `runREPL` 的回合编排与渲染解耦，定义 `ViewSink`；先用一个等价于现有行为的 `plainView` 适配，保证既有测试全绿（零行为变更）。
2. **agent loop 并发化 + 注入点**：`runLoop` 增加 pending drain（steer 并入 tool_result）、回合末降级逻辑；`Agent` 暴露线程安全的 pending 注入。
3. **gate/asker 请求-响应化**：`ViewSink.Ask`；plainView 退回 stdin，行为不变。
4. **bubbletea TTY 视图**：Model/Update/View，对话区 + 队列区 + 输入区 + 模态；键位映射（§7）；`AgentEvent`→`tea.Msg`。
5. **分流与 flag**：`stdinIsTTY` 分流 + `--no-tui`/`OCTO_TUI=0`。
6. **迁 mswe-eval**：headless 入口契约，替换 piped REPL 驱动。
7. **测试补齐**（§9）。

> ⚠️ 单 PR 风险（已知并接受）：diff 大、review 困难、出问题难二分。缓解：上述步骤各自 commit 清晰、turn-core 步骤保持既有测试绿作为「未回归」基线。

---

## 12. 不做 / 未来

- **pending 持久化**：typed-ahead 是瞬态输入，会话退出/崩溃即丢，不写盘。
- **steer 的「立即打断重发」语义**（grill Q2·B）：明确否决——与 interrupt 区分度太低。steer 一律走「下一边界注入」。
- **队列消息的富文本编辑**（grill Q10·C 的回填编辑）：首版只做删除/清空，不做回填到输入区再编辑。
- **Web / IM 端的 queue/steer/interrupt**：本设计只覆盖 CLI TTY；Web/IM 各有自己的输入模型，另案。
- **多回合并行**：始终一次一个在途回合，pending 串行消费。
