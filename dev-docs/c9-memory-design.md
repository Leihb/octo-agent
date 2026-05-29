# C9 — 跨会话记忆（typed auto-memory）design

> ⚠️ **部分已废弃（2026-05-29）。** 本文记录的**边界提取（boundary extraction）**、
> **per-rollout summaries**、以及 **Phase 2 常驻 daemon `octo memoryd`** 已全部移除。
> 原因：边界提取会把整个上一会话的 history 再喂给**主模型**重挖一遍，与 turn 内
> `remember`（由 per-turn memory nudge 驱动，见 #132）的即时捕获**大量重复**，纯属
> token 浪费；既然 live 捕获已可靠，后台重扫不再有价值，daemon（其主要职责就是异步
> 提取）随之退休。
>
> **现状架构**：(1) turn 内 `remember` 工具捕获（nudge 提醒模型扫描 durable 信号）；
> (2) 启动时 `consolidateIfDue` 把累积的活跃条目折叠进 `memory_summary.md`（≥5 条 + ≥24h）；
> (3) 读侧——会话启动时 summary 注入冻结 prefix，**外加 PR #156 的 live cross-session delta**：
> 启动后新增的条目（含其他会话写的）下一回合经 `<memory-update>` 尾部注入热刷（见 §6a）。
> 无后台进程、无会话重扫、无 rollout 叙事文档。
> 下文凡涉及"提取 / extraction / daemon / memoryd / rollout_summaries / Phase 2"的段落
> 均为**历史设计记录**，保留以存"当初为何这么设计"的来龙去脉；各相关章节另有就地标注。
>
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
  model）判断 load-bearing vs noise。注入用整合过的 **summary**，不是全量索引。
- ✅ **即时写入（对标 CC 内联）**：会话中用户给反馈/偏好时，模型经 `remember` 工具
  **立即**落地，不必等会话结束的提取；边界提取补隐式信号 + 整合去重。
- ✅ **原生为主、自足**：不装任何插件，记忆也完整可用（提取 → typed 存储 → summary 注入）。
  **不含检索**。
- ✅ **检索不进 harness 核心**：做一个 **hook 插件机制**，让 **Hindsight** 作为**可选**的
  检索增强叠加（它本就是 hook 全自动 recall/retain + 本地 embedding）。不装插件 → 用
  原生 summary 注入；装了 Hindsight → 多一路检索召回。
- ✅ **和 compaction（C6）同一套 context-management 设计**：调研强调"真正决定 harness
  强弱的是 compaction 时机，auto-memory 跟它是一套系统"。提取复用 `a.summarize` 的
  side-call 模式（`internal/agent/compaction.go`）。

进程模型：原生层**自足**（无外部进程也能用），之上加一个**常驻 memory daemon**做真异步
——最贴 Codex 的「idle 检测 + 异步提取/整合」。daemon **离线**时记忆不失效，退回原生
fallback（退出标记 + 启动时提取/整合，对标 Claude Code 的 Auto Dream）；daemon **在线**时
接管，提取/整合常驻异步进行，不拖慢任何会话。两条路写同一套 `~/.octo/memory/`，经文件锁
互斥（对标 Codex 的全局锁）。

分三阶段，Phase 1 不依赖 daemon/插件，可独立交付：

- **Phase 1 — 原生 typed auto-memory（自足）**：提取 / typed 存储 / 整合 / summary 注入，
  触发用退出标记 + 启动时按需。无外部进程。
- **Phase 2 — 常驻 memory daemon**：接管异步提取/整合，idle 检测，生命周期管理；离线时
  fallback 到 Phase 1 路径。
- **Phase 3 — 插件机制 + Hindsight**：hook 扩展点 + 检索增强集成。

不做：向量/BM25 检索进核心（交给 Hindsight）；EEA 式地区限制。

## 2. 与现有系统的关系

```
第一层（手写）   octorules：~/.octo/octorules.md + .octorules   —— 已有，prompt.Compose 加载
第二层（自动）   C9 auto-memory：~/.octo/memory/                —— 本设计
context 管理     compaction（C6）：internal/agent/compaction.go —— 复用其 side-call 模式
```

C9 的注入层和 skills manifest 一样，落在 `prompt.Compose` 的**冻结 prefix** 里
（session-start 组装、provider 缓存、整场不动）——所以**前缀里**注入的是会话开始时就定下的
summary，整场不变（中途重算会让缓存失效）。

> 〔更新 2026-05-29，PR #156〕「新记忆只在下次会话生效」**不再成立**。冻结 prefix 仍冻结，
> 但新增了一条**缓存安全的尾部 delta 路径**：本会话启动后新增的条目（含**其他终端/会话**写的）
> 会在**下一回合**作为 `<memory-update>` 块追加到 user 消息**尾部**。尾部本就未缓存，所以
> system/tools/历史前缀照常命中——既保住缓存，又让记忆跨会话热刷。详见 §6。

## 3. 存储模型（typed，一事一文件 + 注册表 + 注入摘要 + per-rollout 引用）

> 〔已移除 2026-05-29〕`rollout_summaries/` 及 per-rollout 引用文档已删除——它们只由
> 边界提取产出，提取移除后失去生产者。现存储只剩 `<slug>.md` + `MEMORY.md` +
> `memory_summary.md`。

`~/.octo/memory/` —— **自身是 git repo**（PR #101 起，对标 Codex）：

```
~/.octo/memory/
  .git/                              # 自动初始化；每次 Save / WriteSummary / ArchiveAll 一个 commit
  .gitignore                         # 跑时排除 .lock / .state
  MEMORY.md                          # 注册表/索引：一行一条（slug + 钩子），管理与去重
  memory_summary.md                  # 注入用：整合产出的 summary，首行 v1 协议标记
  <slug>.md                          # 一事一文件：单条记忆 + frontmatter
  rollout_summaries/                 # 每会话一份详细叙事 reference（PR #102）
    <YYYYMMDD-HHMMSS>-<slug>.md
  .state                             # 提取/整合/git-baseline 三个游标（gitignored）
```

`memory_summary.md` 首行恒为 `<!-- octo-memory v1 -->`（PR #100）—— HTML 注释,读取
时剥掉,Markdown 渲染时不可见。给未来 schema 升级留余地（v2 时可一眼区分）。

单条 `<slug>.md` frontmatter：

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
  `<slug>.md`，深度回放回查 `rollout_summaries/<…>.md`。
- **`rollout_summaries/` 不进注入**：每会话一份叙事文档，按需 grep / read（base.md 在
  PR #99 / #102 中提示了这条路径），用来回答"我们以前在 X 区域做过什么吗?"。
- **archive 退场**：PR #96 的 `archive/` 子目录被 git history 取代（PR #101）。
  `ArchiveAll` 现在删工作树文件 + commit("consolidate: drop N entries…")；
  `ListArchived` 走 `git log` 还原。原 `archive/` 子目录如果已存在,被忽略不删,
  非破坏迁移。

## 4. Write — 即时 `remember` 工具 + 边界提取

> 〔已移除 2026-05-29〕**边界提取整条路径已删除**（`ExtractMemory`、
> `extractPreviousSession`、启动 fallback、三件套产出）。写入只剩即时 `remember`
> 工具，由 per-turn memory nudge（#132）驱动模型在 turn 内捕获。下文"边界提取 / daemon
> 离线 / `.state.last_extracted_session`"等均为历史设计。

写入有两个来源（对标 Claude Code：主模型内联即时写 + 事后整合补全）：

### 4a. 即时：`remember` 工具（会话内，捕捉显式信号）

会话进行中用户给出**反馈 / 偏好 / 纠正 / 跨会话约束**时（"我希望你下次运行完测试再
提交"、"以后提交信息用中文"），**立即**落地，而不是等会话结束才提取。

- `remember` 工具加进 tools（像 skill 工具）：参数 `content` + 可选 `type`（模型判断
  user/feedback/project/reference）+ `description`。Execute 立即写 `<slug>.md` + 追加
  MEMORY.md，写前查 MEMORY.md 去重。
- **触发**：(a) 模型自主 —— base prompt 引导它识别到 load-bearing 用户反馈时调用；
  (b) 用户显式 —— `/remember <text>` 或自然语言"记住…"。
- **不刷新当前会话注入**：注入走冻结 prefix（§2/§6），即时写入只落盘、不重注入当前会话；
  但该反馈已在对话上下文里，当前会话本就遵守，记下来是为**下次**会话持久生效。
- 即时工具吃显式信号（低延迟、高精度，用户当场说的）；隐式信号 + 去重整合交给 4b 与 §5。

### 4b. 边界：提取 side-call（事后补全，三件套产出）

提取是一次专门的 side-call，与 `internal/agent/compaction.go` 的 `a.summarize` 同构：
`Sender.SendMessages(ctx, extractModel, extractSystem, msgs, maxTokens)`，独立 system
prompt，token 计入会话预算。

- **extractSystem**（PR #98 升级到 Codex stage_one 风格 → PR #102 改产三件套）：
  从 ~20 行扩到 ~140 行,核心 patterns 全部就位：
  - **No-op gate**：显式问"未来 session 会因这条 fact 做得更好吗？" 否则丢弃。
  - **Reading priority**：User > Tool output > Assistant。assistant 提议未被用户采纳
    不能写成 durable。
  - **Outcome triage**：success / partial / fail / uncertain，不同结局写不同内容。
  - **Evidence → implication shape**：feedback 必须 `user said "<quote>" → implies <default>`。
  - **三件套产出**（PR #102）：单次 side-call 同时产 `rollout_slug` + `rollout_summary`
    + `facts[]`。No-op 时全空。
- **extractModel**：默认主 model；可经配置覆盖为更便宜的 model（对标 Codex 的
  `extract_model` 钩子，但 octo 只暴露一个可选覆盖，不强求）。当前未暴露 CLI flag。
- **触发点（双模式，都不阻塞退出）**：
  - **daemon 在线**：daemon 经 idle 检测发现会话停止活动（见 §7），异步跑提取，完全不碰
    会话进程。
  - **daemon 离线（Phase 1 fallback,已落地）**：下次 `octo chat` 启动时检查上一会话
    （cmd/octo/memory_extract.go `extractPreviousSession`）→ 跑提取，状态游标用
    `.state.last_extracted_session`。对标 Auto Dream 的"启动时做"，不拖慢退出。
  两条路写入同一套 `<slug>.md` + MEMORY.md + rollout_summaries/，经文件锁互斥（§7）+
  git auto-commit（§3）。`--no-memory` 全关。
- **写入**：
  - 每条 fact → `<slug>.md` + 追加 MEMORY.md 一行 + commit("remember: <slug>")。
  - rollout_summary → `rollout_summaries/<timestamp>-<slug>.md` + commit("rollout-summary: <slug>")。
  - 模型返空 slug 时,fallback 用 session id。空 summary → 不写文件（no-op）。

## 5. Manage — 整合（daemon idle / 启动时按需）+ sub-agent + git baseline

> 〔部分已移除 2026-05-29〕整合（consolidation）本身**保留**：仍由 `consolidateIfDue`
> 在 `octo chat` 启动时触发（≥5 条 + ≥24h），sub-agent + git baseline 路径不变。删除的
> 只是 **daemon idle 触发那一路**——`memoryd` 退休后整合只在启动时跑。

随会话累积，`<slug>.md` 会重复/过时。整合是另一次 LLM 调用,读 MEMORY.md + 相关条目，
做合并去重、淘汰过期、刷新 `memory_summary.md`，然后**删除已整合的 entry**（git
history 保留）。

- **触发（Phase 1 已落地）**：启动时按需，累积 ≥ **5** 条新记忆 **且** 距上次整合
  > **24h**（`cmd/octo/memory_extract.go` `consolidateIfDue`）。daemon Phase 2 时改为
  idle 异步。
- **执行路径双轨（PR #105 起,对标 Codex Phase 2 sub-agent 模式）**：
  - **sub-agent 优先**（REPL 模式默认）：`consolidateViaSubAgent` 经 M10 `launch_agent`
    spawn 一个受限 sub-agent,只给 `["read_file", "grep", "glob"]` 三件读工具,
    sub-agent 自己 grep `rollout_summaries/`/读 `<slug>.md`/查 `MEMORY.md`,
    在隔离 context 里产出新 summary。比单次 side-call 多出"自主翻文件"的能力。
  - **side-call fallback**：`consolidateViaSubAgent` 返空（无 spawner、或 sub-agent
    报错），落回 `a.ConsolidateMemory(priorSummary, newNotes)` 的一次性 LLM 调用，
    与 PR #96 以来一致。
- **incremental consolidate**（PR #96 起）：两条路径都接受 `(priorSummary, newNotes)`
  作输入，把新增 entries 折进现有 summary，不每次重建。priorSummary 空 → INIT 模式
  （首次）；非空 → INCREMENTAL UPDATE。两 mode 共用一个 ~10 行 prompt（升级到 Codex
  880 行 的 INIT/INCREMENTAL 分模式 prompt 是 backlog）。
- **产物**（两条路径都走这一步）：
  - 写新 `memory_summary.md` 经 `Store.WriteSummary`（加 v1 标记 + commit("consolidate:
    write summary")）。sub-agent 自己不写文件，输出文本回到父调用。
  - `ArchiveAll` 删除已整合的 `<slug>.md` + 重建 MEMORY.md → commit("consolidate: drop
    N entries…")。git history 是 archive（PR #101 起替代旧的 `archive/` 子目录）。
- **state 三游标**（`.state` JSON,gitignored）：
  - `last_extracted_session` —— 上次跑过提取的 session id,防重复处理。
  - `last_consolidated` —— 上次整合的日期(YYYY-MM-DD),驱动 24h 触发。
  - `last_consolidated_sha` —— 上次整合 commit 的 SHA。`Store.WorkspaceDiff(SHA)` API
    已就位，未来 sub-agent 可拿"自上次整合以来 git diff"作上下文（当前 sub-agent 还
    没消费这个 API,等需求出现再接）。
- 失败非致命：sub-agent 报错或 side-call 报错都保留现状，下次再试（与 maybeCompact
  的容错一致）。

## 6. Read — 注入（summary 进冻结 prefix）+ live cross-session delta（§6a）

> 〔部分已移除 2026-05-29〕summary 注入**保留**。"按需 rollout_summaries"那部分已废弃
> ——rollout 文档不再产出，base.md 里相应的 on-demand reference 引导也应随之失效。

`prompt.Compose` 新增 memory 层，注入 `memory_summary.md` 的内容（剥掉首行 v1 标记）。

- 层位置（skills 之后、用户身份/规则之前——记忆是"跨会话用户上下文"，让用户显式
  规则仍可覆盖）：
  `base → soul → env → skills → memory → user.md → octorules(user) → octorules(project) → --system`
  其中 `soul` / `user.md` 由独立的**身份文件特性**提供（见 `identity-files-design.md`）；
  本设计只负责 `memory` 层。
- 与 skills 一致：caller（cmd/octo）读 `Store.RenderInjection()` 渲染好传入 `Compose`,
  保持 prompt 包不做记忆 IO（单向依赖）。`memory_summary.md` 缺失 fallback 到 entries
  列表；都空则跳过该层。
- **按需 rollout_summaries（PR #99 + #102 加进 base.md）**：injection 之外，base.md 的
  Memory 段（~60 行）告诉模型 `rollout_summaries/<…>.md` 是 on-demand reference —— 用户
  问"我们以前在 X 区域做过什么吗？"或当注入 summary 太短时，模型可以自己 grep / read
  这些文件深挖。不进 prompt prefix（会被 base.md 的引导驱动按需 read_file）。
- **base.md Memory 段还教了三件事**（PR #99）：
  - **Citations**：用 `(from memory: <slug>)` 内联标注 load-bearing 的记忆引用,避免把
    旧记忆当作用户当下说的内容；轻量,不强制每条都加 —— 仅当某条记忆物质性影响这次回答
    时才注明。
  - **Verify-first**：记忆里命名的 path/function/flag 在动手前 `grep` / `read_file` 验证;
    背景信息（用户身份、风格）不必每次验证。
  - **User contradicts memory → user wins**：调用 `remember` 写下新事实,整合下次会和老
    的对账。
- 冻结约束同 §2：**prefix 里**注入的是会话开始时的 summary，整场不变（保缓存）。中途的
  新增改由下面的 live delta 路径处理。

### 6a. Live cross-session delta（PR #156，2026-05-29）

前缀冻结意味着「A 终端写的记忆，B 终端正在跑的会话看不到」——#156 在**不破缓存**的前提下补上这点：

- **注入点**：每回合 `runTurn`（`cmd/octo/turncore.go`）在 memory nudge 之后调
  `cfg.memRefresh.delta()`，把**本会话启动后新增**的条目作为 `<memory-update>` 块追加到
  `turnInput` **尾部**。尾部是当回合的新 user 消息、本就未缓存，所以 system/tools/历史前缀
  仍命中（octo 的断点是 system + tools + 最后两条消息）。
- **变更检测**：`memory.Store.Version()` —— `MEMORY.md` 的 fnv 指纹（`Save` 每次重建索引），
  **走锁读**，未变即快速 no-op。变了才 `List()` 取条目。
- **并发读安全**：`Store.List()` 现在**加锁**（与 `Save` 同一把 advisory `.lock`），避免读到
  半写的索引/条目；持锁的内部调用方（`rebuildIndex`/`ArchiveAll`/`ArchiveCwd`）改用无锁
  `listEntries()` 防自死锁。
- **去重 / 项目过滤**：`memoryRefresher`（`cmd/octo/memory_refresh.go`）启动时把会话开始已注入
  的条目标记为 seen，`delta()` 只吐之后新增、且匹配当前 project 的条目；读失败不推进 version，
  下回合重试。
- **范围**：仅新增**活跃条目**（`remember` 那种）。consolidation 改写 summary 的变化暂不热刷。
  生效时机是**下一回合**，不打断在途回合。`--no-memory` 下整条路径关闭。

> 与 Claude Code 对比:CC 的记忆也是 session-start 冻结、要重启才更新;#156 让 octo 在这点上更进
> 一步(跨会话热刷)。

## 7. Phase 2 — 常驻 memory daemon

> 〔整章已移除 2026-05-29〕`octo memoryd` 守护进程**已删除**（命令、`memoryd_work`、
> `internal/memoryd` 包、PID/launchd 全部移除）。daemon 的主要职责是异步边界提取；提取
> 取消后它失去存在理由，整合改为仅在 `octo chat` 启动时跑。本章全文仅作历史设计记录。

`octo memoryd`：长期运行的守护进程，接管异步提取（§4）与整合（§5）；离线时一切退回
Phase 1 的启动时路径，记忆功能不丢。

- **生命周期**：`octo memoryd start|stop|status`。PID 文件 `~/.octo/memoryd.pid`；
  SIGTERM 优雅关闭（跑完手头的 side-call 再退）；start 时检测已在跑则报错退出。
- **感知会话结束（松耦合，已定 2026-05-28）**：daemon watch `~/.octo/sessions/` 的 mtime，
  两类信号：打了"待提取"标记的会话 daemon **立即**处理；无标记但 mtime 静默 ≥ **idle
  阈值（默认 15 分钟，可配）** 的视为异常退出/挂起，兜底提取。daemon 与 `octo chat`
  **互不依赖**：chat 不必通知 daemon，daemon 不在也不影响 chat。（否决 Unix socket IPC：
  更及时但把 chat 与 daemon 耦合，不值。）
- **并发与锁**：daemon 是 `~/.octo/memory/` 的主写者；fallback 路径下 chat 启动时也可能写。
  统一一把文件锁（`~/.octo/memory/.lock`，flock）互斥，对标 Codex 的全局锁。
- **provider 配置**：daemon 跑提取/整合 side-call 需要 key + provider，启动时从 env /
  `~/.octo/config`（若有）读取，与 chat 同源；缺配置则拒绝启动并提示。
- **跨平台**：macOS 经 launchd plist、Linux 经 systemd user service 托管（可选
  `octo memoryd install` 生成单元文件）；**Windows 降级** —— 不做 daemon，强制走 Phase 1
  启动时路径（与 sandbox 的 Windows 降级一致，fail-soft）。
- **自启动（已定：手动）**：MVP `octo memoryd start` 手动启动。自动 spawn / launchd 托管
  留待后续，不在首版。

## 8. Phase 3 — 插件机制 + Hindsight（检索增强）

原生层（§3–6）自足后，加一个**记忆插件机制**让外部检索层（Hindsight）叠加。Hindsight
在 Claude Code 里靠 **UserPromptSubmit hook**（prompt 前 recall，结果作 `additionalContext`
注入）+ **post-response hook**（自动 retain）+ `agent_knowledge_*` MCP tools。

octo 引入它的两条路径（**开放决策，Phase 3 评审定**）：

- **(倾向) 事件 hook（shell-out，CC 风格）**：octo 在 turn 边界暴露两个 hook 点——
  `pre-turn`（把 user input 交给外部命令，stdout 作 additionalContext 注入本轮）、
  `post-turn`（把本轮交给外部命令做 retain）。轻，落在现有 `runLoop`，Hindsight 的
  hook 模式直接适配。代价：octo 要新增一个通用 hook 配置/执行机制（roadmap 未规划）。
- (备选) MCP client：octo 实现 MCP client 调 Hindsight 的 `agent_knowledge_*` tools。
  通用性强（顺带支持其他 MCP server），但 MCP client 是更大的独立工作。

无论哪条，原生层都不动；插件只是多一路 recall 注入 + retain。不装插件 → 退化为纯原生
summary 注入。

## 9. 决策记录

- **D1（贴 Codex 写入 + 常驻 daemon 真异步）**：独立提取 + summary 注入取 Codex 的写入
  质量；并做常驻 memory daemon 实现 idle 异步提取/整合（最贴 Codex）。daemon 离线时退回
  退出标记 + 启动时整合，所以**原生层始终自足**，记忆不硬绑 daemon。
- **D2（原生为主，Hindsight 可选增强）**：原生层自足、无检索；检索经插件机制外包 Hindsight。
  不装插件零外部依赖仍可用。
- **D3（注册表与注入源分离）**：`MEMORY.md`（管理/去重）vs `memory_summary.md`（注入），
  对标 Codex；避免 CC「全量索引一多 adherence 下降」。
- **D4（type 沿用 CC 语义四类）**：user/feedback/project/reference，与本仓库 auto-memory
  现用形态一致，有现成活样本。
- **D5（注入走冻结 prefix）**：与 skills manifest 同约束，新记忆下次会话生效，不中途刷新
  以保 provider 缓存。
- **D6（提取/整合复用 compaction side-call 模式）**：与 `a.summarize` 同构，不另起机制。
- **D7（daemon 经 sessions mtime 感知会话结束，与 chat 松耦合）**：daemon 不依赖 chat
  通知、chat 不依赖 daemon 在线；Windows 降级到无 daemon。锁用 flock 单点互斥。
- **D8（即时 remember 工具 + 边界提取双路径，对标 CC）**：显式用户反馈经 `remember`
  工具会话内即时落地；边界提取补隐式信号 + 整合去重。即时写入因 prefix 冻结不刷新当前
  会话，下次会话生效。
- **D9（git baseline 替换 archive,PR #101）**：`~/.octo/memory/` lazy `git init`,
  Save/WriteSummary/ArchiveAll auto-commit。`archive/` 子目录退场,git history 是 archive。
  `WorkspaceDiff(baseSHA)` + `LastConsolidatedSHA` 给未来 sub-agent consolidator 留接口。
  好处：rollback 天然 / deletion 信号 / 审计完整。Lazy（无 git in PATH → 静默降级,memory
  仍可用）。
- **D10（三件套 per-rollout 产出,PR #102）**：单次 extract side-call 同时产 `slug` +
  `rollout_summary`（叙事 reference）+ `facts[]`（typed durable）。对标 Codex stage_one
  返回的 `{rollout_summary, rollout_slug, raw_memory}`。Slug fallback 用 session id；
  rollout_summary 空 → no-op。`extractMaxTokens` 1024 → 4096 给叙事留空间。
- **D11（memory_summary.md v1 协议标记,PR #100）**：首行 `<!-- octo-memory v1 -->`,
  HTML 注释（Markdown 不渲染）+ `ReadSummary` 自动剥。给未来 schema 升级留 v2 / v3 通道,
  不必凭 body shape 猜版本。Legacy markerless 文件透传（PR #96 时代的文件不破坏）。
- **D12（extract prompt 升级到 Codex stage_one 形态,PR #98 → PR #102）**：从 ~20 行
  扩到 ~140 行,囊括 no-op gate / outcome triage / evidence→implication / user>tool>assistant
  / explicit DO-NOT。直接修 PR #96 实测时观察到的"两条 feedback 重复"低质量。
- **D13（base.md Memory 段升级到 ~60 行,PR #99）**：教模型 citations（`(from memory: …)`
  内联标注）+ verify-first（动手前验证路径/函数/flag）+ "user contradicts → user wins"。
  借了 Codex `read_path.md` 130 行的形,但用更轻的内联引用而不是 `<oai-mem-citation>`
  XML block —— 适合 conversational REPL,不适合一次性回答的 ceremony。
- **D14（M10 sub-agent tool,PR #104）**：`launch_agent` tool 让父 agent 并发派发子任务,
  child 共享 parent.Sender + parent.System,独立 History,独立 loop budget,token 经
  `AccrueChildUsage` 回滚到 parent。Recursion guard：context marker + tool-list filter
  双层防御。多 `launch_agent` 调用并行 dispatch（`launch_agent ∈ readOnlyTools`）。
- **D15（整合改 sub-agent 执行,PR #105）**：`consolidateViaSubAgent` 在 REPL 启动时
  spawn 一个 sub-agent，给它 `["read_file", "grep", "glob"]` 三件读工具，sub-agent
  自己 grep `rollout_summaries/`、读 `<slug>.md` 拿额外上下文,输出新 summary。
  父调用 `Store.WriteSummary` 写盘（保留 v1 marker + commit 处理）。无 spawner 或
  sub-agent 报错时落回 `ConsolidateMemory` side-call。对标 Codex Phase 2 consolidation
  agent 但克制：sub-agent 不写文件（避免绕开 v1 marker / git commit），只输出文本。
- **D16（Phase 2 memoryd 落地, PR #115–#117）**：`internal/memoryd` 包 + `octo memoryd
  start|stop|status` 命令。手动启动 foreground 进程,PID 文件 `~/.octo/memoryd.pid` +
  SIGTERM 优雅退出。Per-tick 工作：扫描 `~/.octo/sessions/` 找 idle (mtime >
  15min 默认阈值) + 未提取的会话,跑 `ExtractMemory` + `consolidateIfDue`。Provider
  从 env (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `OCTO_MEMORYD_PROVIDER`)
  独立读取；缺配置时启动失败。Chat 路径在 `memorydAlive()` 时跳过 startup
  memory pass —— daemon 在线 = chat 启动即时,不阻塞 LLM 调用。Windows 整体
  fail-soft（IsRunning structurally 返 false → chat 始终走 Phase 1）。
- **D17（Phase 3 hook 机制落地, PR #118）**：`internal/hooks` 包 + 两个 env-var
  钩子点。`OCTO_HOOK_PRE_TURN` 在每 turn 之前 shell out（用户输入 → stdin JSON,
  stdout 文本 / `{additional_context}` JSON → 拼到 user message 后面）；
  `OCTO_HOOK_POST_TURN` 在 turn 成功后 fire-and-forget（user + reply → stdin,
  stdout 忽略）。`OCTO_HOOK_TIMEOUT` 5s 默认 / 30s 上限 / `WaitDelay 1s` 兜底
  防 child sleep 拖住 pipe。错误降级为 stderr 一行 `↳ ... hook: <err>`,
  turn 继续。形态与 CC `UserPromptSubmit` hook 一致 —— Hindsight 等检索层
  脚本直接复用（详见 `c9-phase3-hindsight.md`）。

## 10. 分阶段与切片

**Phase 1（原生自足）—— ✅ 已完成**

| # | 内容 | PR |
|---|---|---|
| 1 | `internal/memory/` 包：`Store` + `Entry`/type + `RenderInjection` + flock | #93 |
| 2 | `remember` 工具：即时写入 + base prompt 引导 + REPL `/remember` | #93 |
| 3 | 边界提取：`agent.ExtractMemory` side-call + extractSystem | #93 |
| 4 | 触发接线：startup 时检查上一会话 + 按需整合（`maybeProcessMemory`） | #94 |
| 5 | 注入：`prompt.Compose` 加 memory 层（9 层完整顺序） | #93 |
| 6 | CLI/REPL：`/memory`、`--no-memory`、`octo memory list [--archive]` | #93, #96 |
| 7 | Incremental consolidate + ArchiveAll | #96 |
| 8 | Codex stage_one 风格 extractSystem（no-op gate / outcome triage / …） | #98 |
| 9 | base.md Memory 段升级 ~60 行（citations / verify-first） | #99 |
| 10 | `memory_summary.md` v1 协议标记 | #100 |
| 11 | Git baseline 替换 archive（lazy init / auto-commit / WorkspaceDiff） | #101 |
| 12 | 三件套 per-rollout 产物（slug + summary + facts） | #102 |
| 13 | M10 — sub-agent tool（launch_agent + 父子 token rollup + 并行 dispatch） | #104 |
| 14 | 整合用 sub-agent 执行（M10-backed，read-only 工具白名单 + 落回 side-call） | #105 |

**Phase 2（常驻 daemon）—— ✅ 已完成**

| # | 内容 | PR |
|---|---|---|
| 15 | `internal/memoryd` daemon core: PID 文件 / start-stop-status / SIGTERM 优雅退出 / Windows fail-soft | #115 |
| 16 | Per-tick 工作循环: scan sessions for idle+unextracted → ExtractMemory + consolidateIfDue. Provider 从 env 独立读, fail-fast on missing key | #116 |
| 17 | Chat 协调: `memorydAlive()` 时跳过 startup memory pass, daemon 接管 | #117 |
| — | 跨平台托管 (launchd / systemd unit files): 留作可选,MVP 手动 `octo memoryd start` ([design §7 决定](#7-phase-2--常驻-memory-daemon)) | — |

**Phase 3（插件 + Hindsight）—— ✅ 已完成**

| # | 内容 | PR |
|---|---|---|
| 18 | `internal/hooks` pre-turn / post-turn hook 机制: env-var 配置, sh-c 执行, JSON / 文本双格式, 5s 默认 timeout + WaitDelay 防 stuck-child | #118 |
| 19 | Hindsight 参考集成 + 文档: `dev-docs/c9-phase3-hindsight.md` 含 recall/retain 脚本样板 + 其他检索层接入指南 | #119 |
| — | MCP client 路径: 留作 future milestone, hook 已够 Hindsight 用 | — |

每步 `make vet && make test`（race）+ gofmt；跨 OS `GOOS=linux/windows go build ./...`。

## 11. 开放决策点（评审拍板）

1. ✓ **已定（2026-05-28）daemon 感知会话结束**：监控 sessions mtime（松耦合）；否决 socket IPC。
2. ✓ **已定 idle 阈值**：默认 15 分钟（可配）；有退出标记的会话立即处理。
3. ✓ **已定 daemon 自启动**：MVP 手动 `octo memoryd start`；自动 spawn / launchd 留后续。
4. ✓ **已定（PR #94）整合阈值**：24 小时 + 5 条新记忆。后续如需调整再开议。
5. **extractModel**：是否暴露独立（更便宜）model 覆盖，还是固定用主 model。当前未暴露 CLI flag。
6. ✓ **已定 注入层位置**：`memory` 在 skills 之后、user 之前（已落 `prompt.Compose`）。
7. **Phase 3 插件形态**：事件 hook（shell-out，倾向）vs MCP client。
8. **type 体系**：沿用 CC 四类是否够，要不要加 Codex 的时效分层（durable/recent）。
9. **整合 prompt 升级**：当前 consolidate prompt 还是 ~10 行,Codex 是 880 行 INIT/INCREMENTAL
   双 mode。值得做但 prompt-engineering 成本高,建议等 M10 sub-agent consolidator 落地时
   一并设计（sub-agent 拿 diff 自己决定改哪个文件,prompt 角色变了）。
10. **rollout_summaries 的 GC**：当前不删。会随时间无限增长。Phase 2 daemon 落地后可加
    `max_unused_days` / `last_usage` 清理（对标 Codex 的 extension `prune`）。

## 12. 测试（stdlib + httptest，无外部框架）

- `internal/memory`：Store 读写/round-trip、frontmatter 解析、MEMORY.md 索引一致性、
  RenderInjection 空/非空、整合去重、flock 两 writer 竞争互斥。
- `internal/agent`：extractMemory side-call（用 stub Sender，仿 compaction 测试）、
  整合触发条件、失败容错。
- `cmd/octo`：待提取标记 round-trip、启动时提取/整合接线、`Compose` memory 层位置、
  `/memory` 与 `--no-memory`。
- **daemon**：idle 检测（伪造 sessions mtime）、生命周期（start 重复检测 / stop /
  status）、fallback 协调（daemon 在线时 chat 跳过启动时提取）。用临时 `~/.octo`，
  stub provider，不起真网络。

---

## Appendix A — Codex 实测对照（2026-05-28）

读 `~/Projects/github/codex/codex-rs/memories/` 源码后整理。原 §1–§12 的基调和分阶段
仍然成立（Codex 风格 + 原生 fallback + 三阶段），但**真实实现比调研描述精细一个数量
级**，几个具体细节调研也漏了或不准；记录在此供 Phase 2/3 设计时参照，避免凭印象。

### A.1 真实架构（不是单一 store）

Codex memories 是**三个 crate + state DB + git baseline**：

| Crate | 职责 |
|---|---|
| `memories-read` | read path 注入 + **citations**（条目带 source 引用）+ usage 遥测 |
| `memories-write` | Phase 1 + Phase 2 + storage + workspace + extensions |
| `memories-mcp` | 把 memories 目录暴露成**只读 MCP server** 给 sub-agents |

存储两层：
- **State DB (SQLite)** — Phase 1 输出落这里，含 claim / lease / backoff / 并发上限，
  防止多个进程或 worker 重复处理同一 rollout。
- **文件系统 `~/.codex/memories/`** — **本身是 git repo**（`.git/` 在 root 下），用
  git workspace diff 决定 Phase 2 是否需要跑、得到 deletion 信号、回滚。
  - `memory_summary.md`（首行 `v1`，**always loaded into system prompt**）
  - `MEMORY.md`（handbook，按 keyword grep）
  - `raw_memories.md`（Phase 1 merged 临时输入）
  - `skills/<name>/SKILL.md`（**整合可以生成 skill！**）
  - `rollout_summaries/<slug>.md`（每个 rollout 一份详细 recap）
  - `extensions/<name>/` — 可插拔记忆源（`ad_hoc`、`prune`，外部产物可加入）

### A.2 两阶段（不是单 side-call）

- **Phase 1 — Rollout Extraction (per-thread)**
  - 触发：**root session 启动时**，异步后台跑（不是 idle 6h；6h 是 Phase 1 *选 rollout*
    时的过滤阈值，避免动 still-active 的 rollout）。
  - 用 state DB 做 claim/lease，并行处理多个 rollouts（concurrency cap）。
  - 模型：独立小模型（`stage_one.MODEL = "gpt-5.4-mini"` + 配置的 reasoning effort）。
  - **stage-one prompt = 569 行**（调研写"570 行"几乎精确）。
  - 输出 **JSON**：`{rollout_summary, rollout_slug, raw_memory}`，三者都可能为空（no-op gate）。
- **Phase 2 — Global Consolidation**
  - 全局锁（确实存在 — 调研里的"全局锁 sub-agent"指的是这个）。
  - 加载 Phase 1 outputs（按 `usage_count` + `last_usage`/`generated_at` 排序，过滤 `max_unused_days`）。
  - sync `raw_memories.md` + `rollout_summaries/`，**git diff** 决定 dirty。
  - 若 dirty → **spawn 内部 consolidation sub-agent**（不是 inline side-call），
    限制：no approvals / no network / local write only / no collab（防递归）。
  - sub-agent 拿 `phase2_workspace_diff.md` 作上下文，自己决定怎么改
    `memory_summary.md` / `MEMORY.md` / `skills/`。
  - 完成后 reset git baseline。
  - **consolidation prompt = 880 行**，有 INIT / INCREMENTAL UPDATE 两个 mode。

### A.3 调研对/错对照

| 调研说 | 实际 | 评 |
|---|---|---|
| stage-one prompt 570 行 | 569 行 | ✅ 精确 |
| idle 6h 触发整合 | 触发是 **session start**；6h idle 是 Phase 1 *选 rollout* 时的过滤 | ⚠️ 调研口径不准 |
| 全局锁 sub-agent | Phase 2 真的 spawn sub-agent 跑整合 | ✅ 存在 |
| 独立提取小模型 | gpt-5.4-mini + reasoning | ✅ 印证 |
| `MEMORY.md` 注册表 + `memory_summary.md` 注入 | ✅，且 summary 首行 `v1` 协议字段 | ✅ 完整 |
| Codex 没有检索 | 真的没（**memories-mcp** 只暴露 fs，没向量索引） | ✅ 印证 |
| — | git baseline tracking | 漏 |
| — | memory extensions（可插拔 sources：`ad_hoc`/`prune`） | 漏 |
| — | citations（条目带引用） | 漏 |
| — | 整合可生成 skill（`skills/<name>/SKILL.md`） | 漏 |
| — | read_path.md 130 行专门 prompt 教模型用 memory | 漏 |

### A.4 stage_one prompt 设计精华

我那 ~20 行 extractSystem 是玩具,Codex 569 行里值得借鉴的核心 pattern：

- **No-op gate**：在产出前显式问"未来 agent 会因为我写的东西做得更好吗?"。如果是
  one-off 查询 / 临时事实 / 平凡知识 / 没有偏好信号 → 返回**全空字段**。我们也说了
  "没东西输出 []"，但没有把它做成一个正式的 "minimum signal gate"。
- **Outcome triage**：每个 task 标 `success | partial | fail | uncertain`，fail/partial
  时 prompt 强调 prevention rules。我们只有 type 没有 outcome。
- **Evidence → Implication shape**：
  `当 <situation>，用户 said "<quote>" → <suggests future default>`。
  保留 user 原话 + 推断,而非压缩成结论。这是 prompt 整篇都在强调的。
- **User > Tool output > Assistant**：读 rollout 的优先级。**用户文本是 preference 的
  最强证据**；assistant brainstorming 不该被当 durable memory 除非用户采纳。
- **rollout_summary vs raw_memory 双产物**：summary 可以详细（作 reference），
  raw_memory 严格（durable preference 为主）。我们一条 fact = 一份 entry，没这个分层。
- **Anti-patterns 显式列出**：avoid generic advice / 大段 raw output / 长 procedural
  recap / 把 assistant 提议当 durable / "should use X" 推荐语言。

frontmatter 字段也更丰富：`description / task / task_group / task_outcome / cwd / keywords`。
我们：`name / description / type / created / last_verified`。

### A.5 consolidation prompt 设计精华

- **Progressive disclosure 是显式目标**：summary 始终注入 / `MEMORY.md` grep / rollout
  summaries 按需读 / skills 模板化复用。和我们的 `RenderInjection`（summary 优先 fallback
  到 entries）一致，但 Codex 多了 skill 生成层。
- **INIT vs INCREMENTAL UPDATE 两 mode**：第一次 vs 后续。INIT 时还要建最小骨架文件。
  我们 PR #96 后实际只有 INCREMENTAL（首次时 priorSummary 空,自然 INIT 化）—— 但没显式
  分 mode，prompt 措辞也没区分。
- **Phase 2 决策给 sub-agent 写文件，不是 side-call 拿字符串**。sub-agent 拿 diff，自己
  决定改哪些文件。我们 ConsolidateMemory 返回 string，cmd 写 `memory_summary.md`。

### A.6 我们 C9 vs Codex 差异表（更新 2026-05-28,PR #98–#102 后）

| 维度 | 我们 C9 Phase 1 | Codex | 差距评 |
|---|---|---|---|
| 触发 | 启动时同步 / Phase 2 daemon idle | Session start 异步后台 | Phase 2 daemon 落地后对齐 |
| 提取模型 | 主 model（无 override） | 独立小模型 + reasoning effort | open decision §11.5 |
| 提取 prompt | ~140 行 stage_one 风格（no-op gate / outcome triage / evidence→implication / DO-NOT） | 569 行 | ✅ 形态对齐,prompt 体量小很多 |
| 写入产物 | **rollout_slug + rollout_summary + facts[]** 三件套（PR #102） | rollout_summary + rollout_slug + raw_memory | ✅ 对齐;facts[] 走 typed 数组,raw_memory 走单 markdown |
| 整合执行 | **sub-agent 优先**（read-only tools；PR #105），失败回 side-call | spawn sub-agent,限权（no net/no approvals/no collab） | ✅ 形态对齐；我们的 sub-agent 不写文件（避免绕开 v1 marker / git commit），只输出文本 |
| 整合 prompt | ~10 行 incremental | 880 行 INIT/INCREMENTAL 两 mode | open decision §11.9 |
| 整合产物 | `memory_summary.md` + 删 entries(commit) | summary + MEMORY.md + skills/ + 改 rollout_summaries | skills/ 自动生成是更大 scope,未排期 |
| 状态管理 | `.state` JSON（last_extracted_session + last_consolidated + last_consolidated_sha） | SQLite state DB（claim/lease/backoff/usage_count/last_usage） | 单进程够用;Phase 2 多 worker 时再升级 |
| 增长控制 | **git baseline + history-as-archive**（PR #101） | git baseline + workspace diff | ✅ 形态对齐 |
| Summary 协议标记 | `<!-- octo-memory v1 -->` 首行（PR #100） | `v1` 首行 | ✅ 对齐 |
| 检索 | 无（依赖 Phase 3 Hindsight） | 无（read 端纯 fs,sub-agent 经 MCP 访问） | 一致 |
| 注入 prompt | ~60 行 base.md Memory 段（citations 内联 / verify-first）（PR #99） | 130 行 `read_path.md` + `<oai-mem-citation>` XML block | 形态对齐,citations 走轻量内联,故意更轻 |
| rollout_summaries | ✅ 写入 + on-demand grep（PR #102） | ✅ 写入 + read_path 教模型按需读 | ✅ 对齐 |
| extensions（可插拔 sources） | 无 | `ad_hoc` / `prune` | 低 ROI,future |

### A.7 推荐改进（按优先级,更新 2026-05-28）

| # | 项目 | 状态 | PR |
|---|---|---|---|
| 1 | 升级 stage_one prompt（no-op gate / outcome triage / evidence→implication / user>tool>assistant） | ✅ done | #98 |
| 2 | 升级 read_path 注入 prompt（base.md Memory 段 ~60 行,citations + verify-first） | ✅ done | #99 |
| 3 | `memory_summary.md` v1 协议字段 | ✅ done | #100 |
| 4 | Git baseline 替换 archive（lazy init / auto-commit / WorkspaceDiff） | ✅ done | #101 |
| 5 | 三件套产物（slug + summary + facts） | ✅ done | #102 |
| 6 | M10 — sub-agent tool（launch_agent） | ✅ done | #104 |
| 7 | 整合改为 sub-agent 执行（read-only 工具白名单,失败回 side-call） | ✅ done | #105 |
| 8 | Memory extensions（可插拔 sources：`ad_hoc`/`prune`） | 未排期 | — |
| 9 | 升级 consolidate prompt 到 ~880 行 INIT/INCREMENTAL 双 mode | 未排期 | — |
| 10 | rollout_summaries GC（`max_unused_days` / `last_usage`） | Phase 2 daemon 时一并 | — |
| 11 | Skills/<name>/SKILL.md 自动生成（Codex 的整合产物之一） | 未排期 | — |

Phase 1 的 "Codex 对齐" 工程已全部到位（#1–#7 完成）。当前 Phase 1 与 Codex 在
read/write 形态、协议字段、git 历史、三件套产物、sub-agent 整合执行这些核心维度上
基本对齐；剩余差距集中在 prompt 体量（~140 行 vs 569 行 stage_one、~10 行 vs 880 行
consolidation）、state DB 替代（Phase 2 多 worker 时再升级）、Skills 自动生成
（#11）、Memory extensions（#8）—— 都不挡 Phase 2 daemon 启动。

### A.8 验证来源

读源码片段：
- `~/Projects/github/codex/codex-rs/memories/README.md`（pipeline 文档）
- `~/Projects/github/codex/codex-rs/memories/write/lib.rs`（write 入口）
- `~/Projects/github/codex/codex-rs/memories/write/templates/memories/stage_one_system.md`（569 行）
- `~/Projects/github/codex/codex-rs/memories/write/templates/memories/consolidation.md`（880 行）
- `~/Projects/github/codex/codex-rs/memories/read/templates/memories/read_path.md`（130 行）
