# Model & API Key Configuration

## Interactive Configuration

During a session:

```bash
> /config
```

This opens an interactive prompt to set:

- **API Key** — Your model provider key
- **Model** — Model identifier (e.g., `anthropic/claude-opus-4-7`)
- **Base URL** — Provider endpoint
- **Anthropic Format** — Whether to use native Anthropic Messages format

## Config File

Configuration is stored in `~/.octo/config.yml`:

```yaml
models:
  - model: anthropic/claude-opus-4-7
    api_key: sk-xxx
    base_url: https://api.anthropic.com
    type: default
    anthropic_format: true
  - model: anthropic/claude-haiku-4-5
    api_key: sk-xxx
    base_url: https://api.anthropic.com
    type: lite
```

## Environment Variables

| Variable | Description |
|---|---|
| `OCTO_API_KEY` | Default model API key |
| `OCTO_BASE_URL` | Default model base URL |
| `OCTO_MODEL` | Default model name |
| `OCTO_ANTHROPIC_FORMAT` | Use anthropic format |
| `OCTO_LITE_API_KEY` | Lite model API key |
| `OCTO_LITE_BASE_URL` | Lite model base URL |
| `OCTO_LITE_MODEL` | Lite model name |

ClaudeCode-compatible variables are also supported:

| Variable | Description |
|---|---|
| `ANTHROPIC_API_KEY` | API key |
| `ANTHROPIC_BASE_URL` | Base URL |

## Model Types

- **default** — Primary model for main agent work
- **lite** — Cheaper/faster model for subagents and simple tasks
- **fallback** — Used when the primary model is unavailable

## Session Budgets

Two opt-in guards in `~/.octo/config.yml` cap runaway tool loops and cost:

```yaml
settings:
  max_turns: 30          # per-task turn cap; LLM tool-loops past this trigger abort. Default 30; set to nil for unlimited.
  max_cost_usd: 5.0      # session USD cap (cumulative across tasks). Default nil (unlimited).
```

CLI override per run: `--max-turns N`, `--max-cost N`. Inside a session, `/cost` prints the current token totals and estimated USD spend.

Cost is computed by `Octo::ModelPricing` from the active model's published rate; self-hosted or unknown models still report tokens but cost displays as `n/a`.

## Hooks

Shell commands can be wired to agent lifecycle events through the same
`settings:` block. Each entry runs in a fresh shell with `OCTO_EVENT`,
`OCTO_TOOL_NAME`, `OCTO_TOOL_INPUT`, `OCTO_SESSION_ID`, `OCTO_WORKING_DIR`
exposed as environment variables.

```yaml
settings:
  hooks:
    before_tool_use:
      - matcher: "terminal"
        command: "echo $OCTO_TOOL_INPUT >> ~/.octo/audit.log"
        block: true     # non-zero exit aborts the tool call
        timeout: 5      # seconds; default 30
    on_complete:
      - command: "osascript -e 'display notification \"Octo done\"'"
```

Events: `before_tool_use`, `after_tool_use`, `on_tool_error`, `on_start`,
`on_complete`, `on_iteration`, `session_rollback`.

Only `before_tool_use` honors `block: true` (non-zero exit denies the tool
call). Other events log non-zero exits but never abort. Hooks run only on
the main agent, not on forked sub-agents.

## Supported Providers

Octo has built-in presets for:

- **openrouter** — `https://openrouter.ai/api/v1`
- **anthropic** — `https://api.anthropic.com`
- **openai** — `https://api.openai.com`
- **bedrock** — AWS Bedrock
- **deepseek** — `https://api.deepseek.com`
- **kimi** — `https://api.moonshot.cn`
- **minimax** — `https://api.minimax.chat`
- **qwen** — `https://dashscope.aliyuncs.com`
- **glm** — `https://open.bigmodel.cn/api/paas/v4`

Use any custom endpoint by specifying `base_url` directly.
