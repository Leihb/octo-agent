# TUI 输入模式 — Inbox / Queue / Interrupt

> 交互式 REPL 在回合运行时也能输入:排队(queue)、中途引导(inbox)、打断(interrupt)。
> 交互式 TTY 走 bubbletea 事件循环,非-TTY 与 `--no-tui` 走纯文本路径。

## 1. 目标

REPL 主循环不再是「读一行 → 阻塞跑完回合 → 再读一行」。回合运行时:

- **Inbox**(裸 Enter):键入的话进入 inbox,在**下次迭代开始时**（`think()` 之前）注入 history,
  引导 agent 改方向而不打断它。
- **Queue**(Alt+Enter):键入的话排队,当前回合完整结束后作为新回合自动执行。
- **Interrupt**(Esc,无模态时):停掉当前在途回合;队列保留。

## 2. 架构

bubbletea 的事件循环占主 goroutine，agent loop 移到后台 goroutine；两者经 `tea.Msg`
(事件出)和 channel(请求-响应)通信。渲染与编排解耦，TTY 与纯文本两视图共享一个 turn-core。
**TUI 使用 inline 模式**（无 alt-screen，`tea.NewProgram(m)`）：已提交内容通过 `tea.Println`
输出到终端原生 scrollback，`View()` 只渲染底部 live 区（spinner、面板、输入行、状态栏）。
终端原生处理鼠标滚动、文本选择和复制粘贴。

```
                    ┌─────────────────────────────────────────┐
                    │            cmd/octo (CLI 层)              │
   stdinIsTTY()?    │   ┌──────────────┐    ┌───────────────┐  │
   ───────┬─────────┼──▶│  TTY 视图     │    │ headless 视图  │◀─┼── 非-TTY / --no-tui
          │         │   │ (bubbletea,  │    │ (纯文本/scanner)│  │
          │         │   │  inline 模式) │    │                │  │
          │         │   └──────┬───────┘    └───────┬───────┘  │
          │         │          │  实现 ViewSink      │          │
          │         │          └─────────┬──────────┘          │
          │         │              ┌─────▼──────┐              │
          │         │              │  turn-core  │ (无渲染依赖)  │
          │         │              └─────┬──────┘              │
          └─────────┴────────────────────┼──────────────────────┘
                                          │ AgentEvent / 请求-响应
                          ┌───────────────▼────────────────┐
                          │ internal/agent (后台 goroutine)  │
                          │  RunStream → runLoop(注入点在此) │
                          └────────────────────────────────┘
```

三个关键件:

1. **turn-core**(`cmd/octo/turncore.go`):`runREPL` 里与渲染无关的回合编排(memory nudge
   + live-delta、pre/post hook、auto-save、turnCtx 生命周期)抽进 `runTurn`,对外只认
   `ViewSink` 接口。TTY 视图(`tuiSink`)和纯文本视图(`plainView`)都驱动它。
2. **agent loop 在后台 goroutine**:`tuiSink` 把 `AgentEvent` 包成 `tea.Msg` 投给 Model,
   并经 channel 接收 inbox/queue/interrupt 信号。
3. **inbox 缓冲 + 注入点**:用户输入经 `Agent.Inbox.Enqueue`,`runLoop` 在**每次迭代开始时**
   drain 并追加为独立 user message;回合自然结束时若 inbox 仍有内容则作为下一回合启动
   (inbox 降级)。

## 3. turn-core / ViewSink

```go
type ViewSink interface {
    userPrompter // Ask(ctx, UserPrompt) (UserResponse, error)
    TurnStarted()
    Emit(ev agent.AgentEvent)               // 流式事件出口
    TurnEnded(reply agent.Reply, err error) // 渲染 cache/^C/error
    Notice(msg string)                       // 带外消息(hook 失败等)
}
```

- **`plainView`**:`Emit` 复用纯文本的工具事件渲染;`Ask` 从 stdin 读(行为同重构前)。backs
  非-TTY 路径与 `--no-tui`。
- **`tuiSink`**:`Emit` → `tea.Msg`;`Ask` → 发模态请求、阻塞等 Model 回填。

`runTurn` 把回合编排集中在一处:memory nudge + 跨会话 live-delta、pre/post hook,然后
`a.RunStream(ctx, …, sink.Emit)`,返回的 reply/err 交 `sink.TurnEnded`。caller 仍管输入读取、
slash 命令、turnCtx、save/loop 决策。

## 4. inbox 注入与降级

`runLoop` 在**每次迭代开始时**（`think()` 之前）排空 inbox，把消息作为**独立 user message**
追加到 history：

```go
for i := 0; limit == unlimitedTurns || i < limit; i++ {
    // Drain inbox messages that arrived since the last iteration.
    for _, m := range a.Inbox.Drain() {
        a.History.Append(NewUserMessage(m))
    }

    reply, err := send(ctx, a.History.Snapshot())
    // ...
}
```

消息序列（多轮工具调用后用户输入）：
```
... assistant(tool_use) → user(tool_result) → assistant(tool_use) → user(tool_result)
→ user(inbox) → assistant(reply)
```

对于要求严格 user/assistant 交替的 provider，适配器层（`provider/anthropic`）负责合并连续的
user 消息（将 tool_result 与 inbox 文本合并为一个 user message）。

`Agent.Inbox.Enqueue` 从 UI goroutine 写、`runLoop` 在自己 goroutine drain，无数据竞争
（`Inbox` 内部互斥锁保护）。

**降级**:回合走到终止(模型不再 tool_use)时若 inbox 仍有内容,turn-core 在回合结束后把
它作为下一回合启动——这就是 inbox 的统一兜底。

## 5. interrupt

每回合独立 `turnCtx`。Esc(无模态)取消它,`runLoop` 在迭代边界检测 `ctx.Err()` 走
`finishInterrupted` 把 history 收尾成 well-formed。**Esc 只取消 turnCtx,不动 queue/inbox**——
pending 存活,turn-core 收尾后照常消费为后续回合。

## 6. permission gate / ask_user_question

bubbletea 接管 stdin、agent 又在后台 goroutine,所以审批/提问改成跨 goroutine 的请求-响应,
经 `ViewSink.Ask`(gate 与 asker 共用):

```
agent goroutine                     bubbletea 主 goroutine
gate.Ask(req) ── 发 UserPrompt ──▶  Update: 进模态,View 渲染选项
     (阻塞在 respCh)                 用户选择 / Esc → 写 respCh
     ◀── 解除阻塞,继续 dispatch
```

`UserPrompt`/`UserResponse`/`userPrompter` 在 `cmd/octo/prompt.go`。**模态 Esc = 取消这一个
工具**(`Ask` 返回取消,`dispatchTools` 合成 is_error tool_result,回合继续),与「无模态
Esc = interrupt 整回合」靠有无模态区分。纯文本视图的 `Ask` 退回 stdin 读法。

问答模态首版:选中 "Other(自由文本)" 视作取消(模型可重问或自取默认);纯文本路径仍支持
Other 自由文本输入。

## 7. 键位

| 键 | idle(无回合) | 回合运行中(无模态) | 模态打开时 |
|---|---|---|---|
| 裸 **Enter** | 提交,开新回合 | **inbox**:进入 inbox,下次迭代生效 | 确认当前选项 |
| **Alt+Enter** | (同 Enter) | **queue**:排队,回合结束后作新回合 | — |
| **Ctrl+X** | — | **撤回**最近排队项(连按清空) | — |
| **Esc** | 清空当前输入行 | **interrupt**:停当前回合,pending 保留 | **取消单个工具**,回合继续 |
| **Ctrl+C** | 保存并退出 | interrupt | 取消工具 |
| **Ctrl+D** | 退出 | — | — |

`Alt+Enter` 在 raw mode 下是 `ESC`+`CR`、bubbletea 可靠检测;不用 Ctrl+Enter / Shift+Enter
(终端普遍发与 Enter 相同的码)。`Ctrl+X` 是队列撤回安全阀——因为 Esc 只停回合、不清队列,
排错的 queue 靠它取消。

常驻队列区列出所有 pending、显示消费顺序;状态行在队列非空时提示 `Ctrl+X unqueue`。pending
为会话内瞬态,不持久化。

## 8. TTY / 非-TTY 分流

```go
useTUI := isREPL && stdinIsTTY(stdin) && !*noTUI && !tuiDisabledByEnv() && seedPrompt == ""
```

为真 → `runTUI`(bubbletea);否则 → `runREPL`(scanner + `plainView`)。`--no-tui` /
`OCTO_TUI=0` 强制纯文本(dumb terminal / SSH / tmux / 屏幕阅读器兜底);`--prompt-file`
(`seedPrompt`)的非交互单发也走纯文本。

**非-TTY 必然走纯文本**:管道 stdin 的 `stdinIsTTY` 恒 false,所以 `octo-eval`(piped-REPL
驱动)、测试(`strings.Reader`)、CI 都走 `runREPL`,bubbletea 永不激活——piped-REPL 契约
天然保留,无需专用 headless 入口。

## 9. 依赖

- `github.com/charmbracelet/bubbletea` — 事件循环。
- `github.com/charmbracelet/bubbles/textarea` — 多行输入框(光标移动、词跳转、剪贴板等)。
- 已有:`lipgloss`(渲染)、`go-isatty`(TTY 探测)、`chzyer/readline`(纯文本路径的 idle 行编辑)。

↑/↓ 用于输入历史 recall,因此禁用 textarea 内置的行导航(`LineNext`/`LinePrevious` 设为空
KeyBinding)。Model 逻辑经 `Update` 直接单测,**不依赖** `teatest`。

## 10. 测试(stdlib,无外部框架)

- **turn-core**:mock `ViewSink` 断言事件序列、`Ask` 请求-响应、inbox 消费时机。
- **inbox/注入**:mock Sender 产 tool_use，断言 inbox 作为独立 user message 追加在
  迭代开始时、无边界时降级；两个 provider 适配器的 wire 形态。
- **interrupt**:cancel `turnCtx`,断言走 `finishInterrupted` 且 inbox 存活、后续被消费。
- **TUI Model**:经 `Update` 断言键位映射(Enter=inbox/Alt+Enter=queue/Ctrl+X=unqueue/Esc)、
  queue 入队与出队、降级、模态状态机、文本缓冲——无需真 TTY。
- **纯文本路径回归**:既有 scanner-based REPL 测试继续绿,保证 `--no-tui` / 非-TTY 不变。
