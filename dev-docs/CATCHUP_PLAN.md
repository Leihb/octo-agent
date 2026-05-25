# Octo 追赶 Claude Code 计划

> 与 Claude Code（`/Users/roy.lei/Projects/github/claude-code/src`）功能对账后的差距清单与落地路线。
> 不追求 1:1 复刻——只补战略性缺口，保住 octo 的差异化（三协议 / 三界面 / Skill-first）。
> 每完成一项更新本文档对应行的状态。

## 状态总览

| 编号 | 项目 | Issue | 优先级 | 状态 | 估时 |
|------|------|-------|--------|------|------|
| P0-1 | LLM 可调度的 `Task`/`Agent` 子代理工具 | [#2](https://github.com/Leihb/octo/issues/2) | P0 | ✅ 2026-05-25 | 2d |
| P0-2 | 通用 MCP 客户端（stdio + SSE） | [#3](https://github.com/Leihb/octo/issues/3) | P0 | ⬜ todo | 5d |
| P0-3 | `config.yml` 用户可配置 hook | [#4](https://github.com/Leihb/octo/issues/4) | P0 | ✅ 2026-05-25 | 3d |
| P0-4 | `max_turns` / `max_cost_usd` 主循环闸门 | [#5](https://github.com/Leihb/octo/issues/5) | P0 | ✅ 2026-05-25 | 1d |
| P1-1 | 内置子代理预设 explore/plan/verification/general-purpose | [#6](https://github.com/Leihb/octo/issues/6) | P1 | ✅ 2026-05-25 | 2d |
| P1-2 | 沙箱执行（命令/网络/路径白名单） | [#7](https://github.com/Leihb/octo/issues/7) | P1 | ⬜ todo | 3d |
| P1-3 | 高价值内置 slash skill：`/commit` `/review` `/diff` `/compact` `/cost` `/doctor` `/resume` `/status` | [#8](https://github.com/Leihb/octo/issues/8) | P1 | ⬜ todo | 5d |
| P1-4 | `AskUserQuestion` 升级（多选 / 选项描述 / 预览） | [#9](https://github.com/Leihb/octo/issues/9) | P1 | ⬜ todo | 3d |
| P2-1 | 输出风格 Output Styles（`~/.octo/output_styles/`） | [#10](https://github.com/Leihb/octo/issues/10) | P2 | ⬜ todo | 2d |
| P2-2 | 微压缩 micro-compact（按 token 预算截断单条 tool result） | [#11](https://github.com/Leihb/octo/issues/11) | P2 | ⬜ todo | 3d |
| P2-3 | 项目级 / 团队记忆 `.octo/memories/` + 远端同步 | [#12](https://github.com/Leihb/octo/issues/12) | P2 | ⬜ todo | 5d |
| P2-4 | 启动并行预热（Session/Provider/Skill 扫描并发） | [#13](https://github.com/Leihb/octo/issues/13) | P2 | ⬜ todo | 1d |
| P2-5 | `/agents` `/skills` `/tasks` `/permissions` 管理面板（CLI 子菜单） | [#14](https://github.com/Leihb/octo/issues/14) | P2 | ⬜ todo | 3d |

状态图例：⬜ todo / 🟡 in progress / ✅ done / 🚫 skipped

---

## P0 — 影响 LLM 行为上限，必须补

### P0-1. LLM 可调度的 `Task`/`Agent` 子代理工具

**现状**：octo 只能通过 `fork_agent: true` 的 skill 触发子代理（`code-explorer` / `persist-memory` / `recall-memory` / `product-help`）。LLM 不能在对话中直接说"开三个子代理并行调研"。

**对标**：claude-code `tools/AgentTool/AgentTool.ts` + `built-in/{explore,plan,general-purpose,verification,claudeCodeGuide,statuslineSetup}.ts`。

**目标**：
- 新增 `lib/octo/tools/agent.rb`，参数：`description`（短标题）、`prompt`（任务详述）、`subagent_type?`（预设名）、`tools?`/`forbidden_tools?`（白/黑名单）、`model?`（默认 lite）。
- 封装现有 `Agent#fork_subagent`，复用 marshal-deep-clone + 缓存共享。
- 不允许子代理递归调用 Agent 工具（防爆栈），通过 `forbidden_tools` 默认排除自身。

**验收**：用例 `spec/octo/tools/agent_spec.rb` 跑通"主代理调 Agent 工具开子代理 → 子代理只能用白名单工具 → 返回字符串结果合并回主对话"。

**完成**：2026-05-25。`lib/octo/tools/agent.rb` 通过 `Octo::Agent#fork_subagent` 封装；参数 `description` / `prompt` / `subagent_type?`（P1-1 占位）/ `tools?` 白名单 / `forbidden_tools?` 黑名单 / `model?`（默认 `lite`）；白名单内部转成"全工具集 - 白名单"的黑名单注入到现有 `before_tool_use` hook；默认 `forbidden_tools` 含 `agent` 自身防递归。子代理结束后 token / USD cost 通过私有 `absorb_session_usage` 合并到父代理的 P0-4 累计器，`/cost` 和 `max_cost_usd` 看到的是完整账单。Spec `spec/octo/tools/agent_tool_spec.rb` 17 个 example。

---

### P0-2. 通用 MCP 客户端

**现状**：仅 `tools/browser.rb` 一处硬编码 chrome-devtools-mcp（stdio JSON-RPC，daemon 由 `server/browser_manager.rb` 管生命周期）。无法接其他 MCP server。

**对标**：claude-code `services/mcp/` 22 文件（`client.ts` / `MCPConnectionManager.tsx` / `InProcessTransport.ts` / `auth.ts` / SSE+stdio 双 transport）。

**目标**：
- 抽出 `lib/octo/mcp/`：
  - `client.rb` — JSON-RPC 2.0 双向流
  - `registry.rb` — server 实例池
  - `transports/stdio.rb` — spawn 子进程
  - `transports/sse.rb` — 远端 SSE
  - `tool_proxy.rb` — 把发现的 MCP tool 注册成 `Octo::Tools::Base` 子类，命名 `mcp__{server}__{tool}`
- 配置文件 `~/.octo/mcp_servers.yml`：`command`/`args`/`env`/`url`/`disabled`
- 启动时拉 tools list；连接失败 fail-open 不阻塞主流程
- `chrome-devtools-mcp` 改造为新框架下的一个 server entry（去重）

**验收**：能挂 `@modelcontextprotocol/server-filesystem` 和 `@modelcontextprotocol/server-github`，LLM 看见 `mcp__filesystem__*` 工具并能调用。

---

### P0-3. `config.yml` 用户可配置 hook

**现状**：`agent/hook_manager.rb` 提供 7 个事件（`:before_tool_use` / `:after_tool_use` / `:on_tool_error` / `:on_start` / `:on_complete` / `:on_iteration` / `:session_rollback`），但只能编程注入，外部用户改不到。

**对标**：claude-code `schemas/hooks.ts` + `settings.json` 的 `hooks` 段，shell 命令挂事件。

**目标**：复用 `~/.octo/config.yml` 的 `settings:` block（P0-4 已经在那里加了 `max_turns` / `max_cost_usd`），新增 `hooks:` 键。不开新文件。

```yaml
settings:
  max_turns: 30
  hooks:
    before_tool_use:
      - matcher: "terminal"
        command: "echo $OCTO_TOOL_INPUT >> ~/.octo/audit.log"
        block: true
    on_complete:
      - command: "osascript -e 'display notification \"Octo done\"'"
```

- 主进程在事件触发时把上下文塞 ENV（`OCTO_EVENT` / `OCTO_TOOL_NAME` / `OCTO_TOOL_INPUT` / `OCTO_SESSION_ID` / `OCTO_WORKING_DIR`），同步/异步 spawn shell
- 非零退出码默认不阻塞；`block: true` 时阻断该工具调用
- `matcher` 对 tool 事件按 tool name 精确匹配，nil 表示全匹配
- `timeout` 默认 30s
- 项目级 `.octo/config.yml` 留到 P2-3 那批项目级配置一起做

**验收**：手写 hook 让 `before_tool_use` 拦截 `terminal` 工具的 `rm -rf *` 调用并退出非零，主代理收到阻断消息。

**完成**：2026-05-25。新增 `Octo::ShellHookRunner`（stateless，负责 ENV 构建 + popen3 spawn + timeout/kill 清理 + block:true 解析）。`AgentConfig` 加 `:hooks` 字段，复用现有 `CONFIG_SETTINGS_KEYS` 自动 yaml 持久化（empty 时不输出 stray 键）。`Agent#initialize` 末尾调 `register_user_shell_hooks!`，把 yaml 描述每条 hook 转成 `@hooks` 上的 Ruby 闭包；subagent 防护通过闭包内 `@is_subagent` 运行时检查实现（构造时该 ivar 还没设好）。matcher 按 tool name 精确匹配，timeout 默认 30s，async/sync 两路。product-help/docs/agent-config.md 同步加 "Hooks" 段。

---

### P0-4. `max_turns` / `max_cost_usd` 主循环闸门

**现状**：`agent.rb:385` 主循环只有 `task_truncation_count >= 3` 兜底，没有显式上限。坏的 LLM 可以无限 tool-loop 烧钱。

**对标**：claude-code `query/tokenBudget.ts` + `QueryEngineConfig.maxTurns/maxBudgetUsd`。

**目标**：
- `Octo::AgentConfig` 新增 `max_turns`（默认 30）和 `max_cost_usd`（默认 nil=无限）
- 主循环每 iteration 末判断；超出抛 `Octo::TurnLimitExceeded` / `Octo::CostLimitExceeded`，UI 友好退出
- `/cost` slash 命令显示当前会话累计 token / 估算成本（按 provider 价目表）
- CLI 标志 `--max-turns` / `--max-cost`

**验收**：用 `--max-turns 3` 跑一个会反复 tool-loop 的 prompt，第 4 轮被截断并返回明确错误消息。

**完成**：2026-05-25。`AgentConfig#max_turns` 默认 30、`#max_cost_usd` 默认 nil；`Agent#enforce_loop_budget!` 每轮 iteration 顶部检查、抛 `Octo::TurnLimitExceeded` / `Octo::CostLimitExceeded`；`accumulate_session_usage!` 复用现有 `Octo::ModelPricing.calculate_cost` 维护 `@session_token_totals` + `@session_cost_usd`；CLI 加 `--max-turns` / `--max-cost`；TTY + JSON 两条交互路径都接 `/cost` slash。

---

## P1 — 命令面和安全面打底

### P1-1. 内置子代理预设

**依赖**：P0-1。

**目标**：`lib/octo/default_agents/` 下增加：
- `explore/` — 只读工具白名单（`file_reader` / `glob` / `grep` / `terminal` 限 `ls/cat/find`）
- `plan/` — 仅 thinking，不允许 `write` / `edit` / `terminal`
- `verification/` — 读 + 跑测试，禁止 `write` / `edit`
- `general-purpose/` — 全工具，但 `forbidden_tools: [agent]` 防递归

每个含 `agent.yml`（model、forbidden_tools）+ `system_prompt.md`。

**验收**：Agent 工具传 `subagent_type: "explore"` 时自动应用预设。

**完成**：2026-05-25。新增 `Octo::SubagentPreset` + `Octo::SubagentRegistry`（user → built-in 二级查找）+ 4 个 preset 目录 `lib/octo/default_subagents/{explore,plan,verification,general-purpose}/`，每个含 `subagent.yml`（model / forbidden_tools / description）+ `system_prompt.md`（curated playbook）。`agent` 工具的 `subagent_type` 不再是 stub：未知名字直接拒；命中 preset 时 model 缺省走 preset、forbidden_tools 与 caller 取并集、system_prompt 与 brief 拼接。同 PR 顺手修了一致性 bug：`Octo::Agent#absorb_subagent_session_usage!` 抽到 Agent 类，5 个 fork_subagent 调用点（agent tool / skill_manager / memory_updater / skill_reflector / skill_auto_creator）都接入，`/cost` 不再低估子代理开销。

---

### P1-2. 沙箱执行

**现状**：`tools/security.rb` 二分"安全/不安全"，颗粒粗。

**对标**：claude-code `BashTool` 的 sandbox prompt 段 + `services/policyLimits/`。

**目标**：
- `~/.octo/sandbox.yml`：
  ```yaml
  filesystem:
    write_allow: ["/tmp/octo-*", "~/Projects"]
    write_deny: ["**/.git/**", "**/.env"]
  network:
    allowed_hosts: ["api.github.com", "*.anthropic.com"]
  terminal:
    deny_commands: ["rm -rf /", "dd if=*"]
  ```
- `terminal` / `write` / `edit` 工具在执行前查规则；命中白名单跳过 confirm，命中 deny 直接拒绝
- `confirm_safes` 模式下，规则覆盖默认问询，减少打断

**验收**：`write` 到 `~/Projects/foo.txt` 在 `confirm_safes` 下不问询；`write` 到 `~/.ssh/config` 被拒。

---

### P1-3. 高价值内置 slash skill

**形态**：纯 SKILL.md，不开新工具，全部走现有 `terminal` / `file_reader` / `grep`。

候选清单（按优先级）：
- `/commit` — git status → diff → 生成 message（参考 CLAUDE.md 的 commit 风格）→ `git commit`
- `/review` — 仿 claude-code `commands/review.ts`：列改动 → 三路并发子代理（reuse / quality / efficiency）→ 汇总
- `/diff` — `git diff` 格式化输出
- `/compact` — 手动触发 `MessageCompressor`
- `/cost` — 当前会话 token/cost（需 P0-4）
- `/doctor` — 检查 provider 可达性、配置完整性、磁盘空间
- `/resume` — 列最近 5 个 session 让用户选（比 `-c` 显式）
- `/status` — provider/model/mode/cwd 一屏

**验收**：用户在交互模式输入 `/commit`，skill 自动跑完一次提交流程。

---

### P1-4. `AskUserQuestion` 升级

**现状**：`request_user_feedback` 是单问，UI 简单。

**对标**：claude-code `AskUserQuestionTool`（2-4 options，multiSelect，per-option description/preview/notes）。

**目标**：
- 新增 `Octo::Tools::AskUserQuestion`（与 `request_user_feedback` 并存或替代），参数：`questions[]`，每个含 `question` / `header`（≤12 字符 chip）/ `options[]`（label/description/preview）/ `multiSelect`
- Web UI `format_result_for_ui` 渲染卡片：左边选项列表、右边 preview（仅单选）
- IM 适配器降级为编号文本菜单
- CLI 用 tty-prompt 多选

**验收**：LLM 在歧义点发"实现方式 A vs B vs C"卡片，三界面（CLI/Web/IM）都能正常作答。

---

## P2 — 体验细节，可挑着做

### P2-1. 输出风格 Output Styles
- `~/.octo/output_styles/<name>.md`（frontmatter `description`）+ `/output-style <name>` 切换
- 选中后注入到 `SystemPromptBuilder` 末尾
- 内置三档：`default` / `terse` / `educational`

### P2-2. 微压缩
- 按 token 预算截断单条 tool result（特别是 `file_reader` 读大文件、`grep` 海量匹配）
- 不重写整段历史，仅压缩单条 `:content`
- 对标 `services/compact/microCompact.ts`

### P2-3. 项目级 / 团队记忆
- 新增 `.octo/memories/`（项目级，可入 git）
- `SystemPromptBuilder` 分层 merge：built-in < user < project
- 可选 `octo memory sync` 命令推到远端（CEG 知识库 / GitHub repo）

### P2-4. 启动并行预热
- `Octo::CLI.start` 入口并发跑：`SessionManager.scan` / `Providers.resolve` / `SkillLoader.scan` / `MemoryUpdater.metadata`
- 用 `Thread` + `Concurrent::Future`，注意线程安全（`@state_mutex` 等）
- 仅在测量启动时间 > 1s 时投入

### P2-5. 管理面板 slash 命令
- `/agents` `/skills` `/tasks` `/permissions` 进入子菜单（CLI 走 tty-prompt，Web UI 跳路由）
- 多数 `/api` 后端已有，主要是 UX 打包

---

## P3 — 战略上不追

| 项目 | 理由 |
|------|------|
| 自己 fork Ink 渲染器 | `ui2` 已够用，TUI 不是瓶颈 |
| vim mode / voice mode / desktop sprite | 受众小，ROI 低 |
| IDE 桥接（`bridge/` JWT+trusted-device） | octo 走 IM-first；要 IDE 集成走"octo 当 MCP server"路线更优雅 |
| 插件 marketplace | 先把 skill 生态做厚；`skill-add` 已支持 zip URL 安装 |
| `migrations/` 框架 | 量级未到 |
| `coordinator/` 多代理协调 | 超前需求 |
| 组织策略限位 `policyLimits` | to-B 需求，先 to-D |

---

## 不要为追赶而冲掉的差异化

1. **三协议原生**（Anthropic Messages / OpenAI Chat+Responses / Bedrock Converse 平等头等公民）——不要让新功能只在 Anthropic 路径跑通。
2. **三界面平等**（CLI / Web UI / IM）——新工具必带 `format_result_for_ui` 和 IM 文本回退。
3. **Skill-first 哲学**（`.octorules` 明文）——能用 skill 解决就别新增 Ruby Tool。

---

## 更新规则

每一项对应一个 GitHub issue（见状态表 Issue 列）。完成一项后：
1. PR / commit message 写 `Closes #<n>` 让 GitHub 自动关 issue
2. 把状态总览表对应行从 ⬜ 改成 ✅，加完成日期
3. 在该项小节末尾追加 `**完成**：日期 / commit hash / 关键决策记录`
4. 如果实施中发现需要拆分，在状态表新增子项（如 `P0-2a` / `P0-2b`）并各建一个 sub-issue 链回母 issue
