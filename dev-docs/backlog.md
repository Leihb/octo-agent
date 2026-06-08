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
| 跨会话记忆 | `~/.octo/memories/`、自动提取/合并、memory nudge |
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

### 1. Skill Toggle 状态持久化 ✅

- **位置**：`internal/server/skill_toggle_handler.go`
- **现状**：`PATCH /api/skills/{name}/toggle` 之前是 no-op stub
- **已完成**：`tools.disabled_skills` 写入 `~/.octo/config.yaml`，`skills.Registry` 用 disabled-name set 过滤 `Get`/`List`/`Len`/`RenderManifest`，server 启动时从 config 重读。`GET /api/skills` 返回 `enabled` 字段。

### 2. Web UI Benchmark API ✅

- **位置**：`internal/server/benchmark_handler.go`
- **现状**：前端有 benchmark UI 但后端缺失对应 handler
- **已完成**：新增 `POST /api/sessions/{id}/benchmark`，streaming TTFT 测量（通过 `StreamingSender` 检测首个 token 到达时间），非 streaming fallback，60s 超时。前端无需改动。

---

## P2 — 中影响、需要一定设计

### 3. 用户自定义 Agent 预设 ✅

- **位置**：`internal/tools/agent_presets.go:62`
- **现状**：`lookupAgentPreset` 只有 4 个硬编码预设（explore / plan / general / code-review），代码里留了 TODO：
  ```go
  // TODO: load user-defined agents from ~/.octo/agents/*.md
  ```
- **已完成**：新增 `internal/tools/agents.go`，扫描 `~/.octo/agents/*.md`，YAML frontmatter 格式（`name` / `description` / `read_only`），正文为 persona。`lookupAgentPreset` 优先用户定义、再回退内置；`listPresetNames` 合并两者去重。

### 4. 微信 CDN 文件上传 ✅

- **位置**：`internal/channel/adapters/weixin/weixin.go:411`
- **现状**：`SendFile` 之前是 TODO stub，只回复文本提示
- **已完成**：
  - 新增 `ilink/crypto.go`：AES-128-ECB + PKCS7 加解密（参考 corespeed-io/wechatbot 实现）
  - 扩展 `ilink/protocol.go`：`GetUploadURL`、`UploadToCDN`（带 3 次重试）、`BuildMediaMessage`、`BuildCDNUploadURL`
  - `weixin.go` 的 `SendFile` 实现完整 CDN 上传流程：生成 AES key → 加密文件 → getuploadurl → POST 到 CDN → 构造 file_item 消息发送
  - 按扩展名自动路由 media type（image/video/file）
  - `CDNBaseURL` 改为 var，方便测试 mock

---

## P3 — 低影响 / 未来工作

### 5. TUI 子 Agent 面板交互式展开/折叠 ✅

- **位置**：`dev-docs/sub-agent-design.md:201`
- **现状**：TUI 底部有 sub-agent 实时面板，每行内联显示最近几个 tool 名 + 累计计数
- **已完成**：
  - `subAgentUI` 新增 `history []string` 记录完整 tool 调用链（`recent` 仍保留用于折叠时的内联预览）
  - `subAgentFocus int` 焦点管理：↑/↓ 在子 agent 间导航，Enter 展开/折叠，Esc 返回输入框
  - 进入焦点模式：输入框为空、光标在第一行、按 ↑ 且 history 已到顶时自动进入
  - 展开时渲染完整 history（每行一个 tool，缩进显示），`liveHeight` 动态计算展开后的高度
  - 面板标题在焦点模式下显示快捷键提示 `[↑/↓ nav · Enter expand · Esc back]`
  - `removeSubAgent` 自动调整焦点索引，避免焦点悬空

### 6. 截断恢复 Layer 2（resume-and-chunk）✅

- **位置**：`dev-docs/truncation-recovery.md:105`
- **现状**：Layer 1（escalate-and-retry）已实现——遇到 `max_tokens` 时自动提升 cap 重试一次
- **已完成**：Layer 2 实装——当 escalate 后仍然截断时，将 partial text 追加到 history，注入 recovery user message，继续 loop 让模型完成剩余内容。限制最多 3 次恢复，防止无限循环。跳过了截断的 tool_use（OpenAI 协议中 partial tool_use 在 history 中不安全）。引入 `escalateExhausted` 标志避免 resume 后重复 escalate。

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
