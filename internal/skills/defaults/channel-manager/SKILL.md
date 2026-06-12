---
name: channel-manager
description: |
  Configure IM platform channels (Feishu, Weixin/WeChat, WeCom, DingTalk, Discord, Telegram) for octo.
  Guides the user through platform consoles, collects credentials, writes ~/.octo/channels.yml,
  and diagnoses connection problems.
  Trigger on: "channel setup", "setup feishu", "setup weixin", "setup wechat", "setup wecom",
  "setup dingtalk", "setup discord", "setup telegram",
  "channel config", "channel status", "channel enable", "channel disable", "channel doctor",
  "connect feishu", "connect wechat", "connect wecom", "connect dingtalk", "connect discord", "connect telegram".
  Subcommands: setup, status, enable <platform>, disable <platform>, doctor.
---

# Channel Manager Skill

Configure IM platform channels for octo. Supported platforms: `feishu`, `weixin`, `wecom`, `dingtalk`, `discord`, `telegram`.

## How channels work in octo

- Config lives in `~/.octo/channels.yml` (YAML, mode 600). Edit it directly with
  `read_file` / `write_file` — there is no hot reload.
- Adapters run inside `octo serve`, started alongside the HTTP server (skip with
  `--no-channel`). channels.yml is read once at serve startup, so after any config
  change the user must restart `octo serve`.
- Weixin login state lives separately in `~/.octo/weixin-credentials.json`, written by
  the QR-login flow this skill drives (`POST /api/channels/weixin/login` on the running serve).

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
  wecom:
    enabled: true | false
    bot_id: string            # required, intelligent robot Bot ID (starts with "aib")
    secret: string            # required, robot secret
    allowed_users: string
  discord:
    enabled: true | false
    bot_token: string         # required, from the Discord Developer Portal
    allowed_users: string
  telegram:
    enabled: true | false
    bot_token: string         # required, from @BotFather
    base_url: string          # optional, default https://api.telegram.org
    parse_mode: string        # optional, default "Markdown"; empty string disables
    allowed_users: string
```

## Command Parsing

| User says | Subcommand |
|---|---|
| `channel setup`, `setup feishu`, `setup weixin`, `setup wechat`, `setup wecom`, `setup dingtalk`, `setup discord`, `setup telegram` | setup |
| `channel status` | status |
| `channel enable <platform>` | enable |
| `channel disable <platform>` | disable |
| `channel doctor` | doctor |

---

## `status`

1. Read `~/.octo/channels.yml`. If missing or empty: "No channels configured yet. Run `/channel-manager setup` to get started." and stop.
2. Check whether the serve process (which hosts the adapters) is running:
   ```bash
   pgrep -f "octo serve" > /dev/null && echo RUNNING || echo STOPPED
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
- WeCom: show `bot_id` truncated to 12 chars. Never print `secret`.
- Discord: show whether `bot_token` is set (`token: present`). Never print the token value.
- Telegram: show whether `bot_token` is set (`token: present`). Never print the token value.

If the process is STOPPED but at least one platform is enabled, remind: "Run `octo serve` to bring the channels online." If it is RUNNING but was started before this config change, remind the user to restart it.

---

## `setup`

Ask with `ask_user_question`:
> Which platform would you like to connect?
>
> 1. Feishu (飞书)
> 2. Weixin (Personal WeChat via iLink QR login)
> 3. WeCom (企业微信 intelligent robot)
> 4. DingTalk (钉钉)
> 5. Discord
> 6. Telegram (Bot API)

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

7. Tell the user to (re)start `octo serve` and wait until the log prints `[feishu] connected to WebSocket`.
8. "In 'Events & Callbacks' (事件与回调), select 'Long Connection' (长连接) mode and save. Then click Add Event, search `im.message.receive_v1`, and add it. Reply done." Wait for "done".

#### Phase 7 — Publish

9. "Open 'Version Management & Release' (版本管理与发布), create a version (e.g. 1.0.0) and publish it. Reply done." Wait for "done".
10. "✅ Feishu channel configured. Find the bot in Feishu and send it a message."

### Weixin setup (Personal WeChat via iLink QR login)

Weixin uses a QR-code login — no app credentials needed.

1. Start the QR login flow via the serve API (the response carries the QR link):
   ```bash
   curl -s -X POST http://127.0.0.1:8080/api/channels/weixin/login
   # → {"status":"pending","qr_url":"https://…"}      (or "already_logged_in")
   ```
   Pass `-d '{"force":true}'` to re-login over existing credentials.
2. Relay the QR link to the user:
   > Open this link and scan the QR code with WeChat, then confirm login in the app:
   > `<qr_url from the response>`
3. Poll until the flow finishes (every ~3s, up to 5 minutes):
   ```bash
   curl -s http://127.0.0.1:8080/api/channels/weixin/login
   ```
   - `"status":"done"` — credentials are saved to `~/.octo/weixin-credentials.json`. Continue.
   - `"status":"pending"` with a new `qr_url` — the QR expired and was refreshed; relay the new link.
   - `"status":"failed"` — show the `error` and offer to retry from step 1.
   This agent-driven flow is the only way to log in; the web Channels panel
   intentionally has no inline QR button.
4. Enable the platform in `~/.octo/channels.yml` (preserve other platforms), then `chmod 600`:
   ```yaml
   channels:
     weixin:
       enabled: true
   ```
   The adapter reads `~/.octo/weixin-credentials.json` automatically; only set `cred_path` if the user keeps credentials elsewhere.
5. "✅ Weixin channel configured. (Re)start `octo serve`, then message the bot on WeChat."

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
6. "✅ DingTalk channel configured. Publish the app version if you haven't, (re)start `octo serve`, then message the robot in DingTalk."

### WeCom setup (企业微信 intelligent robot)

WeCom "API mode" intelligent robots connect over a WebSocket long connection — no public callback URL needed.

1. Tell the user to open <https://work.weixin.qq.com/wework_admin/frame#/aiHelper/create> (scan to log in if needed), then:
   "Scroll to the bottom of the right panel and click 'API mode creation' (API 模式创建). Reply done." Wait for "done".
2. "Click 'Add' next to 'Visible Range' (可见范围), select the top-level company node, and confirm. Reply done." Wait for "done".
3. "If the Secret is not visible, click 'Get Secret' (获取 Secret). Copy the Bot ID and Secret **before** clicking Save, and paste them here as: Bot ID: xxx, Secret: xxx". Parse the reply. Trim whitespace; the `bot_id` starts with `aib` — if the two values look swapped, swap them back.
4. "Click Save, enter a name (e.g. octo) and description, confirm, and click Save again. Reply done." Wait for "done".
5. Merge into `~/.octo/channels.yml` (preserve other platforms), then `chmod 600`:
   ```yaml
   channels:
     wecom:
       enabled: true
       bot_id: <BOT_ID>
       secret: <SECRET>
   ```
   There is no public REST endpoint to pre-validate these credentials — they are checked when the WebSocket subscribes. After `octo serve` starts, an invalid pair logs `[wecom] authentication failed`.
6. "✅ WeCom channel configured. (Re)start `octo serve`, then find the bot in the WeCom client under Contacts → Smart Bot (智能机器人) and message it."

### Discord setup

Discord requires manual portal interaction (hCaptcha gates application creation). Guide the user through the portal in one round-trip.

1. Tell the user to open <https://discord.com/developers/applications>, then give **all** of the following in a single message and collect the values with `ask_user_question` (or a plain reply):
   > 1. Click **New Application** (top-right), name it (e.g. "octo"), accept the ToS, click **Create**.
   > 2. In the left nav click **Bot**.
   > 3. Scroll to **Privileged Gateway Intents** and turn on **MESSAGE CONTENT INTENT**, then **Save Changes**.
   > 4. Scroll up, click **Reset Token** → **Yes, do it!** → **Copy**. (The token is shown only once — copy before navigating away.)
   > 5. In the left nav click **General Information** and copy the **Application ID**.
   >
   > Paste both back as one line: `token=YOUR_BOT_TOKEN app_id=YOUR_APPLICATION_ID`

   If the user chats in a non-English language, append the localized label in parens after each bolded English button name (the English label is what they physically click). Parse with tolerant matching (`token=\S+`, `app_id=\d+`); if either field is missing, re-ask with the same format reminder (up to 3 tries).
2. Validate the token:
   ```bash
   curl -s -H "Authorization: Bot <BOT_TOKEN>" \
     -H "User-Agent: DiscordBot (https://github.com/Leihb/octo-agent, 1.0)" \
     https://discord.com/api/v10/users/@me
   ```
   Success returns the bot user JSON with an `id`. A 401 means a bad token — re-ask.
3. Merge into `~/.octo/channels.yml` (preserve other platforms), then `chmod 600`:
   ```yaml
   channels:
     discord:
       enabled: true
       bot_token: <BOT_TOKEN>
   ```
4. Build the invite URL with the Application ID and tell the user to open it:
   `https://discord.com/oauth2/authorize?client_id=<APP_ID>&scope=bot&permissions=274877975552`
   > Pick your server from the dropdown → Continue → Authorize. If the dropdown is empty you don't have a server yet — open <https://discord.com/channels/@me>, click the **+** button → Create My Own, then re-open the invite link.
5. "✅ Discord channel configured. (Re)start `octo serve`, then @-mention the bot in a channel or DM it."

### Telegram setup (Bot API)

Telegram is the simplest — no browser automation, no QR. The user creates a bot via @BotFather and pastes the token.

1. Tell the user:
   > Open Telegram and chat with **@BotFather** (https://t.me/BotFather). Send `/newbot`, pick a display name and a username ending in `bot`. BotFather replies with an HTTP API token like `123456789:ABCdefGhIJKlmNoPQRsTUVwxyZ`. Paste the token here.
   >
   > Optional: if your network blocks `api.telegram.org`, also give me the base URL of your self-hosted Bot API server.

   Parse the token (matches `^\d+:[\w-]{30,}$`).
2. Validate with `getMe`:
   ```bash
   curl -s "<BASE_URL_OR_https://api.telegram.org>/bot<TOKEN>/getMe"
   ```
   Success returns `"ok":true`. `401 Unauthorized` means a wrong token — re-ask.
3. Merge into `~/.octo/channels.yml` (preserve other platforms; omit `base_url` unless the user gave one), then `chmod 600`:
   ```yaml
   channels:
     telegram:
       enabled: true
       bot_token: <TOKEN>
   ```
4. "✅ Telegram channel configured. (Re)start `octo serve`, open your bot in Telegram, and send it a message."
   > **For group chats**: disable Privacy Mode first (@BotFather → `/mybots` → Bot Settings → Group Privacy → Turn off), then **remove and re-add the bot to the group** — otherwise it cannot receive any group messages, including @-mentions.

---

## `enable` / `disable`

1. Read `~/.octo/channels.yml`. If the platform has no entry (or required fields are missing), redirect to `setup`.
2. Toggle `enabled: true|false` for that platform only; preserve every other field and platform.
3. Write back, `chmod 600 ~/.octo/channels.yml`.
4. Say "✅ `<platform>` channel enabled." / "❌ `<platform>` channel disabled.", and remind: "Restart `octo serve` for the change to take effect."

---

## `doctor`

Check each item, report ✅ / ❌ with remediation:

1. **Config file** — `~/.octo/channels.yml` exists, is valid YAML, and has mode 600 (`stat -f %Lp` on macOS, `stat -c %a` on Linux).
2. **Required fields** — for each enabled platform:
   - Feishu: `app_id`, `app_secret` non-empty.
   - Weixin: `token` non-empty, or a readable credential file (`cred_path`, else `~/.octo/weixin-credentials.json`).
   - DingTalk: `client_id`, `client_secret` non-empty.
   - WeCom: `bot_id` (starts with `aib`), `secret` non-empty.
   - Discord: `bot_token` non-empty.
   - Telegram: `bot_token` non-empty.
3. **Feishu credentials** (if enabled) — run the tenant_access_token curl from setup Phase 5; `"code":0` → ✅, else ❌ "Feishu credentials rejected — re-run setup".
4. **DingTalk credentials** (if enabled) — run the accessToken curl from setup step 4; `accessToken` present → ✅, else ❌ "DingTalk credentials invalid — re-run setup".
5. **Weixin credentials** (if enabled) — credential file exists and contains a non-empty `token` → ✅, else ❌ "Re-run the weixin QR login (web Channels panel, or `POST /api/channels/weixin/login`)".
6. **Discord credentials** (if enabled) — run the `/users/@me` curl from Discord setup step 2; an `id` in the response → ✅, else ❌ "Discord token invalid or revoked — re-run setup".
7. **Telegram credentials** (if enabled) — run the `getMe` curl from Telegram setup step 2; `"ok":true` → ✅, else ❌ "Telegram token rejected by getMe — re-run setup".
8. **WeCom credentials** (if enabled) — no public REST validation endpoint; check the `octo serve` output for `[wecom] authentication failed` → ❌ "WeCom credentials incorrect — re-run setup", or `[wecom] connected, authenticating` with no auth error → ✅.
9. **Serve process** — `pgrep -f "octo serve"`; if no enabled platform, skip; if enabled but not running, ❌ "Run `octo serve`".

---

## Security

- Always mask secrets in output — show at most the first 4 and last 4 characters.
- `~/.octo/channels.yml` must be mode 600; fix with `chmod 600` after every write.
- Never echo `app_secret`, `client_secret`, `secret`, `bot_token`, or `token` values back to the user.
