# Permission System

octo's permission engine gates every tool call through a rule-driven decision engine. Each invocation is evaluated against an ordered list of rules; the first matching rule wins. If no rule matches, the implicit default is `ask`.

## Modes

The engine runs in one of three modes, selectable at startup or cycled at runtime:

| Mode | Behavior | Best for |
|------|----------|----------|
| **interactive** | `ask` → prompt the user for approval | CLI default — safe and usable |
| **strict** | `ask` → deny automatically | Non-interactive callers (HTTP server, IM bridge) |
| **auto** | `ask` → allow automatically | Trusted, repetitive workflows — use with caution |

Cycle modes in the TUI with **Shift+Tab**.

## Rule format

Rules are defined in `~/.octo/permissions.yml` (user overrides) and an embedded `defaults.yml` (base layer). The YAML schema:

```yaml
tool_name:
  - allow: { pattern: "substring" }
  - deny:  { pattern: "substring" }
  - ask:   { pattern: "substring" }
```

Rules are scanned in order; the first match wins. A tool with no matching rules falls through to the implicit `ask`.

### Rule axes

| Axis | Applies to | Syntax |
|------|-----------|--------|
| `pattern` | `terminal` — substring match against the command string | Plain string, case-sensitive |
| `path` | `write_file`, `edit_file`, `read_file` — glob match against file path | Glob with `**` for recursive dirs; `$CWD` expands to working directory |
| `hostname` | `web_fetch` — glob match against URL host | `*` matches a single DNS label |

### Pattern refinements

- A pattern ending in `/` or `~` (filesystem root marker) only matches when that marker ends an argument. So `deny: rm -rf /` blocks `rm -rf /` and `rm -rf /*` but **not** `rm -rf /path/under`.
- Empty pattern (`""`) matches unconditionally — used for default allow/deny on pattern-free tools.

## Default rules (excerpt)

```yaml
terminal:
  - deny:  { pattern: "rm -rf /" }
  - deny:  { pattern: "rm -rf ~" }
  - ask:   { pattern: "rm -rf" }
  - ask:   { pattern: "sudo " }
  - ask:   { pattern: "curl " }
  - allow: { pattern: "ls" }
  - allow: { pattern: "git status" }
  - allow: { pattern: "go test" }

write_file:
  - deny:  { path: ["**/.ssh/**", "**/.env", "**/id_rsa*"] }
  - allow: { path: ["$CWD/**"] }

web_fetch:
  - deny:  { hostname: ["10.*", "192.168.*", "127.*", "localhost"] }
  - allow: { hostname: ["github.com", "*.github.com", "go.dev"] }
```

## Session memory

When the user answers a permission prompt, they can choose:
- **y** — allow this once
- **a** — allow for the rest of this session (remembered until exit)
- **n / Esc** — deny this once

Session-remembered decisions are keyed by a hash of (tool, input), so identical calls short-circuit without re-prompting.

## Custom rules

Create `~/.octo/permissions.yml` to override defaults per-tool:

```yaml
terminal:
  - allow: { pattern: "docker " }   # auto-allow docker commands
  - deny:  { pattern: "docker system prune" }  # except this one
```

User rules **fully replace** the default rule list for that tool (not append). To keep defaults and add extras, copy the defaults and edit.

## Denial messages

When a tool call is denied, the engine returns a structured reason that the LLM sees, so it can explain to the user instead of failing silently:

- `permission_denied: terminal matched deny rule (pattern: "rm -rf /")`
- `permission_denied: write_file matched deny rule (path: [**/.ssh/**])`
- `permission_denied: web_fetch — no matching rule in strict mode (implicit ask → deny)`
