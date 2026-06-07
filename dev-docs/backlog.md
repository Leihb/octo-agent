# octo-agent 待办清单

> 本文档沉淀了项目功能调研后的剩余待办项，按优先级分组。每个条目标注了代码位置、当前状态和估计影响面。
> 更新日期：2026-06-07

---

## 已完成的“大件”（上下文）

以下模块均已是 **done** 状态，作为后续工作的基线：

| 模块 | 备注 |
|------|------|
| Core CLI (TUI + headless) | 交互式 TUI、headless one-shot、`--prompt-file` |
| Providers | Anthropic + OpenAI 双协议，兼容第三方 |
| 内置工具集 | terminal、read/write/edit_file、glob、grep、web_fetch/search |
| Agentic Loop | 多轮工具调用、权限 gate、历史压缩、Ctrl-C 优雅中断 |
| MCP Client | stdio + Streamable HTTP、OAuth Device Flow |
| 跨会话记忆 | `~/.octo/memory/`、自动提取/合并、memory nudge |
| Skills | Claude Code 格式 SKILL.md、默认 skills 嵌入 + materialize |
| Sandbox | macOS Seatbelt / Linux Landlock + seccomp |
| Sub-agent (`sub_agent`) | 统一工具（fork / fresh / background）、异步通知 |
| 自主编排 (`octo conduct`) | LEDGER + worktree 隔离 + verifier 闸门 + resume |
| Web Server (`octo serve`) | REST + SSE + WebSocket + 内嵌 dashboard |
| IM Bridge | 微信 iLink、钉钉、飞书 |
| Tool Search (MCP) | `mcp_search` / `mcp_describe` / `mcp_call` 桥 |
| 截断恢复 (Layer 1) | `max_tokens` 时 escalate-and-retry |
| Next Suggestion | TUI ghost text、Web UI 占位 |
| 统一会话引导 | `internal/app.Bootstrap` |

---

## P1 — 高影响、低复杂度

### 1. Skill Toggle 状态持久化

- **位置**：`internal/server/skill_toggle_handler.go`
- **现状**：`PATCH /api/skills/{name}/toggle` 是 no-op stub，返回成功但不持久化
  ```go
  writeJSON(w, http.StatusOK, map[string]any{
      "name":    name,
      "enabled": true,
      "note":    "skill toggle is not yet persisted in the Go rewrite",
  })
  ```
- **影响**：Web UI 里点 toggle 只是临时 UI 状态，刷新后丢失
- **建议**：写入 `~/.octo/skills.yml`（或复用 config.yaml 的 tools 节），server 启动时重读。可独立成一个 PR。

### 2. Web UI Benchmark Panel（后端缺失）

- **位置**：
  - 前端：`internal/server/static/index.html:303`、`sessions.js:3981`
  - 后端：`internal/server/`（无对应 handler）
- **现状**：前端有完整的 benchmark UI（模型切换器里的延迟测试按钮），调用 `POST /api/sessions/{id}/benchmark`，但后端没有实现该 endpoint
- **影响**：Web UI 的 latency 信号栏只能看历史缓存，无法跑新的 benchmark
- **建议**：在 `internal/server/` 里加一个 benchmark handler，跑一个轻量的 provider ping（如发一个空 user message 测 TTFT）。前端代码已就绪，只需补齐后端。

---

## P2 — 中影响、需要一定设计

### 3. 用户自定义 Agent 预设 ✅

- **位置**：`internal/tools/agent_presets.go:62`
- **现状**：`lookupAgentPreset` 只有 4 个硬编码预设（explore / plan / general / code-review），代码里留了 TODO：
  ```go
  // TODO: load user-defined agents from ~/.octo/agents/*.md
  ```
- **已完成**：新增 `internal/tools/agents.go`，扫描 `~/.octo/agents/*.md`，YAML frontmatter 格式（`name` / `description` / `read_only`），正文为 persona。`lookupAgentPreset` 优先用户定义、再回退内置；`listPresetNames` 合并两者去重。

### 4. 微信 CDN 文件上传

- **位置**：`internal/channel/adapters/weixin/weixin.go:411`
- **现状**：用户发文件时，目前只回复一条文本提示：
  ```go
  // TODO: implement CDN file upload. For now fall through to text hint.
  return a.SendText(chatID, fmt.Sprintf("📎 File: %s", name), replyTo)
  ```
- **影响**：IM 桥的文件传输体验不完整，用户收到的是文件名文本而非真实文件
- **建议**：实现微信 CDN 文件上传接口（先获取 media_id，再发文件消息）。这是 iLink 协议已支持的常规能力。

---

## P3 — 低影响 / 未来工作

### 5. TUI 子 Agent 面板交互式展开/折叠

- **位置**：`dev-docs/sub-agent-design.md:201`
- **现状**：TUI 底部有 sub-agent 实时面板，每行内联显示最近几个 tool 名 + 累计计数
- **尚未做**：按某个键（如 Enter）展开某个子 agent 的完整 tool 调用历史（需要焦点管理 + 按键处理）
- **影响**：低——核心信息已显示，展开是锦上添花
- **建议**：如果后续 TUI 做更多“可交互面板”重构时一并处理。

### 6. 截断恢复 Layer 2（resume-and-chunk）

- **位置**：`dev-docs/truncation-recovery.md:105`
- **现状**：Layer 1（escalate-and-retry）已实现——遇到 `max_tokens` 时自动提升 cap 重试一次
- **尚未做**：Layer 2——保留部分输出，喂回 "you were cut off; resume mid-thought and write in smaller pieces" 提示并继续（Claude Code 式多轮恢复）。需要处理 provider 对不完整 `tool_use` block 的兼容性。
- **影响**：低——Layer 1 已覆盖绝大多数大文件写入场景
- **建议**：等有大模型在 escalate 后仍然截断的真实案例时再起工。

### 7. Conductor Phase 2 / Phase 3（渐进交付的后续阶段）

- **位置**：`dev-docs/autonomous-orchestrator-design.md:182`
- **现状**：Conductor 已实现 Phase 1（最小可用单线程），具备 LEDGER、Verifier、Continue 续跑
- **Phase 2 未做**：WorktreeManager + 有界并行 + 串行合并 + 冲突处理（`WorkSpec.Workdir` 字段已预留，但 conductor 主循环目前仍跑在主工作区）
- **Phase 3 未做**：自动重规划 + 停滞检测 + 预算/终止/恢复全套护栏 + 结构化报告
- **影响**：中——Phase 1 已能让大任务“跑得下去、不崩”，Phase 2/3 是性能和鲁棒性的提升
- **建议**：有真实的大型无人值守任务需求（如批量代码迁移）时再推进。

---

## 快速索引

| 待办项 | 优先级 | 代码位置 | 类型 |
|--------|--------|----------|------|
| Skill Toggle 持久化 | P1 | `internal/server/skill_toggle_handler.go` | stub → 实装 |
| Web UI Benchmark API | P1 | `internal/server/`（缺失） | 前端已就绪，补后端 |
| 用户自定义 Agent 预设 | P2 | `internal/tools/agent_presets.go:62` | TODO |
| 微信 CDN 文件上传 | P2 | `internal/channel/adapters/weixin/weixin.go:411` | TODO |
| TUI 子 Agent 面板展开 | P3 | `dev-docs/sub-agent-design.md:201` | 体验优化 |
| 截断恢复 Layer 2 | P3 | `dev-docs/truncation-recovery.md:105` | 未来工作 |
| Conductor Phase 2/3 | P3 | `dev-docs/autonomous-orchestrator-design.md:182` | 渐进交付 |
