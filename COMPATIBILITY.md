# Compatibility

octo follows [Semantic Versioning](https://semver.org). This document
declares which surfaces that promise covers: what you can build on, what is
documented but may move, and what is internal. If a surface isn't listed
here, assume Internal.

**Status:** this policy is in force now (0.19). Until v1.0.0, a minor
release may still break a Stable surface — always with a **Breaking** callout
in the [CHANGELOG](CHANGELOG.md). From v1.0.0 the tiers below are a hard
commitment: breaking a Stable surface requires a major version.

## What the versions mean

- **Major** — a Stable surface changed incompatibly (after the deprecation
  window below).
- **Minor** — new features; additive changes to Stable surfaces; any change
  to Best-effort surfaces.
- **Patch** — fixes; no surface changes.

## Tiers

- **Stable** — covered by SemVer. Changes are additive in minors; renames,
  removals, and semantic changes happen only in a major, after a
  deprecation window.
- **Best-effort** — documented and real, but not covered. Changes land in
  minors and are called out in the CHANGELOG.
- **Internal** — implementation detail. May change in any release without
  notice.

## Stable surfaces

### Configuration files

| File | Format | The promise |
|---|---|---|
| `~/.octo/config.yml` | YAML, mode 0600 | Recognized fields keep their name and meaning (`models` list entries, `default_model`, `lite_model`, `permission_mode`, `access_key`, `tools.*`, …). New fields are optional with working defaults. Unknown fields are ignored, never an error. |
| `~/.octo/permissions.yml` | YAML | The rule format: top-level keys are tool names, each holding an ordered list of `allow:` / `deny:` / `ask:` rules with `pattern` (terminal substring; a pattern ending in `/` or `~` anchors to an argument boundary), `hostname` (glob list), or `path` (glob list, `$CWD` expansion) matchers. Matching semantics — first match wins, no match → ask — are part of the contract. |
| `~/.octo/mcp.json`, `.octo/mcp.json` | JSON | The `mcpServers` map: stdio entries (`command`, `args`, `env`) and HTTP entries (`url`, `headers`, `auth: "oauth"`), plus `disabled`. Project file overrides user file by server name. The shape stays compatible with the common `mcpServers` convention, so configs written for other MCP hosts keep loading. |
| `~/.octo/channels.yml` | YAML | The `channels` map keyed by platform name; each platform's documented keys (and `enabled`) keep their meaning. |

### Skills

`SKILL.md` files — YAML frontmatter (`name`, `description`; unknown keys
ignored) plus a markdown body — discovered from `~/.octo/skills/<name>/` and
`.octo/skills/<name>/` (project wins). The format is Claude Code's: skills
written for Claude Code load unmodified, including the tool-name bridge.
Disabling via `tools.disabled_skills` in config.yml is part of the contract.

### Custom sub-agents

`~/.octo/agents/<name>.md` definitions — YAML frontmatter (`name`,
`description`, `read_only`; unknown keys ignored) plus a markdown system
prompt — keep loading with the same fields and discovery path.

### Identity and memory files

`~/.octo/soul.md`, `~/.octo/user.md`, `~/.octo/octorules.md`, and per-repo
`.octorules` keep being read, with the same layering (later overrides
earlier) and `@include` support. Cross-session memory keeps its layout:
`~/.octo/memories/<slug>/MEMORY.md` as the always-injected index plus topic
files loaded on demand. These are plain markdown you can edit by hand;
upgrades never reformat them.

### Saved state (read guarantee)

Sessions (`~/.octo/sessions/*.jsonl`) and scheduler tasks (`~/.octo/tasks/`)
carry a **read guarantee**: every 1.x release reads state written by any
earlier 1.x release (and by 0.19+). The write format itself may evolve in
minors — the promise is that your history and tasks survive upgrades, not
that the bytes stay identical. Downgrades are not covered: state written by
a newer octo may not load in an older one.

### CLI

- **Subcommands** (`serve`, `config`, `skills`, `memory`, `init`,
  `completion`, `version`, `help`, and bare `octo` as chat) and their
  **documented flags** keep their names and semantics. New flags are
  additive; renames keep the old spelling working through the deprecation
  window.
- **Exit codes**: `0` success, `1` error, `2` usage/unknown-help, and `42`
  from `octo serve` meaning "restart requested" (the supervisor contract —
  external supervisors may rely on it).
- **Not covered**: human-readable stdout/TUI text. Don't parse it; if you
  need machine-readable output for a command that lacks it, that's a
  feature request, not a stability bug.

### Environment variables

`OCTO_*` variables (e.g. `OCTO_ACCESS_KEY`, `OCTO_PROVIDER`,
`OCTO_SERVE_WORKER`) and the per-vendor credential variables
(`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, …, and the
catch-all `OPENAI_COMPATIBLE_API_KEY` / `OPENAI_COMPATIBLE_BASE_URL` /
`ANTHROPIC_COMPATIBLE_*` pair) keep their meaning. New variables are
additive.

## Best-effort surfaces

- **The HTTP API (`/api/*`) and WebSocket events.** This is the embedded
  Web UI's private API; the UI ships in the same binary, so they can never
  drift apart. Scripting it with `curl` works and is documented behavior
  (see [SECURITY.md](SECURITY.md) for authentication), but routes, fields,
  and events may change in any minor — with a CHANGELOG callout. A
  versioned `/api/v1` becomes worth doing when third-party integrations
  exist; until then there is no API stability promise.
- **Default content**: the embedded default permission rules, default
  skills, and prompt composition are behavior tuning, not format — they
  evolve in minors. (The *format* they're written in is Stable, above.)
- **`GET /api/health` and `GET /api/version`** stay unauthenticated and
  keep returning JSON, but their bodies may gain fields.

## Internal

Everything under `~/.octo/` not named above — including `tmp/`, `logs/`,
`bin/`, `trash/`, `uploads/`, `mcp-tokens/`, `history/` —
plus `~/.octo/skills-default/` (re-extracted per release), the
`__`-prefixed CLI entrypoints (`__complete`, `__sandboxed-exec`,
`__trash-backup`), dev-docs, and all Go packages. The module's Go API is
not a supported surface: octo is distributed as a binary, and
`internal/` packages carry no compatibility promise.

## Migration policy

- **Old formats migrate automatically on read.** When a format changes,
  octo keeps reading the old shape and upgrades it on the next write — the
  way the legacy flat `config.yaml` migrates into the `models:` list today
  (the original is kept as `config.yaml.bak`). No manual migration step.
- **Deprecation window**: a deprecated format or CLI spelling keeps working
  for **at least one minor release** after the CHANGELOG announces it
  (under a *Deprecated* heading, plus a runtime warning where feasible).
- **Removing read support for an old format is a breaking change** and
  happens only in a major release.
- Auto-migration is one-way; where a file is rewritten, the original is
  preserved with a `.bak` suffix when practical.

## Reporting

A Stable surface that broke without a major version (post-1.0) or without a
CHANGELOG **Breaking** callout (pre-1.0) is a bug — please
[open an issue](https://github.com/Leihb/octo-agent/issues).
