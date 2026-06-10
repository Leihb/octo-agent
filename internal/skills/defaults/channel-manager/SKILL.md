---
name: channel-manager
description: |
  Configure IM platform channels (Feishu, Weixin/WeChat, DingTalk) for octo.
  Guides the user through platform consoles, collects credentials, writes ~/.octo/channels.yml,
  and diagnoses connection problems.
  Trigger on: "channel setup", "setup feishu", "setup weixin", "setup wechat", "setup dingtalk",
  "channel config", "channel status", "channel enable", "channel disable", "channel doctor",
  "connect feishu", "connect wechat", "connect dingtalk".
  Subcommands: setup, status, enable <platform>, disable <platform>, doctor.
---

# Channel Manager Skill

Configure IM platform channels for octo. Supported platforms: `feishu`, `weixin`, `dingtalk`.

## How channels work in octo

- Config lives in `~/.octo/channels.yml` (YAML, mode 600). Edit it directly with
  `read_file` / `write_file` — there is no hot reload.
- Adapters run in a standalone foreground process: `octo channel start`. It reads
  `channels.yml` once at startup, so after any config change the user must restart it.
- Weixin login state lives separately in `~/.octo/weixin-credentials.json`, written by
  `octo channel login --platform weixin`.

`channels.yml` schema:

```yaml
channels:
  feishu:
    enabled: true | false
    app_id: string            # required
    app_secret: string        # required
    domain: string            # optional, default https://open.feishu.cn (include scheme)
    allowed_users: string     # optional, comma-separated user IDs; empty = allow all
  weixin:
    enabled: true | false
    token: string             # bot token; optional if cred_path (or the default
    cred_path: string         #   ~/.octo/weixin-credentials.json) exists
    base_url: string          # optional, default https://ilinkai.weixin.qq.com
    allowed_users: string
  dingtalk:
    enabled: true | false
    client_id: string         # required (AppKey)
    client_secret: string     # required (AppSecret)
    allowed_users: string
```

## Command Parsing

| User says | Subcommand |
|---|---|
| `channel setup`, `setup feishu`, `setup weixin`, `setup wechat`, `setup dingtalk` | setup |
| `channel status` | status |
| `channel enable feishu/weixin/dingtalk` | enable |
| `channel disable feishu/weixin/dingtalk` | disable |
| `channel doctor` | doctor |

---

## `status`

1. Read `~/.octo/channels.yml`. If missing or empty: "No channels configured yet. Run `/channel-manager setup` to get started." and stop.
2. Check whether the adapter process is running:
   ```bash
   pgrep -f "octo channel start" > /dev/null && echo RUNNING || echo STOPPED
   ```
3. Display:

```
Channel Status                       (adapter process: RUNNING/STOPPED)
─────────────────────────────────────────────────────
Platform   Enabled   Details
feishu     ✅ yes    app_id: cli_xxx…
weixin     ✅ yes    credentials: present
dingtalk   ❌ no     (not configured)
─────────────────────────────────────────────────────
```

- Feishu: show `app_id` truncated to 12 chars.
- Weixin: show whether `token` is set or a credential file exists (`cred_path`, else `~/.octo/weixin-credentials.json`). Never print the token value.
- DingTalk: show `client_id` truncated to 12 chars. Never print `client_secret`.

If the process is STOPPED but at least one platform is enabled, remind: "Run `octo channel start` to bring the channels online."

---

## `setup`

Ask with `ask_user_question`:
> Which platform would you like to connect?
>
> 1. Feishu (飞书)
> 2. Weixin (Personal WeChat via iLink QR login)
> 3. DingTalk (钉钉)

### Feishu setup

#### Phase 1 — Create the app

1. Tell the user to open <https://open.feishu.cn/app> (log in if needed), then:
   "Click 'Create Enterprise Self-Built App' (创建企业自建应用), fill in a name (e.g. octo) and description, and submit. Reply done." Wait for "done". Always create a new app — do NOT reuse existing apps.

#### Phase 2 — Enable Bot capability

2. "On the Add App Capabilities page, find the Bot (机器人) card and click Add. Reply done." Wait for "done".

#### Phase 3 — Get credentials

3. "Open 'Credentials & Basic Info' (凭证与基础信息) in the left menu, copy App ID and App Secret, and paste them here as: App ID: xxx, App Secret: xxx". Parse `app_id` and `app_secret` from the reply.

#### Phase 4 — Add message permissions

4. "Open 'Permission Management' (权限管理) → bulk import (批量导入), clear the example content, paste the JSON below, and confirm. Reply done." Wait for "done".

```json
{
  "scopes": {
    "tenant": [
      "im:message",
      "im:message.p2p_msg:readonly",
      "im:message:send_as_bot"
    ],
    "user": []
  }
}
```

#### Phase 5 — Validate and save

5. Validate the credentials:
   ```bash
   curl -s -X POST "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal" \
     -H "Content-Type: application/json" \
     -d '{"app_id":"<APP_ID>","app_secret":"<APP_SECRET>"}'
   ```
   Check for `"code":0`. On failure show the error and re-ask for credentials (up to 3 tries).
6. Merge into `~/.octo/channels.yml` (preserve other platforms), then `chmod 600 ~/.octo/channels.yml`:
   ```yaml
   channels:
     feishu:
       enabled: true
       app_id: <APP_ID>
       app_secret: <APP_SECRET>
   ```

#### Phase 6 — Configure event subscription (Long Connection)

**CRITICAL**: octo's Feishu adapter uses a WebSocket long connection, and Feishu refuses to save the long-connection event config until a client is connected. So the adapter must be running first.

7. Tell the user to run `octo channel start` in another terminal (or run it yourself in the background) and wait until the log prints `[feishu] connected to WebSocket`.
8. "In 'Events & Callbacks' (事件与回调), select 'Long Connection' (长连接) mode and save. Then click Add Event, search `im.message.receive_v1`, and add it. Reply done." Wait for "done".

#### Phase 7 — Publish

9. "Open 'Version Management & Release' (版本管理与发布), create a version (e.g. 1.0.0) and publish it. Reply done." Wait for "done".
10. "✅ Feishu channel configured. Find the bot in Feishu and send it a message."

### Weixin setup (Personal WeChat via iLink QR login)

Weixin uses a QR-code login — no app credentials needed.

1. Run the built-in login command (it prints a QR link and blocks until the user scans and confirms — use `timeout: 300`):
   ```bash
   octo channel login --platform weixin
   ```
2. As soon as the command prints the QR link, relay it to the user:
   > Open this link and scan the QR code with WeChat, then confirm login in the app:
   > `<QR URL from the command output>`
3. Wait for the command to exit:
   - Exit 0 — credentials are saved to `~/.octo/weixin-credentials.json`. Continue.
   - QR expired — the command requests a new QR automatically; relay the new link.
   - Non-zero exit — show the error and offer to retry from step 1.
4. Enable the platform in `~/.octo/channels.yml` (preserve other platforms), then `chmod 600`:
   ```yaml
   channels:
     weixin:
       enabled: true
   ```
   The adapter reads `~/.octo/weixin-credentials.json` automatically; only set `cred_path` if the user keeps credentials elsewhere.
5. "✅ Weixin channel configured. Run `octo channel start`, then message the bot on WeChat."

### DingTalk setup

1. Tell the user to open <https://open-dev.dingtalk.com/> (log in if needed), then:
   "Create an internal app (企业内部应用): Application Development → Create Application. Reply done." Wait for "done".
2. "In the app, open 'Add Application Capabilities' (添加应用能力) and add the Bot (机器人) capability. In the bot's message receiving mode, select **Stream mode** (Stream 模式) — octo connects over a WebSocket stream, no callback URL needed. Save/publish the capability. Reply done." Wait for "done".
3. "Open 'Credentials & Basic Info' (凭证与基础信息), copy Client ID (AppKey) and Client Secret (AppSecret), and paste them here as: Client ID: xxx, Client Secret: xxx". Parse the reply.
4. Validate:
   ```bash
   curl -s -X POST "https://api.dingtalk.com/v1.0/oauth2/accessToken" \
     -H "Content-Type: application/json" \
     -d '{"appKey":"<CLIENT_ID>","appSecret":"<CLIENT_SECRET>"}'
   ```
   Success returns an `accessToken` field. On failure show the error and re-ask (up to 3 tries).
5. Merge into `~/.octo/channels.yml` (preserve other platforms), then `chmod 600`:
   ```yaml
   channels:
     dingtalk:
       enabled: true
       client_id: <CLIENT_ID>
       client_secret: <CLIENT_SECRET>
   ```
6. "✅ DingTalk channel configured. Publish the app version if you haven't, run `octo channel start`, then message the robot in DingTalk."

---

## `enable` / `disable`

1. Read `~/.octo/channels.yml`. If the platform has no entry (or required fields are missing), redirect to `setup`.
2. Toggle `enabled: true|false` for that platform only; preserve every other field and platform.
3. Write back, `chmod 600 ~/.octo/channels.yml`.
4. Say "✅ `<platform>` channel enabled." / "❌ `<platform>` channel disabled.", and remind: "Restart `octo channel start` for the change to take effect."

---

## `doctor`

Check each item, report ✅ / ❌ with remediation:

1. **Config file** — `~/.octo/channels.yml` exists, is valid YAML, and has mode 600 (`stat -f %Lp` on macOS, `stat -c %a` on Linux).
2. **Required fields** — for each enabled platform:
   - Feishu: `app_id`, `app_secret` non-empty.
   - Weixin: `token` non-empty, or a readable credential file (`cred_path`, else `~/.octo/weixin-credentials.json`).
   - DingTalk: `client_id`, `client_secret` non-empty.
3. **Feishu credentials** (if enabled) — run the tenant_access_token curl from setup Phase 5; `"code":0` → ✅, else ❌ "Feishu credentials rejected — re-run setup".
4. **DingTalk credentials** (if enabled) — run the accessToken curl from setup step 4; `accessToken` present → ✅, else ❌ "DingTalk credentials invalid — re-run setup".
5. **Weixin credentials** (if enabled) — credential file exists and contains a non-empty `token` → ✅, else ❌ "Run `octo channel login --platform weixin` to log in again".
6. **Adapter process** — `pgrep -f "octo channel start"`; if no enabled platform, skip; if enabled but not running, ❌ "Run `octo channel start`".

---

## Security

- Always mask secrets in output — show at most the first 4 and last 4 characters.
- `~/.octo/channels.yml` must be mode 600; fix with `chmod 600` after every write.
- Never echo `app_secret`, `client_secret`, or `token` values back to the user.
