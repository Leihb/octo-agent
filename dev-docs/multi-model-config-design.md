# Multi-Model Configuration

`~/.octo/config.yml` holds a list of named model configurations instead of a
single provider/model pair. One entry is the default for new sessions; each
web session can switch to a different entry without affecting others; an
optional lite entry serves history compaction. The file is renamed from
`config.yaml` to `config.yml`, matching `permissions.yml` and `channels.yml`.

This makes the existing web settings panel real: `settings.js` already renders
multiple model cards with add/remove/set-default actions (a contract inherited
from the Ruby frontend), but the Go backend collapses everything onto the
single top-level config — saving a second card silently overwrites the first,
and `POST /api/config/models/{id}/default` is a no-op.

## Goals

- Persist multiple named model configs, each with its own provider, base URL,
  API key, and reasoning settings.
- Make the settings panel's multi-card CRUD and set-default work against real
  storage.
- Per-session model selection: `POST /api/sessions` can name a config entry;
  `PATCH /api/sessions/{id}/model` switches that session only, not the global
  default.
- Give the lite model its first runtime consumer: compaction summarisation
  runs on the lite entry when one is configured.
- Rename the config file to `config.yml` with a transparent migration.

## Non-goals

- **Mid-turn model switching.** A model change takes effect on the next turn.
- **Per-sub-agent model routing.** Sub-agents inherit the spawning session's
  sender, unchanged.
- **Failover / load balancing** between entries. One session, one model.
- **IM / cron model selection.** Channel and cron sessions use the default
  entry; per-session switching is a web-UI affordance this round.
- **Merging config files.** `mcp.json`, `permissions.yml`, `channels.yml`
  stay separate (different writers, different lifecycles).

## Config schema

```yaml
# ~/.octo/config.yml
models:
  - name: main                 # unique handle; API id and CLI --model target
    provider: anthropic
    model: claude-fable-5
    base_url: https://api.anthropic.com   # optional; empty = vendor default
    api_key: sk-...                       # optional; env var preferred
    reasoning_effort: high               # optional, per-model
    show_reasoning: true                  # optional, per-model
  - name: kimi
    provider: kimi
    model: kimi-k2.6

default_model: main      # entry used when nothing else selects one
lite_model: ""           # optional entry name for cheap internal calls

# global fields, semantics unchanged:
permission_mode: interactive
access_key: ...
coauthor: true
compact_auto_pct: 75
compact_batch_threshold: 0
tools:
  ...
```

Decisions:

- **`name` is the identity.** Unique, user-visible, stable across edits; the
  HTTP API uses it as `{id}` and the CLI accepts it for `--model`. The server
  generates one from the model string on create when the panel doesn't supply
  it (deduplicated with a numeric suffix).
- **`reasoning_effort` / `show_reasoning` move per-entry.** A reasoning model
  and a non-reasoning model need different settings; `app.SenderOptions`
  already carries both per-sender (`internal/app/sender.go`). The legacy
  top-level values migrate into the synthesized entry.
- **`permission_mode` stays global.** The current `saveModelRequest` mixes it
  into the model card; the backend keeps applying it globally and the panel
  field keeps working unchanged.
- **default/lite are references, not entry types.** The panel's
  `type: "default" | "lite"` badges are derived by the server from
  `default_model` / `lite_model` when building `GET /api/config`; nothing is
  stored per entry.

## File rename and migration

- `config.Path()` returns `~/.octo/config.yml`. `Load()` reads it; when it
  does not exist, `Load()` falls back to legacy `~/.octo/config.yaml`.
- A legacy file (top-level `provider`/`model`/`base_url`/`api_key`/
  `reasoning_effort`/`show_reasoning`, no `models:` block) is normalised in
  memory: those fields become `models[0]` with `name: default`, and
  `default_model: default`. Global fields pass through.
- `Save()` always writes the new schema to `config.yml`. When a legacy
  `config.yaml` exists at that moment it is renamed to `config.yaml.bak` —
  non-destructive, and it can never shadow the new file because `config.yml`
  wins the read order.
- Help text and the `octo config` wizard messages switch to the new path
  (`cmd/octo/help.go`, `cmd/octo/config.go`, `cmd/octo/chat.go`,
  `cmd/octo/init.go`).

`config.Config` keeps the legacy top-level fields as yaml-tagged members so
old files unmarshal, but they are write-once-migration inputs; new saves emit
only the `models:` schema. A `cfg.DefaultModel()` accessor returns the
resolved default entry so the many existing `cfg.Model` / `cfg.Provider`
readers (`internal/server/server.go`, `cmd/octo/chat.go`,
`internal/server/onboard_config_handlers.go` onboarding detection) migrate
mechanically.

## Resolution precedence (CLI)

`resolveProviderModel` (`cmd/octo/chat.go`) gains one rule, the rest is
today's order:

1. `--model <value>`: when the value equals a config entry **name**, the whole
   entry applies — provider, model, base URL, API key, reasoning settings.
   Otherwise the value is a raw model string under the separately-resolved
   provider, exactly as today.
2. Env vars (`OCTO_PROVIDER`, `<PROVIDER>_MODEL`, key env vars) unchanged.
3. Otherwise the `default_model` entry.

## Server: per-session sender binding

Today `internal/server/server.go` builds one global sender at startup
(`resolveProviderAndModel` → `app.NewSender`); `handleCreateSession`
(`internal/server/handlers.go`) accepts a raw model string but it rides the
global sender, and `handleUpdateSessionModel` rewrites the global config while
the UI presents it as per-session.

Design:

- `agent.Session` (`internal/agent/session.go`) gains
  `ModelConfig string \`json:"model_config,omitempty"\`` — the config entry
  name the session is bound to. Empty means "the default entry at turn time",
  which is also the value for every pre-existing session on disk.
- The server keeps a `senderCache map[string]agent.Sender` keyed by entry
  name, built lazily via `app.NewSender` from the entry's fields. Any mutation
  through `/api/config/models*` invalidates the whole cache; the next turn
  rebuilds. The cache replaces the single `s.sender`/`s.model` pair as the
  source for turn execution; the startup-time resolution remains only as the
  onboarding-mode probe (no key → onboarding, unchanged).
- The turn path resolves session → entry name → (sender, model string,
  reasoning options) at the start of each turn, so a switch mid-session
  applies from the next turn.
- `PATCH /api/sessions/{id}/model`: `model_id` is an entry name; the handler
  sets `session.ModelConfig` and persists the session. No global config write.
  Unknown name → 400 listing valid names.
- `POST /api/sessions`: `req.Model` naming a config entry binds the session to
  it; a non-matching value keeps today's raw-model-string behaviour on the
  default entry's provider.

## Config CRUD API

All five routes exist (`internal/server/server.go:406-409`); the handlers in
`internal/server/onboard_config_handlers.go` become real:

| Route | Behaviour |
|---|---|
| `GET /api/config` | Full `models` list (id = name, keys masked via `maskKey`), real `default_model_idx`, `type` derived from `default_model`/`lite_model`. |
| `POST /api/config/models` | Append a new entry; generate a unique name when absent; returns the id. |
| `PATCH /api/config/models/{id}` | Update the named entry. Empty `api_key` in the request keeps the stored key (the panel echoes masked keys). |
| `DELETE /api/config/models/{id}` | Remove the entry. Deleting the default reassigns `default_model` to the first remaining entry; deleting the lite entry clears `lite_model`. Sessions bound to the deleted name fall back to default on their next turn. |
| `POST /api/config/models/{id}/default` | Set `default_model`. |
| `POST /api/config/models/{id}/lite` | New: set (or clear, with empty id semantics via `DELETE`-style toggle) `lite_model`. |

`POST /api/config/test` is already entry-shaped and stays as is.

## Lite model → compaction

`Agent` (`internal/agent/agent.go`) gains an optional pair set together:

```go
// LiteSender/LiteModel, when both set, run history summarisation on a
// cheaper model. Unset falls back to Sender/Model.
LiteSender Sender
LiteModel  string
```

`summarize` (`internal/agent/compaction.go`) sends on the lite pair when
present — including its head-popping overflow protection, which must then size
against `contextWindow(LiteModel)`, and the streaming path, which
type-asserts the lite sender. On a lite-call error it retries once on the
primary sender before giving up — a misconfigured lite entry must not break compaction, whose
failure already aborts the turn's compact pass. Both `maybeCompact` and the
overflow compact path go through `summarize`, so one change covers both;
micro-compaction is string-level and unaffected.

Wiring: each transport that constructs an `Agent` resolves `lite_model` to an
entry, builds its sender through the same cache/`app.NewSender` path, and sets
the pair.

**Implicit lite.** When no `lite_model` entry is configured, the vendor's
registry `LiteModel` (e.g. deepseek → `deepseek-v4-flash`,
anthropic → `claude-haiku-4-5`) serves as the lite model, riding the
session's **own sender** — same endpoint, key, and prompt-cache routing, so
no extra credentials and, where the backend caches by shared prefix, the
summarisation call can hit the cache the conversation already built.
`app.ImplicitLiteModel(provider, model, baseURL)` resolves it and returns
nothing when the vendor is unknown or has no `LiteModel`, when the primary
model already is the lite model, or when the base URL points off the
vendor's own endpoints — a custom endpoint is a different backend wearing a
compatible protocol, and its catalogue won't include the vendor's lite
model (the primary-sender retry would catch the failure, but there's no
point sending a doomed call). An explicitly configured `lite_model` entry
always wins. For a session bound to a config entry, the implicit lookup
uses that entry's vendor and endpoint.

## Frontend

`internal/server/static/settings.js` already implements the multi-card panel
(add, remove, per-card test, set-default, default/lite badges) against this
exact API shape, so it needs only:

- id-based addressing for PATCH/DELETE/set-default (it currently indexes by
  position; ids arrive in `GET /api/config`),
- a set-as-lite action symmetric to the existing set-default button,
- the session info bar's model picker listing entries from `GET /api/config`
  and sending the entry name to `PATCH /api/sessions/{id}/model`.

## Compatibility

- **Old `config.yaml` files** load unchanged (legacy fallback + in-memory
  normalisation); the first save migrates to `config.yml` and parks the old
  file as `config.yaml.bak`.
- **Old session JSON** has no `model_config`; empty means default entry —
  exactly the previous behaviour.
- **API shapes are unchanged** — the frontend contract was multi-model all
  along; only the backend semantics catch up. The one addition is the
  `/lite` route.
- **IM bridge, cron, headless `octo chat`** resolve through the default entry
  with no behavioural change until the user adds entries.

## Implementation split

Three PRs, each independently shippable:

1. **Config schema + rename + CRUD** — `internal/config`, migration,
   `onboard_config_handlers.go`, settings panel id-addressing. After this the
   panel stores multiple entries and set-default works; sessions still all use
   the default entry.
2. **Per-session binding** — session field, sender cache, create/switch
   handlers, session info bar picker.
3. **Lite compaction** — agent fields, `summarize` fallback, `/lite` route and
   panel action, transport wiring.

## Testing

- `internal/config`: legacy-yaml → yml migration (load, save, `.bak` rename),
  name uniqueness, default/lite reference repair on delete.
- `internal/server`: CRUD handler tests against a temp home; per-session
  switch persists `model_config` and leaves global config untouched; create
  with entry name vs raw model string.
- `internal/agent`: compaction uses the lite sender when set, falls back to
  the primary on lite error (fake senders, no network — per `.octorules`).
