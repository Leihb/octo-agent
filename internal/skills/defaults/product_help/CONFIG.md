# octo Configuration Reference

## Config file

Path: `~/.octo/config.yaml`

Created interactively via `octo config` or edited directly.

## Fields

| Field | Type | Description |
|-------|------|-------------|
| `provider` | string | `"anthropic"` or `"openai"` |
| `model` | string | Model name (e.g. `claude-sonnet-4-20250514`, `gpt-4.1`) |
| `base_url` | string | Optional custom API endpoint |
| `permission_mode` | string | `"strict"`, `"confirm"`, or `"auto"` |
| `coauthor` | bool | Append `Co-authored-by` to git commits |
| `show_reasoning` | bool | Stream reasoning/thinking traces to terminal |
| `reasoning_effort` | string | `"low"`, `"medium"`, `"high"`, or `""` (off) |
| `api_key` | string | Plaintext fallback (discouraged — use env var instead) |

## Precedence

CLI flags (`--provider`, `--model`, etc.) > env vars (`OCTO_PROVIDER`, `ANTHROPIC_API_KEY`, etc.) > config file > built-in defaults.

## Permission modes

- **strict** — Block risky operations (file overwrites outside worktrees, shell commands that modify system state, etc.). The agent must ask for explicit approval.
- **confirm** — Ask before executing any tool that has side effects (default behavior).
- **auto** — Run without asking. Useful for trusted, repetitive workflows. Use with caution.

## Example

```yaml
provider: anthropic
model: claude-sonnet-4-20250514
coauthor: true
show_reasoning: true
reasoning_effort: medium
```
