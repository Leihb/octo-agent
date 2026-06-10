---
name: update-config
description: Update octo's persisted configuration files — ~/.octo/config.yaml (provider, model, etc.), ~/.octo/mcp.json (MCP servers), ~/.octo/permissions.yml (permission rules), and ~/.octo/channels.yml (IM platform bridges). Use when the user wants to change any octo setting without running the setup wizard manually.
---

# Update octo configuration

octo has four persisted configuration files. Read with `read_file`, modify, and write back with `write_file`.

| File | Format | What it controls |
|------|--------|------------------|
| `~/.octo/config.yaml` | YAML | Provider, model, base URL, permission mode, coauthor, reasoning |
| `~/.octo/mcp.json` | JSON | MCP servers (stdio and HTTP) |
| `~/.octo/permissions.yml` | YAML | Custom permission rules per-tool |
| `~/.octo/channels.yml` | YAML | IM platform bridges (Weixin iLink, DingTalk, Feishu) |

## Rules (all files)

- **Always read first** — use `read_file` to see current values before changing anything.
- **Preserve existing values** — only change the field(s) the user asked for; leave everything else untouched.
- **Validate before writing** — reject invalid values (unknown provider, malformed URL, bad YAML, etc.).
- **Never expose secrets** — if a file contains API keys or tokens, do not echo them back to the user.
- **Confirm with the user** before writing if the change would overwrite an existing non-default value.

---

## 1. ~/.octo/config.yaml

### Schema

```yaml
provider: anthropic | openai
model: string (model name, e.g. claude-sonnet-4-20250514)
base_url: string (optional custom endpoint)
permission_mode: string (optional: strict | confirm | auto)
coauthor: true | false
show_reasoning: true | false
reasoning_effort: low | medium | high | ""
```

- `provider`: only `"anthropic"` or `"openai"` are valid.
- `permission_mode`: `"strict"` (block risky ops), `"confirm"` (ask before executing), `"auto"` (run without asking). Omit to use the built-in default.
- `coauthor`: when `true`, octo appends `Co-authored-by: octo-agent <no-reply@octo-agent.dev>` to every git commit it writes.
- `show_reasoning`: when `true`, reasoning/thinking traces are streamed to the terminal.
- `reasoning_effort`: `""` (empty) means off; otherwise `"low"`, `"medium"`, or `"high"`.

### Steps

1. Read `~/.octo/config.yaml`. If missing, treat as empty document.
2. Ask the user if ambiguous (e.g. "switch provider" → ask which one).
3. Validate: reject unknown providers, malformed URLs, unknown permission modes, invalid reasoning effort.
4. Merge the new value into the existing object.
5. Write back with `write_file`, then `chmod 600 ~/.octo/config.yaml`.
6. Confirm what changed (excluding `api_key`).

---

## 2. ~/.octo/mcp.json

### Schema

```json
{
  "mcpServers": {
    "server-name": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "env": {"FOO": "bar"}
    },
    "remote-api": {
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer ..."},
      "auth": "oauth"
    }
  }
}
```

- Each server entry is either **stdio** (`command` set) or **HTTP** (`url` set). Both set is invalid.
- `auth`: `"oauth"` for device-flow OAuth (HTTP only), or omit.
- `disabled`: `true` to keep the config but skip loading.

### Steps

1. Read `~/.octo/mcp.json`. If missing, treat as `{"mcpServers": {}}`.
2. Determine the operation:
   - **Add** a new server — ask for name, type (stdio/HTTP), and required fields.
   - **Edit** an existing server — show current config, ask which fields to change.
   - **Remove** a server — confirm, then delete the entry.
   - **Enable/disable** — toggle the `disabled` flag.
3. Validate:
   - Reject stdio+HTTP hybrid entries.
   - Reject unknown `auth` values.
   - Reject malformed `url`.
4. Merge into the existing `mcpServers` object.
5. Pretty-print JSON with 2-space indentation and trailing newline.
6. Write back with `write_file`, then `chmod 600 ~/.octo/mcp.json`.
7. Confirm and remind: changes take effect on next `octo chat` start.

---

## 3. ~/.octo/permissions.yml

### Schema

```yaml
tool_name:
  - allow: { pattern: "substring" }
  - deny:  { path: ["**/secret/**"] }
  - ask:   { hostname: ["*.internal.com"] }
```

- Top-level keys are **tool names** (`terminal`, `write_file`, `edit_file`, `read_file`, `web_fetch`, etc.).
- Each rule has exactly one of `allow` / `deny` / `ask`.
- Rule axes:
  - `pattern` — substring match for `terminal` commands (case-sensitive)
  - `path` — glob match for file tools; `**` = any depth; `$CWD` = working directory
  - `hostname` — glob match for `web_fetch` hosts; `*` = one DNS label
- User rules **fully replace** the default rule list per-tool (not append).

### Steps

1. Read `~/.octo/permissions.yml`. If missing, the user has no custom rules yet.
2. Determine the operation:
   - **Add a rule** — ask which tool, decision (allow/deny/ask), and matching criteria.
   - **Remove a rule** — show current rules for the tool, ask which to delete.
   - **Show current rules** — pretty-print the file (or say "using defaults" if missing).
3. Validate:
   - Reject unknown tool names.
   - Reject rules with multiple or missing decision clauses.
   - Warn if a new rule would make the tool unusable (e.g. deny everything).
4. Write back with `write_file`, then `chmod 600 ~/.octo/permissions.yml`.
5. Confirm and remind: changes are live immediately in the current session.

---

## Example interactions

**User:** "use openai as my default"
→ Read config.yaml, see `provider: anthropic`
→ Suggest: "Switch provider to openai and model to gpt-4.1?"
→ User confirms → Write updated config.yaml, chmod 600

**User:** "add a filesystem MCP server"
→ Read mcp.json, see current servers
→ Ask: name, command, args → Validate → Merge → Write back

**User:** "allow docker commands"
→ Read permissions.yml, see current rules (or none)
→ Add: `terminal: [{ allow: { pattern: "docker " } }]`
→ Validate → Write back

**User:** "show my permission rules"
→ Read permissions.yml → Pretty-print, or say "using defaults" if missing

**User:** "enable weixin channel"
→ Read channels.yml, see `weixin: {enabled: false}`
→ Toggle enabled to true, keep all other fields untouched
→ Write back, chmod 600

---

## 4. ~/.octo/channels.yml

### Schema

```yaml
channels:
  weixin:
    enabled: true | false
    base_url: string (optional, default https://ilinkai.weixin.qq.com)
    token: string (bot token from iLink login)
    cred_path: string (optional, path to credentials JSON file)
    timeout_sec: integer (optional, HTTP timeout)
    allowed_users: string (optional, comma-separated user IDs)
  dingtalk:
    enabled: true | false
    client_id: string (app key / client ID)
    client_secret: string (app secret)
    allowed_users: string (optional, comma-separated user IDs)
  feishu:
    enabled: true | false
    app_id: string
    app_secret: string
    domain: string (optional, e.g. "open.feishu.cn")
    allowed_users: string (optional, comma-separated user IDs)
  wecom:
    enabled: true | false
    bot_id: string (intelligent robot Bot ID, starts with "aib")
    secret: string (robot secret)
    allowed_users: string (optional, comma-separated user IDs)
  discord:
    enabled: true | false
    bot_token: string (bot token from the Discord Developer Portal)
    allowed_users: string (optional, comma-separated user IDs)
  telegram:
    enabled: true | false
    bot_token: string (from @BotFather)
    base_url: string (optional, default https://api.telegram.org; override for self-hosted Bot API)
    parse_mode: string (optional, default "Markdown"; empty string disables formatting)
    allowed_users: string (optional, comma-separated user IDs)
```

- `enabled`: `true` to start the bridge on the next `octo serve` start; `false` to keep the config but skip loading.
- `allowed_users`: optional allow-list; if set, only listed users can interact with the bot. Empty = allow all.

### Supported platforms

| Platform | Required fields | Optional fields |
|----------|-----------------|-----------------|
| `weixin` | `token` (or `cred_path`) | `base_url`, `timeout_sec`, `allowed_users` |
| `dingtalk` | `client_id`, `client_secret` | `allowed_users` |
| `feishu` | `app_id`, `app_secret` | `domain`, `allowed_users` |
| `wecom` | `bot_id`, `secret` | `allowed_users` |
| `discord` | `bot_token` | `allowed_users` |
| `telegram` | `bot_token` | `base_url`, `parse_mode`, `allowed_users` |

### Steps

1. Read `~/.octo/channels.yml`. If missing, treat as `channels: {}`.
2. Determine the operation:
   - **Enable/disable** a platform — toggle `enabled`; preserve all other fields.
   - **Add** a new platform — ask which platform, then collect required fields.
   - **Edit** an existing platform — show current config (hide secrets), ask which fields to change.
   - **Remove** a platform — confirm, then delete the entire entry under `channels`.
3. Validate:
   - Reject unknown platform names.
   - Reject missing required fields for the chosen platform.
   - Reject malformed URLs in `base_url`.
4. Merge the new value into the existing `channels` object; preserve fields the user did not touch.
5. Write back with `write_file`, then `chmod 600 ~/.octo/channels.yml`.
6. Confirm what changed and remind: channel changes take effect on the next `octo serve` start — restart it if it's already running.
