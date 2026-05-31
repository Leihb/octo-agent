# TUI UX 升级（design）— 已全部实施

> 把交互式 bubbletea TUI 从「比 plain 路径还朴素」升级到对标 Claude Code（Ink）的体验：
> 富工具卡片、底部状态栏、富 spinner、glamour markdown、面板化 + 统一主题。
> 最终采用 **alt-screen + 内部 scrollback viewport**（实测 inline 模式的 `tea.Println` 与 live 区 `View()` 更新存在竞态和视觉抖动，故改为 alt-screen）；退出时 dump scrollback 回主屏幕保留历史。
> 基于 octo 现有的 bubbletea / lipgloss / chroma 栈实现。

---

## 1. 背景与目标

### 现状（已实施完毕）

原始 TUI（#149，`cmd/octo/tuirepl.go` + `tuirepl_view.go`）已从最初的 inline/`tea.Println` 模式演进为 **alt-screen + 内部 scrollback** 架构——`tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion())`。所有输出（助手文本、工具卡片、通知）累积在 `m.scrollback` 中，在 `View()` 内整体渲染；退出时 dump 回主屏幕。

一个**已存在但最初未被 TUI 复用的渲染器**是 `internal/tui/diffcard.go` 的 `RenderEditCard`（true-color、行号、±标记、真·chroma 高亮——`highlightLine` 用 `terminal16m` formatter + `github-dark` 主题）。它原本只接在 `plainView`（非-TTY / `--no-tui` 回退视图）的 `replToolEventHandler` 上，现已按决策 #8/#9 **下沉到 TUI**——`plainView` 的 `replToolEventHandler` 已移除所有卡片分支（`_ = plain`），所有 headless 输出退回纯文本 `↳ tool ✓/✗` 一行。

当前 TUI 渲染卡片的工具：`edit_file`（diff 卡片）、`terminal`、`grep`、`web_search`、`glob`、`read_file`、`web_fetch`（输出预览卡片）。未列入的工具仍走 `↳ tool ✓/✗` 一行。

| 维度 | TUI（`tuirepl.go`） | 非-TTY 回退视图（`plainView`） |
|---|---|---|
| edit_file | **chroma 高亮 diff 卡片**（`RenderEditCard`） | `↳ tool ✓/✗` 一行 |
| terminal / grep / read_file / glob / web_search / web_fetch | **输出预览卡片**（`RenderOutputCard`） | `↳ tool ✓/✗` 一行 |
| 其他工具 | `↳ tool ✓/✗` 一行 | `↳ tool ✓/✗` 一行 |
| 助手文本 | **glamour markdown** 渲染 | 原始 text delta |
| 输入 / 状态 | flat `> ` 输入行 + status bar（cwd/ctx%/perm/elapsed） | n/a（非交互） |

所有卡片统一为 **TUI-only**。`--plain` 在 TUI 中关闭卡片渲染（`tuirepl.rendersCard` 返回 false），退回一行状态。

### 目标（已实施）

对标 CC 的 Ink TUI 体验，已在 bubbletea 里实现：

1. **富工具卡片**：diff 卡片（edit_file）、输出预览卡片（terminal/grep/read_file/web_search/glob/web_fetch），带 spinner→●/●err 的状态头。
2. **底部状态栏**：cwd · context% · 权限模式 · 计时 + 上下文键位提示。
3. **富 spinner + 活动指示器**：Braille spinner ⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏（~8 Hz ticker），带计时和轮转提示词。
4. **glamour markdown**：助手文本的标题/粗体/列表/引用/代码块渲染，流式按 block 边界提交。
5. **面板化 + 统一主题**：队列/后台做成圆角边框面板；自适应（亮/暗）主题色板；章鱼像素-art 横幅。

### 非目标（见 §11）

Web/IM 端渲染；内联图形 / 图片；mswe-eval 等 headless 路径（纯文本不变）。

---

## 2. 决策记录（grill 已定）

| # | 决策 | 选择 | 理由 |
|---|---|---|---|
| 1 | 布局架构 | **alt-screen + 内部 scrollback viewport**（最终选择） | 最初设计为 inline + `tea.Println`，实测 inline 的 `tea.Println` 与 live 区 `View()` 在同一个帧循环内竞争终端输出位置，产生视觉抖动和竞态。alt-screen 将所有渲染统一在 `View()` 内：scrollback 内容缓存于 `m.scrollback` 切片，一起渲染；退出时 `fmt.Println` dump 回主屏幕保留对话历史。鼠标滚轮 scrollback 也自然在此架构内实现 |
| 2 | markdown | **glamour 全量渲染** | 最接近 CC；新增依赖 `charmbracelet/glamour`（其内部也用 chroma，与现有依赖同源） |
| 3 | 工具卡片 | 复用并扩展 `internal/tui` | 已有 `RenderEditCard`；新增 output-preview 卡片 + 状态头卡片。TUI 接进来，先抹平与 plain 的差距再超越 |
| 4 | 运行中 vs 已完成的卡片 | 运行中（spinner）放 live 区；`EventToolDone/Error` 时定版成卡片 `pushScrollback` 推入内部 scrollback buffer | alt-screen 下所有渲染统一在 `View()` 内：live 区动态状态与 scrollback 历史在同一帧渲染，终态卡片落 scrollback 后不再重绘 |
| 5 | markdown 流式 | 流式期间 partial 缓冲；到 **block 边界**（空行外 / 闭合 ```）才把该 block 经 glamour 渲染后 `pushScrollback` | glamour 要完整 block 才渲染对；逐 delta 渲染会抖。块级提交兼顾流式手感与正确渲染。滚动中未完成 partial 实时渲染在 live 区 |
| 6 | 主题 | 统一 `internal/tui` theme 包，`lipgloss.AdaptiveColor` 亮/暗自适应，收拢 `tuirepl_view.go` + `diffcard.go` 散落 style | 一处定义、两处（TUI 卡片）共享 |
| 7 | 落地策略 | **分期多 PR**（P1–P5，§8），每期独立可发 | UX 改动面大；分期便于 review、回归、按你反馈调方向。区别于 tui-input-modes 的单大 PR |
| 8 | 卡片归属 | **卡片是 TUI-only**；`plainView`（非-TTY / `--no-tui`）一律纯文本，**已移除接在 plainView 上的 edit_file 卡片**（下沉到 TUI 层） | 卡片是 ANSI true-color 框，在管道 / CI / mswe / 重定向输出里是噪声；交互式 TTY 才是它的归宿 |
| 9 | headless 行为 | `plainView` 的工具事件退回 `↳ tool ✓/✗` 一行（含 edit_file）；`--no-tui` / 非-TTY / mswe 不变地走纯文本 | `replToolEventHandler` 已移除所有 `rendersAsCard`/`renderToolCard` 分支（`_ = plain`）；headless 输出干净可解析 |

---

## 3. 架构（实际实施）

**alt-screen + 内部 scrollback viewport**。`View()` 渲染完整屏幕：

- 顶部：章鱼像素-art 横幅（模型名称 + cwd）
- 中间：scrollback 历史（从 `m.scrollback` 渲染，含用户消息、助手 markdown、工具卡片、通知），支持鼠标滚轮偏移（`m.scrollOffset`）
- 过渡：live partial 助手文本（未完成 block 实时 glamour 渲染）、活动指示器（spinner 或正在进行的卡片工具）
- 面板区（瞬态）：队列面板、后台进程面板
- 底部：flat 输入行（`> ` + `textinput.Model`）+ 状态栏

```
  ┌─ alt-screen（View() 完整渲染）──────────────────────────────┐
  │      ████████                                              │
  │    ████░░░░████  ◆ octo chat  v1.2.3                      │
  │    ████████████  claude-sonnet-4-20250514 · ~/proj         │
  │  ████████████████                                          │
  │  ████  ████  ████                                          │
  │  ██    ████    ██                                          │
  │        ████                                                │
  │  ─────────────────────────────────────────                  │
  │  > hello                                                  │  ← scrollback 历史
  │  ● Update(internal/cache/bk.go)  (+3 -1)                  │
  │   4  ─ func Get(k string) (V, bool) {                     │
  │   5  +  if m == nil { return zero, false }                │
  │  ● Run(go test ./...)                                     │
  │   │ ok  internal/cache  0.423s                             │
  │   └ 1 more line                                            │
  │  好的，缓存层已加上，**关键点**：…                         │  ← 通过 glamour 渲染
  │  ┌ queue (2) ───────────────────────────┐                 │  ← 边框面板
  │  │ 1. add error handling                 │                 │
  │  │ 2. write tests                        │                 │
  │  └───────────────────────────────────────┘                 │
  │  ⠧ terminal: go build ./...  (3.2s)                       │  ← activity indicator
  │  > continue refactoring █                                  │  ← flat input line
  │  ─────────────────────────────────────────                  │
  │  cwd: ~/proj · ctx: 42% · perm: interactive · elapsed: 5s  │  ← status bar
  │  Enter steer · Alt+Enter queue · Esc interrupt             │  ← key hints
  └────────────────────────────────────────────────────────────┘
```

关键约束：`View()` 高度 = Banner(8行) + scrollback(剩余空间) + live 区(partial/spinner) + 面板 + 输入行(1) + 状态栏(2-3)。`liveHeight()` 预计算底部固定区域高度，剩余空间分配给 scrollback。退出时 `fmt.Println(strings.Join(m.scrollback, "\n"))` 将对话历史 dump 回主屏幕。

事件 → 渲染映射（`handleEvent`，`tuirepl.go:425-490`）：

| AgentEvent | 处理 |
|---|---|
| `EventTextDelta` | 累积到 `m.partial`；block 边界经 glamour 渲染后 `pushScrollback`；未完成 block 在 live 区实时渲染 |
| `EventToolStarted` | 卡片工具：`m.running` 设为 live spinner 指示器；非卡片工具：`commitToolLine("↳ tool: input")` |
| `EventToolProgress` | 卡片工具：忽略（所有输出延迟到 done 卡片）；非卡片工具：`commitToolLine("│ chunk")` |
| `EventToolDone/Error` | 清除 `m.running`；卡片工具：`renderToolCard()` → `commitToolLine()`；非卡片工具：`↳ tool ✓/✗` |
| `EventSteerInjected` | steered 文本回显到 scrollback，加入 `inputHistory` |

---

## 4. 富工具卡片 ✅

已实现的卡片工具（`toolcards.go:13-29`，`cardVerbFor` 注册）：`edit_file`、`terminal`、`grep`、`web_search`、`glob`、`read_file`、`web_fetch`。未列入的工具走 `↳ tool ✓/✗` 一行。

- **diff 卡片**：`tui.RenderEditCard(path, old, new)`（`diffcard.go:68`）——Chroma 语法高亮、行号、`+`/`-` 标记、`+` 行绿色背景 / `-` 行红色背景（亮/暗自适应），text 行灰色。
- **输出预览卡片**：`tui.RenderOutputCard(verb, target, output, maxLines, isErr)`（`outputcard.go:22`）——`● Verb(target)` 头部，最多 12 行预览 + `└ N more lines` 折叠标记。错误时 bullet 变红。
- **运行中卡片**（live 区）：`EventToolStarted` 将 `m.running` 设为 `{verb, target, start}`；`View()` 渲染为带 Braille spinner 的 `⠹ Verb(target) (3.2s)`。`EventToolDone/Error` 时清除 `m.running`，定版的终态卡片推入 `m.scrollback`。卡片工具抑制 started/progress 行以避免完成卡片上方出现冗余文本。

卡片渲染函数留在 `internal/tui`，**仅由 TUI（`tuirepl.handleEvent`）调用**。`plainView` 的 `replToolEventHandler` 已移除所有卡片分支（`repl.go:577` 的 `_ = plain`）。

---

## 5. 状态栏 + 输入行 ✅

都在 `View()` 内渲染（`tuirepl_view.go:496-530`）：

- **输入行**（`renderInputBox`）：flat 样式 `> ` + `textinput.Model`，无边框包装。Claude Code 风格——prompt 前缀直接接输入光标。
- **状态栏**（`renderStatusBar`）+ `tui.StatusBar`（`theme.go:130-157`）：
  - `cwd`：缩写 home（`~/…`）。
  - `ctx`：当前 input tokens / 模型上下文窗口百分比（`ContextUsage()`）。
  - `perm`：权限模式（interactive / strict / auto-approve），Shift+Tab 切换。
  - `elapsed`：当前回合耗时（仅在 `turnRunning` 时显示）。
  - Model 名称展示在顶部横幅中（非状态栏）。
  - 键位提示在第二行（回合运行时）："Enter steer · Alt+Enter queue · Esc interrupt"，队列非空时追 " · Ctrl+X unqueue"。

---

## 6. 富 spinner + 活动指示器 ✅

- `tea.Tick`（`tickInterval = 120ms`，~8 Hz）驱动 spinner 帧（`spinnerFrames` 10 帧 Braille `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`）+ `m.turnStart` 起计的回合已用时间。
- thinking 态：`⠹ Thinking (3.2s)` + 轮转提示词（"Thinking" / "Pondering" / "Working" / "Reasoning"，每 16 ticks ≈ 2s 切换——`tuirepl_view.go:479-484`）。
- 工具运行中：`⠹ Update(path.go) (1.5s)`，复用同一 ticker（`m.running != nil` 时渲染）。
- ticker 持续运行只要有 turn 在跑或有后台进程在活动（`tuirepl.go:330-338`），回合之间自动停止以节省 CPU。

---

## 7. glamour markdown ✅

- `charmbracelet/glamour`，固定深/浅主题的 `glamour.TermRenderer`（`markdownRenderer.render`，`markdown.go:21-45`），宽度 = `m.width`。
- 流式（`splitCommittableMarkdown`，`markdown.go:53-75`）：`EventTextDelta` 累积到 `m.partial`；扫描最近的 `\n\n`（空行）在 code fence 之外形成的 block 边界；达到边界时：前缀经 glamour 渲染 → `pushScrollback`；尾部保留在 `m.partial` 中继续接收增量。非边界时没有输出被提交。因此 glamour 始终只接收完整 block。
- 代码块：glamour 内部使用 chroma 高亮（与 diff 卡片同源主题），视觉效果统一。
- 未完成 block 在 live 区通过 glamour 实时渲染（`View()` 中 `m.md.render(p, m.width)`），流式感知良好。

---

## 8. 分期（均已实施）

| 期 | 内容 | 状态 |
|---|---|---|
| **P1** | `internal/tui` theme 包雏形 + 工具富卡片接进 TUI（§4）：edit_file 卡片**从 plainView 下沉到 TUI**（plainView 退回纯文本）+ 新建 terminal/grep/read_file/web_search/glob/web_fetch 输出预览卡片 | ✅ |
| **P2** | 状态栏 + 输入行（§5） | ✅ |
| **P3** | 富 spinner + 活动指示器（§6） | ✅ |
| **P4** | glamour markdown（§7） | ✅ |
| **P5** | 面板化（队列/后台/模态边框）+ 主题收口 + 章鱼横幅 + 鼠标滚轮 scrollback | ✅ |

---

## 9. 测试

- `teatest`（charmbracelet 官方）驱动 Model：键位、modal 状态机、`View()` 快照（状态栏/输入框/面板）。
- 卡片渲染：复用 `cmd/tui-preview`（已有，dev-only 肉眼校验）+ 对 `internal/tui` 渲染函数加字符串断言（含/不含语法高亮、行号、cap 折叠）。
- markdown block 边界：断言半截 block 不提交、完整 block 经 glamour 提交、回合末 flush。
- 回归：现有 `tuirepl_test.go` / `repl_test.go` 全绿。P1 改 `plainView` 的 edit_file 渲染（卡片→一行），更新对应断言；其余 headless 行为不变。

---

## 10. 新增依赖

- `github.com/charmbracelet/glamour`（markdown 渲染；内部依赖 chroma，与现有同源）。
- 已有：`bubbletea` / `lipgloss` / `chroma`（diff 卡片 + glamour 共用）。

---

## 11. 已实现（先前列为 "未来"）

- **alt-screen / 内部 scrollback viewport**：`tea.WithAltScreen()`，`m.scrollback` 缓冲区承载所有已提交内容，退出时 dump 回主屏幕。`m.scrollOffset` + 鼠标滚轮支持向上滚动历史（`wheelScrollLines = 4`）。
- **鼠标交互**：鼠标滚轮已支持（`handleMouse`，`tuirepl_view.go:36-51`），click/drag 暂不支持。
- **章鱼像素-art 横幅**：`tui.Banner()`（`theme.go:79-116`），8×7 像素章鱼 + 版本/模型/cwd 信息。

### 不做

- **Web / IM 端富渲染**：各端自有渲染模型，另案（`AgentEvent` 已带 JSON tag，便于 Web/SSE 复用同一事件流）。
- **内联图形 / 图片**。
- **mswe-eval / headless** 始终纯文本。
