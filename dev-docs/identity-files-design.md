# 身份文件（soul.md / user.md）design

> openclaw / hermes 式的 persona 配置：`soul.md`（agent 身份与行为规范）+ `user.md`
> （用户个人信息）。手写、会话启动注入 system prompt。独立于自动记忆
> （`memory-design.md`）——身份文件是手写的层，自动记忆是 agent 自管的层。

## 1. 目标与范围

给 octo 一对用户级身份文件，让用户用 openclaw/hermes 的范式定义「这个 agent 是谁、为谁
服务」：

- `~/.octo/soul.md` —— agent 的身份、人格、行为规范。
- `~/.octo/user.md` —— 用户的个人信息 / 画像。

用户手写这两个文件,会话启动时注入冻结的 system prompt prefix。仅用户级
(`~/.octo/`,跨项目),符合 openclaw/hermes 的身份范式。纯 Compose 注入层,独立于 C9。

范围外:项目级 soul/user(不同 repo 不同 persona);管理/编辑 skill(可用 skill loader 做,见 §7)。

## 2. 与现有来源的分工

| 来源 | 回答 | 谁写 | 注入层 |
|---|---|---|---|
| **soul.md** | agent **是谁**、人格、行为规范 | 用户手写 | base 之后 |
| **user.md** | 用户**是谁**、个人信息 | 用户手写 | memory 之后、规则之前 |
| `~/.octo/octorules.md` / `.octorules` | **怎么工作**（全局 / 项目规则） | 用户手写 / `octo init` | user / project 规则层 |
| C9 auto-memory | 会话中**自动观察**到的偏好/事实 | agent 自动 | memory 层 |

维度不同，共存不冗余：soul=agent 身份，user=用户身份，octorules=工作规则，C9=自动记忆。

## 3. 文件格式与位置

- `~/.octo/soul.md`、`~/.octo/user.md`，自由 markdown，无强制 frontmatter。
- 缺失即跳过（像 `.octorules`，多数情况只有部分文件存在，不是错误）。
- 路径用 `os.UserHomeDir()` + `~/.octo/`，与 octorules / sessions / memory 同根。

## 4. 注入（prompt.Compose）

soul.md / user.md 由 **prompt 包内直读**（与 octorules 的 `userRulesPath` 同模式），
不新增 Compose 参数——签名已含 skills、C9 还要加 memory，单文件直读放包内更干净。

完整注入顺序（身份层 + 现有层 + C9 memory）：

```
base → soul → env → skills → memory → user.md → octorules(user) → octorules(project) → --system
```

- **soul 紧跟 base**：先确立 octo 是谁（base），再让 soul 重塑 persona / 行为风格。
- **user.md 在 memory 之后、规则之前**：用户画像，先于工作规则。
- 空层跳过；各层之间维持现有 `\n\n---\n\n` 分隔。
- 实现：prompt 包加 `soulPath` / `userProfilePath`（var，便于测试覆盖）+ `readSoul` /
  `readUserProfile`，在 `Compose` 内对应位置插入。

## 5. soul 与 base 的关系（补充，非覆盖）

`base.md`（内嵌的 octo identity + 工具/权限/安全规范）始终在 soul 之前。soul **补充**
persona 与行为风格，但**不应覆盖** base 的硬规范（read-before-write、权限门控、`edit_file`
而非 `sed -i` 等）。

- 这是**软约束**：注入顺序让 soul 在 base 之后，LLM 倾向后文优先，所以 persona 层 soul
  说了算；但 base 的安全规范靠其措辞 + 用户不在 soul 里写「忽略安全规范」来维持，不做技术
  强制。
- 若日后需要硬保证，再把 base 拆成「persona 段（soul 前）+ 不可覆盖的安全段（最末）」——
  当前不做(过度设计)。

## 6. 与 C9 的协调

- `user.md` 是用户**手写的稳定画像**（名字、角色、长期偏好）；自动记忆
  （`memory-design.md`）是 agent **自管的增长部分**。两者都注入，互补。
- 避免冗余：base.md 的记忆指引提醒 agent「不要记录 repo / CLAUDE.md / 这些身份文件里
  已有的事实」，所以 user.md 已写明的画像不会被自动记忆重复。
- 注入顺序里 `user.md` 紧跟 `memory`，二者相邻，便于模型把"手写画像 + 自动观察"作为一组
  用户上下文理解。

## 7. 管理/编辑 skill(后续)

可用 skill loader 做一个 skill(如 `/identity`)帮用户查看/编辑 soul.md 与 user.md——交互式
补全字段、校验、给模板。当前只做手写 + 注入。

## 8. 测试(stdlib,无外部框架)

- `prompt`：soul 在 base 之后、user.md 在 memory/skills 之后且在 octorules 之前的顺序；
  两文件缺失各自跳过、不留空分隔；与 octorules 共存时层次正确。
