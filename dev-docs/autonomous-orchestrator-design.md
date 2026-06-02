# 自主编排器设计(Conductor)

把一个大目标(如「把某 TS 代码库整体翻译成 Go」)交给 octo-agent **无人值守**地跑完。
`octo conduct` / `/conduct` 是 octo 唯一的编排入口。

## 设计动机:扇出一次的 DAG 为什么不行

conductor 取代了早期的一次性 DAG 编排器(曾叫 `/goal`,已移除)。那种「开工前一次性拍定、
扇出一次、彼此隔离且互相看不见结果的无状态子 agent DAG」有五个结构性缺陷,conductor 逐条规避:

1. **子任务零共享状态**。`Executor.Execute(ctx, description)` 唯一输入是 planner 最初写下的
   一行静态描述;上游 `sub.Result` 存了但从不拼进下游 prompt。下游 worker 对上游产出是瞎的。
2. **计划开工前冻死**。`PlanTask` 只跑一次,输入只有 `.octorules` + 截断历史 + goal(看不到代码),
   产出 ≤12 个一行描述后 DAG 不可变,子任务不能再拆/新增/改依赖。
3. **stop-on-fail**:任一子任务出错,全盘 Failed。
4. **max-turns 当硬失败**:`sub_agent.go` 把 `StopReasonMaxTurns` 转成 error 且丢弃 partial reply,
   叠加 stop-on-fail → 一个雄心子任务撞 100 轮就让整个目标流产。
5. **同目录并发写**:`dispatchBatch` 并发跑的 worker 在同一工作区互相踩,无 worktree 隔离。

## 核心转变

三条原则,逐一对应上面的缺陷:

1. **计划是磁盘上的活文件,不是内存里的不可变 DAG**(治 #2)。Conductor 每轮读它、改它。
2. **子任务之间通过磁盘上的共享文档传递状态**(治 #1)。约定/决策/产物落盘,每个 worker
   开工先读、收工追加——module B 的 worker 由此知道 module A 的 worker 把包叫什么、类型怎么命名。
3. **「完成」可由可配置的验证闸门判定,而非盲信 agent 自述**(治 #3/#4)。闸门三选一:
   默认**不验证**(信任 worker 自报完成,适合无客观 oracle 的通用/非代码目标)、**LLM judge**
   (`--verify`,让模型判产出是否达标)、**shell 闸门**(`--verify-cmd`,如 `go build && go test`,
   代码任务最强)。max-turns 是检查点不是失败:保存 partial、记日志、下一轮 `Continue` 续跑。
   失败在 worktree 内被隔离,绿灯才合并(治 #5)。

## 架构

```
Conductor(长程循环 · 单线程编排 · 唯一写 LEDGER 的人)
  │  ① 读 LEDGER + CONVENTIONS + 上轮验证结果
  │  ② 决定下一个工作单元 / 必要时重规划
  │  ③ 派发 Worker ──────────────► agentSpawner.Spawn / Continue(复用现有)
  │       (隔离 worktree + 喂全上下文)        │
  │  ④ Verifier 闸门 ◄───────────────────────┘
  │       (默认无 / LLM judge / shell 如 go build·test —— 可配置)
  │  ⑤ Integrator:绿灯才把 worktree 合回 trunk,处理冲突
  │  ⑥ 更新 LEDGER + JOURNAL,压缩上下文,回到 ①
  ▼
持久层(磁盘,崩溃可恢复):  .octo/conductor/<id>/
    LEDGER.md        活的任务台账
    CONVENTIONS.md   命名/包布局/类型映射/idiom —— worker 间的共享大脑
    JOURNAL.md       append-only 审计 + resume 依据
```

编排是**单线程**的:同一时刻只有 Conductor 在改 LEDGER 和合并 trunk。Worker 可有界并行,
但只对「可证明独立」的叶子,且各自在独立 worktree(见「并行策略」)。重一致性优先于重并发。

## 持久状态:磁盘三件套

这是修 `/goal` #1/#2 的关键——把「共享记忆」外置到磁盘,不依赖会话上下文。

- **LEDGER.md(任务台账)**:活文件,Conductor 每轮可重写。每个单元有:id、描述、状态
  (pending/in-progress/blocked/verifying/done/abandoned)、`blocked_by`、worktree 分支、
  最近一次验证结果摘要、所属里程碑。**新发现的工作随时追加**——这就是「边做边规划」。
- **CONVENTIONS.md(决策与约定)**:命名规范、包/目录布局、TS→Go 类型映射表、错误处理 idiom、
  已定的公共接口签名。**worker 开工前必读、收工把新决策追加进去**。Conductor 负责在追加产生冲突
  时(两个 worker 对同一约定给了不同答案)裁决并归一。
- **JOURNAL.md**:append-only。每个 worker 干了什么、验证结果、合并 commit。用于调试和 resume。

崩溃恢复 = 重启时读这三个文件即可续跑,无需会话历史。这点 `/goal` 的 `taskgraph.Store` 已经证明
可行(两次 fsync/批次),直接复用其持久化模式,只是数据模型从不可变 DAG 换成活台账。

## Conductor 循环

```
load LEDGER, CONVENTIONS from disk
loop:
    if budget exhausted or stalled(K iters no progress): break → 产出报告
    state := scan(repo) + LEDGER + last verification
    if 现实与计划偏离(发现新依赖/单元过大/约定冲突):
        replan: 改写 LEDGER(拆单元 / 加单元 / 调依赖)   # 治 #2
    unit := pick_ready_unit(LEDGER)                       # 无 ready 且全 done → 收尾
    if unit == nil: 跑全局验证;全绿 → done;否则把缺口写成新单元
    ctx := build_worker_context(unit, CONVENTIONS, upstream_results)   # 治 #1
    branch, wt := worktree.create(unit.id)
    result := dispatch_worker(ctx, wt)        # Spawn;若续跑用 Continue
    if result.max_turns:                      # 治 #4
        journal("checkpoint"); unit.status = in-progress(续跑标记); continue
    verdict := verifier.run(wt)               # 治 #3/#4:客观闸门
    if verdict.green:
        integrator.merge(branch)              # 治 #5:绿灯才合
        unit.status = done; append CONVENTIONS deltas
    else:
        unit.attempts++; 把失败详情作为下一轮 worker 上下文
        if attempts > N: unit.status = blocked(需人工/换策略)
    persist(LEDGER, JOURNAL); compact_if_needed()
```

关键点:**没有 stop-on-fail**。单个单元失败只影响该单元(进 blocked/重试),不拖垮全局。
**max-turns 不丢工作**——partial 留在 worktree,下一轮用 `Continue` 接着干(现有 `agentSpawner.Continue`
在 `/goal` 路径里完全没被用上,这里正好用上)。

## Worker:喂全上下文 + worktree 隔离

每个 worker 仍是现有 `agentSpawner` 派生的隔离 child(复用 `Spawn`/`Continue`),但 Conductor
给它的 prompt 不再是一行静态描述,而是组装好的:

- 本单元目标 + 验收标准
- CONVENTIONS.md 全文(或相关切片)
- 上游单元的产物摘要 / 关键决策(从 LEDGER 取 `blocked_by` 单元的 result)
- 它的工作区是一个独立 git worktree;收工要求:把新决策追加到 CONVENTIONS、产出可编译的代码

worker 内部仍可用全套工具(terminal/编辑/读),但 `launch_agent`/`send_message` 照旧被
`filterChildTools` 砍掉——递归分解由 Conductor 通过重规划完成,而不是 worker 自己 fork。

## Verifier:可插拔验证闸门

```go
type Verifier interface {
    // target 带上 Goal/UnitDesc/Result/Workdir/Global —— shell 闸门只看 Workdir,
    // LLM judge 据 Goal/UnitDesc/Result 判断,返回是否通过 + 给 worker 看的失败摘要。
    Verify(ctx context.Context, target VerifyTarget) (Verdict, error)
}
type Verdict struct { Green bool; Summary string }
```

三种实现,运行时三选一:

- **`NopVerifier`(默认)**:永远绿,worker 自报完成即 done。适合无客观 oracle 的通用/非代码目标。
- **LLM judge(`--verify`)**:side-call 让模型判 worker 产出是否满足 unit 目标,默认偏严(证据不足判 fail)。
  通用——文档、设计、研究、代码都能判;但对代码,shell 闸门严格更强。
- **`CmdVerifier`(`--verify-cmd "<cmd>"`)**:跑 shell 命令,全 0 退出才绿。如 `go build && go test`、
  `make ci`、`pytest`。对「能不能编译/测试过」这种可判定问题,这是最强闸门。

红灯的 `Summary`(编译错误 / judge 的理由)直接喂回 worker 下一轮。无论哪种闸门,单元在绿之前永远不是
done、worktree 也不会合并——这把 `/goal` 的「agent 自述完成但其实没做到」从盲信变成可选的、可升级的校验。
全局收尾闸门(`Global=true`)只对 shell 闸门有意义(在 trunk 根重跑);Nop / judge 直接放行。

## 并行策略

默认**串行**。只对 LEDGER 里 `blocked_by` 互不相交、且预判文件集不重叠的叶子做**有界并行**
(`maxConcurrent`,如 2–4),每个独立 worktree。合并回 trunk 始终由 Conductor **串行**做,
逐个 `verify→merge`,后一个合并前先 rebase 到最新 trunk,冲突让 worker 解。
一致性优先:宁可慢,不要并发写脏 trunk。

## 无人值守护栏

「全程无人值守」要求这些必须到位,否则它要么烧钱空转、要么把 trunk 搞坏:

- **预算**:token / 迭代数 / wall-clock 三选一硬上限,到顶停下并产出报告。
- **停滞检测**:连续 K 轮 LEDGER 无 done 增量(或同一单元 attempts 超限)→ 判定卡死,停 + 报告,
  不无限重试。
- **失败 containment**:坏 worker 的产物锁在 worktree;只有 Verifier 绿灯才合 trunk。trunk 始终可编译。
- **崩溃恢复**:LEDGER/JOURNAL 落盘,`octo conduct resume <id>` 读盘续跑。
- **终止条件**(三者之一):台账全 done 且全局闸门绿(成功) / 预算耗尽 / 停滞。任一都产出
  结构化报告(完成了什么、blocked 在哪、trunk 现状、下一步建议)。

## 复用现有 vs 新增

| 复用 | 位置 |
|------|------|
| worker 派生 + 续跑 | `cmd/octo/sub_agent.go` 的 `agentSpawner.Spawn/Continue` |
| 隔离 child(共享 Sender/System、独立 History、防递归) | 同上 |
| 流式事件 → 实时面板 | `tools.SubAgentEventSink` / `RunStream` handler |
| 规划 side-call | `internal/agent` 的 `PlanTask` |

| 组成 | 说明 |
|------|------|
| `internal/conductor`(Conductor 循环 + LEDGER 模型 + 原子 Store) | 单线程编排 + 活台账 |
| `Verifier` 接口 + `CmdVerifier`(build/vet/test) | 客观闸门 |
| `gitWorktrees`(create/commit/merge/cleanup) | git worktree 隔离 + 串行合并 |
| 共享文档约定(LEDGER/CONVENTIONS/JOURNAL 读写) | worker 间共享记忆 |
| `octo conduct` CLI + `/conduct` TUI | headless + 交互两条入口 |

## 对 CC TS→Go 用例具体长什么样

- **Verifier** = `go build ./... && go vet ./... && go test ./...`。
- **CONVENTIONS.md** 一开始由 Conductor 播种:目标 module path、目录布局、TS→Go 类型映射约定
  (interface→struct/interface、Promise→error 返回、class→struct+方法…),worker 逐步补全成一张活的映射表。
- **工作单元** = 一个 TS 模块或一组紧耦合文件 → 一个 Go 包。`blocked_by` 反映真实编译依赖。
- **重规划**触发点:worker 在翻某模块时发现它依赖一个尚未建模的底层模块 → Conductor 把该底层模块
  作为新单元插到台账前面。
- **续跑**:大模块一轮翻不完(撞 100 轮)→ partial 留 worktree,下一轮 `Continue` 接着翻,而非重头来。

## 渐进交付(够用就好)

不要一次造齐。建议三段:

- **Phase 1 — 最小可用单线程**:Conductor 循环 + LEDGER + Verifier + `Continue` 续跑。
  去掉 stop-on-fail 和 max-turns-即死。**不做 worktree、不做并行**(worker 直接在主工作区干,
  靠 Verifier + 串行保证)。这一步就能让大任务「至少跑得下去、不会一个错就全崩」。
- **Phase 2 — 隔离与并行**:WorktreeManager + 有界并行 + 串行合并 + 冲突处理。
- **Phase 3 — 完整无人值守**:自动重规划 + 停滞检测 + 预算/终止/恢复全套护栏 + 结构化报告。

## 验收标准

定义「完成」长什么样,先于写代码:

- **Phase 1 通过**:给定一个含 ~10 个有依赖模块的中型 TS 子集,`octo conduct "<goal>"` 能在无人干预下
  跑到「全局 `go build` 绿」,且单个模块翻译失败时其余模块不受影响、失败单元可 `Continue` 续跑成功。
- **Phase 2 通过**:两个独立模块在各自 worktree 并行翻译,串行合并后 trunk 仍 `go build` 绿,无互相覆盖。
- **Phase 3 通过**:启动后无人值守跑到三个终止条件之一,产出报告;中途 `kill -9` 后 `resume` 能续跑不重做已完成单元。
