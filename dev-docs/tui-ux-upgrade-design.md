# TUI UX 升级（design）— 已全部实施，最终为 inline 模式

> 把交互式 bubbletea TUI 升级到对标 Claude Code 的体验：富工具卡片、bottom bar、spinner、
> glamour markdown、面板化 + 统一主题。最终采用 **inline 模式**（无 alt-screen），与
> Claude Code 对外部用户的默认行为一致——Claude Code 只在 Anthropic 内部用 alt-screen。
> 基于 bubbletea / lipgloss / chroma 栈实现。

---

## 1. 背景与最终架构

### 演进历史

| 阶段 | PR | 说明 |
|---|---|---|
| 初始 | #149 | 交互式 bubbletea REPL，inline + `tea.Println` |
| UX 升级 | P1–P5 | 富卡片、spinner、glamour、面板、主题 |
| 尝试 alt-screen | — | 加入 `tea.WithAltScreen()` + 内部 scrollback + stickyScroll + 鼠标滚动 |
| **回退 inline** | #245 | alt-screen 问题太多（鼠标不通用、选文本要 Shift、复制粘贴坏），参考 Claude Code 外部用户默认 inline，切回 |

### 最终架构：inline 模式

```
  ┌─ 终端原生 scrollback（tea.Println 输出，终端管理滚动/选文本/复制）──┐
  │  > hello                                                         │
  │  好的，我来帮你…                                                    │
  │  ● Update(path.go)  (+3 -1)  ← diff 卡片                          │
  │  ● Run(go test ./...)  ← 输出预览卡片                              │
  │  完成。                                                            │
  ├─ View() — 钉在底部，不随内容滚动 ──────────────────────────────────┤
  │  ⠹ Thinking… (2.3s · ↑ ~1.2k tokens)  ← live 区                    │
  │  ┌ queue (2) ──────────────┐          ← 边框面板                   │
  │  │ 1. add error handling    │                                      │
  │  │ 2. write tests           │                                      │
  │  └──────────────────────────┘                                      │
  │  > █                                    ← flat 输入行              │
  │  ───────────────────────────────────────                            │
  │  cwd: ~/proj · ctx: 42% · perm: interactive · elapsed: 5s          │
  │  Enter inbox · Alt+Enter queue · Esc interrupt                     │
  └────────────────────────────────────────────────────────────────────┘
```

**核心原则：**

- Banner 在 `Init()` 中通过 `tea.Println` 打印一次到终端顶部
- 所有已提交内容（助手文本、工具卡片、通知）通过 `println()` → `flushPrints()` → `tea.Println` 输出到终端原生 scrollback
- `View()` 只渲染底部 live 区：partial 文本、spinner、面板、输入行、状态栏
- 终端原生处理：鼠标滚动、文本选择、复制粘贴——不需要 bubbletea 参与
- 不需要：`scrollback` 缓冲区、`scrollOffset`、`stickyScroll`、`handleMouse`、键盘滚动

---

## 2. 数据流

### println / flushPrints 模式

```go
// printlnBuf 暂存待输出行
printlnBuf []string

// println 队列一行到终端
func (m *tuiModel) println(line string) {
    m.printlnBuf = append(m.printlnBuf, line)
}

// flushPrints 通过 tea.Println 输出所有排队行
func (m *tuiModel) flushPrints() tea.Cmd { ... }

// Update() 中每个 return 点都调用 flushPrints
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    // ... 处理各种消息，调用 m.println() ...
    return m, m.flushPrints()  // 每个 return 点
}
```

### 事件 → 输出映射

| AgentEvent | 处理 |
|---|---|
| `EventThinkingDelta` | 不进 scrollback；只累积 `turnOutChars`，喂给 live 区 `thinkingLine` 的「↑ ~N tokens」读数 |
| `EventTextDelta` | 累积到 `m.partial`；block 边界经 glamour 渲染后 `m.printlnBlock()` |
| `EventToolStarted` | 所有工具：`m.running` 设为 live spinner（不提交 started 行）；`--plain` 保留 `↳ tool: input` 行 |
| `EventToolProgress` | 忽略（live spinner 已示活动）；`--plain` 保留 `│ chunk` 行 |
| `EventToolDone/Error` | 清除 `m.running`；卡片：`renderToolCard()`；非卡片：`tui.RenderToolStatus` 单行 `● tool(input)`；`--plain` 保留 `↳ tool ✓/✗` |

### 降噪规则（对标 Claude Code）

- **思考过程不落盘**：reasoning trace 不进 scrollback，等待期的活动行显示
  `⠹ Thinking… (12s · ↑ ~3.2k tokens)`（token 数 = 本回合输出字符 / 4 估算）。
- **块间空行**：所有 block 级内容（用户 echo、markdown block、卡片、通知）经
  `printlnBlock()` 提交，块与块之间恰好一个空行；`--plain` 保持紧凑行布局。
- **卡片行数上限**：输出卡 `outputCardMaxLines = 4`；diff 卡每侧（删除/新增）
  `diffCardMaxRows = 6`；折叠标记统一为 `… +N lines`。
- **cache 行仅 verbose**：`ⓘ cache: …` 回合页脚只在 `--verbose` 下输出，默认
  靠状态栏 ctx% 感知。
- **后台 shell 单行化**：不再渲染逐命令的边框面板；空闲时一行
  `⠹ 26s · 1 shell still running`（最老 shell 的运行时长），turn 进行中只靠
  状态栏的 `N shell(s)` 计数段（accent 色）。启动/退出事件本来就落在 transcript。

---

## 3. 已实施特性

| 特性 | 状态 | 文件 |
|---|---|---|
| 富工具卡片（diff + 输出预览） | ✅ | `toolcards.go`, `internal/tui/diffcard.go`, `internal/tui/outputcard.go` |
| Bottom bar（cwd/ctx/perm/elapsed） | ✅ | `tuirepl_view.go:renderStatusBar`, `internal/tui/theme.go:StatusBar` |
| 富 spinner（Braille + 轮转提示词） | ✅ | `tuirepl_view.go:spinnerLine/thinkingPhrase`, `spinner.go` |
| glamour markdown（流式 block 边界提交） | ✅ | `markdown.go` |
| 面板化（队列/sub-agent/模态；后台 shell 已改单行） | ✅ | `internal/tui/theme.go:Panel/Box` |
| 统一自适应主题 | ✅ | `internal/tui/theme.go` |
| 章鱼像素-art 横幅 | ✅ | `internal/tui/theme.go:Banner`（Init 时打印一次） |
| 卡片归属 TUI-only（plainView 纯文本） | ✅ | `toolcards.go`, `repl.go` |

### 决策记录

| # | 决策 | 选择 |
|---|---|---|
| 1 | 布局 | **inline**（`tea.NewProgram(m)`，无 alt-screen，无鼠标追踪）|
| 2 | markdown | glamour |
| 3 | 卡片 | `internal/tui`，TUI-only |
| 8 | headless | `plainView` 纯文本 `↳ tool ✓/✗` |

---

## 4. 新增依赖

- `github.com/charmbracelet/glamour`
- 已有：`bubbletea` / `lipgloss` / `chroma`

---

## 5. 非目标

- alt-screen / 内部 scrollback（已移除，inline 模式更可靠）
- Web / IM 端富渲染（各端自有渲染模型）
- 内联图形 / 图片
- octo-eval / headless 路径（纯文本不变）
