# C9 — 跨会话记忆（typed auto-memory）design

> 路线图源：`competitive-parity-roadmap.md` C9（"跨会话记忆 · 第二层，类型化记忆"，原与 M7
> 合并规划；M7 已落地 #87，本文是 C9 单独成案）。
>
> 设计依据：内部调研《Agent Harness Auto-Memory 机制：Claude Code vs Codex 与第三方记忆层》
> （Obsidian vault `05-Learning/Agent 系统原理/`，2026-05-27）。下文凡引"调研"均指该文。

## 1. 目标与范围

给 octo 一层 **auto-memory**：agent 自己决定记什么、自己写、自己在后续会话召回。
区别于 octo 已有的 `octorules`（手写上下文文件，第一层）—— C9 是调研里说的**第二层**。

骨架统一是 **write → manage → read**（提取 → 整合 → 注入）。

本轮基调（已与用户对齐）：

- ✅ **贴 Codex 的写入质量**：独立提取（专门的 side-call + 独立 prompt，可配更便宜的
  model）判断 load-bearing vs noise，而非主模型顺手内联。注入用整合过的 **summary**，
  不是全量索引。
- ✅ **原生为主、自足**：不装任何插件，记忆也完整可用（提取 → typed 存储 → summary 注入）。
  **不含检索**。
- ✅ **检索不进 harness 核心**：做一个 **hook 插件机制**，让 **Hindsight** 作为**可选**的
  检索增强叠加（它本就是 hook 全自动 recall/retain + 本地 embedding）。不装插件 → 用
  原生 summary 注入；装了 Hindsight → 多一路检索召回。
- ✅ **和 compaction（C6）同一套 context-management 设计**：调研强调"真正决定 harness
  强弱的是 compaction 时机，auto-memory 跟它是一套系统"。提取复用 `a.summarize` 的
  side-call 模式（`internal/agent/compaction.go`）。

适配 octo 的进程模型（关键约束）：octo 是「`octo chat` 用完即退的单二进制 CLI」，**没有
常驻后台**。所以 Codex 的"idle 6h 异步后台整合 + 全局锁 sub-agent"不照搬 —— 提取在
**会话边界**、整合在**下次启动时按需**跑（对标 Claude Code 的 Auto Dream，启动时清理），
不引入后台进程、不拖慢退出。

分两阶段，Phase 1 不依赖插件/Hindsight，可独立交付：

- **Phase 1 — 原生 typed auto-memory**：提取 / typed 存储 / 整合 / summary 注入。
- **Phase 2 — 插件机制 + Hindsight**：hook 扩展点 + 检索增强集成。

不做：向量/BM25 检索进核心（交给 Hindsight）；常驻后台；EEA 式地区限制。

## 2. 与现有系统的关系

```
第一层（手写）   octorules：~/.octo/octorules.md + .octorules   —— 已有，prompt.Compose 加载
第二层（自动）   C9 auto-memory：~/.octo/memory/                —— 本设计
context 管理     compaction（C6）：internal/agent/compaction.go —— 复用其 side-call 模式
```

C9 的注入层和 skills manifest 一样，落在 `prompt.Compose` 的**冻结 prefix** 里
（session-start 组装、provider 缓存、整场不动）——所以注入的是会话开始时就定下的
summary，**不**随对话中途新增的记忆刷新（那会让缓存失效）。新记忆在**下次会话**生效。

## 3. 存储模型（typed，一事一文件 + 注册表 + 注入摘要）

`~/.octo/memory/`，融合 CC 的「一事一文件 + 类型 frontmatter + MEMORY.md 索引」与 Codex 的
「注册表 + 注入摘要」分离：

```
~/.octo/memory/
  MEMORY.md            # 注册表/索引：一行一条（标题 + 钩子），管理与去重用
  memory_summary.md    # 注入用：整合产出的 summary，进 system prompt（read 层读它）
  <slug>.md            # 一事一文件：单条记忆 + frontmatter
```

单条 `<slug>.md` frontmatter（type 沿用调研里 CC 的语义分类，也是本仓库 auto-memory
正在用的形态）：

```markdown
---
name: <kebab-slug>
description: <一行摘要——整合/召回时判断相关性用>
type: user | feedback | project | reference
created: <YYYY-MM-DD>
last_verified: <YYYY-MM-DD>
---

<事实正文；feedback/project 附 **Why:** / **How to apply:** 行>
```

- **type 语义**：`user`（用户是谁/偏好）、`feedback`（怎么做事的纠正与确认，含 why）、
  `project`（与代码/git 无关的在研工作与约束）、`reference`（外部资源指针）。
- **MEMORY.md ≠ 注入源**：MEMORY.md 是注册表（整合时读它做去重/分类），注入进 prompt 的
  是 `memory_summary.md`（贴 Codex：整合过的紧凑摘要，避免 CC「全量索引一多就退化」）。
- 二阶性约束：`memory_summary.md` 是整合产物，不作权威事实源；需要细节时回查
  `<slug>.md`。

## 4. Write — 提取（贴 Codex，复用 summarize side-call）

提取是一次专门的 side-call，与 `compaction.go` 的 `a.summarize` 同构：
`Sender.SendMessages(ctx, extractModel, extractSystem, msgs, maxTokens)`，独立 system
prompt，token 计入会话预算。

- **extractSystem**：独立提取 prompt，明确「记什么 / 不记什么」。对标 Codex 的 stage-one：
  只记 load-bearing（用户是谁、确认过的偏好/纠正、跨会话的项目约束、外部资源指针）；
  **不记** 仓库/octorules/CLAUDE.md 已有的、单任务琐碎纠正、实时状态、下次大概率翻盘的。
  输出结构化条目（每条带建议的 type + slug + description + body）。
- **extractModel**：默认主 model；可经配置覆盖为更便宜的 model（对标 Codex 的
  `extract_model` 钩子，但 octo 只暴露一个可选覆盖，不强求）。
- **触发点**：会话边界，**不阻塞退出**。两个候选，推荐前者：
  - **(推荐) 退出标记 + 启动时提取**：`/exit`/EOF 时把本会话标记为"待提取"
    （session 元数据加一个 flag）；下次 `octo chat` 启动时检查有无待提取会话 → 跑提取。
    对标 Auto Dream 的"启动时做"，不拖慢退出，无需后台。
  - (备选) 退出时同步提取：简单直接，但拖慢退出几秒；`--no-memory` 可关。
- 写入：把提取出的条目写成 `<slug>.md` + 追加 MEMORY.md 一行。与已有 session
  持久化（`internal/agent/session.go`，`~/.octo/sessions/`）同目录体系。

## 5. Manage — 整合（启动时按需，对标 Auto Dream）

随会话累积，`<slug>.md` 会重复/过时。整合是另一次 side-call，读 MEMORY.md + 相关条目，
做合并去重、淘汰过期、刷新 `memory_summary.md`。

- **触发**：启动时按需 —— 距上次整合 > N 天 **且** 累积 ≥ M 条新记忆（默认值文档评审定，
  起点 N=1、M=5，对标 CC Auto Dream 的 24h+5session 量级）。
- **产物**：更新 `memory_summary.md`（注入源）+ 清理 `<slug>.md`/MEMORY.md。
- 失败非致命：整合 side-call 失败则保留现状，下次再试（与 maybeCompact 的容错一致）。

## 6. Read — 注入（summary 进冻结 prefix）

`prompt.Compose` 新增 memory 层，注入 `memory_summary.md` 的内容。

- 层位置（在 skills manifest 之后、user octorules 之前——记忆是"跨会话用户上下文"，
  性质接近 user 规则但更靠前，让用户显式规则仍可覆盖）：
  `base → env → skills → memory → user → project → system`
- 与 skills 一致：caller（cmd/octo）读 `memory_summary.md` 渲染好传入 `Compose`，
  保持 prompt 包不做记忆 IO（单向依赖）。空则跳过该层。
- 冻结约束同 §2：注入的是会话开始时的 summary，整场不变。

## 7. Phase 2 — 插件机制 + Hindsight（检索增强）

原生层（§3–6）自足后，加一个**记忆插件机制**让外部检索层（Hindsight）叠加。Hindsight
在 Claude Code 里靠 **UserPromptSubmit hook**（prompt 前 recall，结果作 `additionalContext`
注入）+ **post-response hook**（自动 retain）+ `agent_knowledge_*` MCP tools。

octo 引入它的两条路径（**开放决策，Phase 2 评审定**）：

- **(倾向) 事件 hook（shell-out，CC 风格）**：octo 在 turn 边界暴露两个 hook 点——
  `pre-turn`（把 user input 交给外部命令，stdout 作 additionalContext 注入本轮）、
  `post-turn`（把本轮交给外部命令做 retain）。轻，落在现有 `runLoop`，Hindsight 的
  hook 模式直接适配。代价：octo 要新增一个通用 hook 配置/执行机制（roadmap 未规划）。
- (备选) MCP client：octo 实现 MCP client 调 Hindsight 的 `agent_knowledge_*` tools。
  通用性强（顺带支持其他 MCP server），但 MCP client 是更大的独立工作。

无论哪条，原生层都不动；插件只是多一路 recall 注入 + retain。不装插件 → 退化为纯原生
summary 注入。

## 8. 决策记录

- **D1（贴 Codex 写入，但不照搬其后台）**：独立提取 + summary 注入取 Codex 的写入质量；
  idle 后台整合换成「退出标记 + 启动时整合」以适配无常驻后台的 CLI 进程模型。
- **D2（原生为主，Hindsight 可选增强）**：原生层自足、无检索；检索经插件机制外包 Hindsight。
  不装插件零外部依赖仍可用。
- **D3（注册表与注入源分离）**：`MEMORY.md`（管理/去重）vs `memory_summary.md`（注入），
  对标 Codex；避免 CC「全量索引一多 adherence 下降」。
- **D4（type 沿用 CC 语义四类）**：user/feedback/project/reference，与本仓库 auto-memory
  现用形态一致，有现成活样本。
- **D5（注入走冻结 prefix）**：与 skills manifest 同约束，新记忆下次会话生效，不中途刷新
  以保 provider 缓存。
- **D6（提取/整合复用 compaction side-call 模式）**：与 `a.summarize` 同构，不另起机制。

## 9. 分阶段与切片

**Phase 1（原生，可独立合并）**

1. `internal/memory/` 包：`Store`（读写 `~/.octo/memory/` 的 `<slug>.md` + MEMORY.md +
   memory_summary.md）、`Entry`/type、`RenderInjection()`。+ 测试。
2. 提取：`agent` 层加 `extractMemory` side-call（复用 summarize 模式）+ extractSystem
   prompt + extractModel 可选覆盖。+ 测试。
3. 触发接线：会话退出打"待提取"标记（session.go）；启动时检查待提取 + 按需整合
   （cmd/octo）。+ 测试。
4. 注入：`prompt.Compose` 加 memory 层；chat.go 两处调用点接线。+ 测试。
5. CLI/REPL：`/memory`（列出/开关）、`--no-memory`、`octo memory list` 之类表面。+ 测试。

**Phase 2（插件 + Hindsight，单独 PR/里程碑）**

6. 记忆插件机制（pre-turn / post-turn hook 点；形态 hook vs MCP 评审定）。
7. Hindsight 参考集成 + 文档。

每步 `make vet && make test`（race）+ gofmt；跨 OS `GOOS=linux/windows go build ./...`。

## 10. 开放决策点（评审拍板）

1. **提取触发**：退出标记 + 启动提取（推荐）vs 退出同步提取。
2. **整合阈值**：N 天 / M 条的默认值（起点 N=1、M=5）。
3. **extractModel**：是否暴露独立 model 覆盖，还是固定用主 model。
4. **注入层位置**：`memory` 放在 skills 之后 / user 之前（本文采用），还是紧跟 env。
5. **Phase 2 插件形态**：事件 hook（shell-out，倾向）vs MCP client。
6. **type 体系**：沿用 CC 四类是否够，要不要加 Codex 的时效分层（durable/recent）。

## 11. 测试（stdlib + httptest，无外部框架）

- `internal/memory`：Store 读写/round-trip、frontmatter 解析、MEMORY.md 索引一致性、
  RenderInjection 空/非空、整合去重。
- `internal/agent`：extractMemory side-call（用 stub Sender，仿 compaction 测试）、
  整合触发条件、失败容错。
- `cmd/octo`：待提取标记 round-trip、启动时提取/整合接线、`Compose` memory 层位置、
  `/memory` 与 `--no-memory`。
