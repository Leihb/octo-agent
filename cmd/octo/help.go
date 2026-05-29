package main

import (
	"fmt"
	"io"
)

// printCommandHelp renders the rich help for one subcommand: a one-line
// summary, copy-paste-able examples, the most-reached-for flags grouped
// by purpose, environment variables, and a pointer to `--help` for the
// full flag list. Returns false when name doesn't match a known command
// so the caller can fall back to "no help available" and exit 2.
func printCommandHelp(name string, w io.Writer) bool {
	switch name {
	case "chat":
		chatHelp(w)
	case "goal":
		taskHelp(w)
	case "memory":
		memoryHelp(w)
	case "init":
		initHelp(w)
	case "config":
		configHelp(w)
	case "completion":
		completionHelp(w)
	case "mcp":
		mcpHelp(w)
	default:
		return false
	}
	return true
}

func mcpHelp(w io.Writer) {
	fmt.Fprintln(w, `octo mcp — Model Context Protocol client. Tools are on by default, so every
server listed in mcp.json gets connected at session start; its tools, resources,
and prompts ride alongside octo's built-in tools. Pass --no-tools to skip them.

Configuration:
  ~/.octo/mcp.json                 user-global config (always loaded)
  ./.octo/mcp.json                 project-local config (overrides user-global per name)

mcp.json format (mirrors Claude Code):
  {
    "mcpServers": {
      "filesystem": {
        "command": "npx",
        "args":    ["-y", "@modelcontextprotocol/server-filesystem", "/some/path"],
        "env":     {"FOO": "bar"}
      },
      "remote-api": {
        "url":     "https://example.com/mcp",
        "headers": {"Authorization": "Bearer abc..."}
      },
      "lark-project": {
        "url":  "https://project.larksuite.com/mcp_server/v1",
        "auth": "oauth"
      }
    }
  }

A server entry must set EITHER `+"`"+`command`+"`"+` (stdio transport — spawn a subprocess)
OR `+"`"+`url`+"`"+` (Streamable HTTP transport). `+"`"+`disabled: true`+"`"+` skips an entry without
removing it.

Auth (HTTP only):
  ""       — no auth beyond what's in headers (default)
  "oauth"  — discover the server's auth-server metadata on first 401,
             run RFC 8628 Device Authorization Grant (open a URL,
             enter a code), cache the token at
             ~/.octo/mcp-tokens/<server>.json. Refresh tokens are
             used automatically; expired refresh tokens trigger a
             fresh device-flow prompt.

Tool naming in the agent's tool list:
  mcp__<server>__<tool>            one entry per advertised tool
  mcp__<server>__resource_read     synthesized when server supports resources/*
  mcp__<server>__prompt_get        synthesized when server supports prompts/*

Examples:
  octo chat                        # auto-connect every configured server
  /mcp                             # REPL: list connected servers + surface counts

Failure handling — a server that won't start, fails initialize, or times out
during the handshake is logged on stderr and skipped. The session keeps going
with the survivors. The connect timeout is 10s per server.

What's not yet implemented (v1): server-initiated notifications, resource
subscriptions, sampling (server → client LLM calls), roots.`)
}

func chatHelp(w io.Writer) {
	fmt.Fprintln(w, `octo chat — interactive AI session, or single-turn with a message argument.

Examples:
  octo chat                             Start a new REPL session
  octo chat "summarise the README"      Single-turn mode (no REPL)
  octo chat -c last                     Resume the most recent session
  octo chat -c a3b2c1d4                 Resume by short ID (any unique substring works)
  octo chat --list-sessions             Show recent sessions and exit
  octo chat --no-tools                  Plain chat — disable the built-in tools
  octo chat --provider openai           Use OpenAI instead of Anthropic
  octo chat --sandbox                   Confine tool commands to repo + tmp, no network

Common flags:
  -c, --continue <id>      Resume a session — 'last', short ID, or substring of an ID
  --no-tools               Disable built-in tools (terminal, edit_file, …) + MCP/skills — plain chat
  --provider <name>        anthropic (default) | openai
  --model <name>           Override the default model for the provider
  --no-save                Don't auto-save the session to ~/.octo/sessions
  --no-memory              Disable cross-session memory injection
  --sandbox                OS-enforced confinement for terminal commands (macOS/Linux)
  --permission-mode <m>    interactive (default; prompts on ask) | strict (denies asks)
  --list-sessions          Print recent sessions and exit
  --quiet                  Strip status chrome (spinner, banner, cache line)
  --verbose                Print extra context (provider/model/endpoint, always-on cache line)

Environment:
  ANTHROPIC_API_KEY        Required when --provider=anthropic
  OPENAI_API_KEY           Required when --provider=openai
  ANTHROPIC_BASE_URL       Override the Anthropic endpoint (proxies, Claude-compatible servers)
  OPENAI_BASE_URL          Override the OpenAI endpoint
  OCTO_HISTORY_FILE        Override the REPL history path (default ~/.octo/history)
  OCTO_HOOK_PRE_TURN       Shell command run at the start of each turn (C9 Phase 3)
  OCTO_HOOK_POST_TURN      Shell command run at the end of each successful turn
  OCTO_VERBOSITY           quiet | normal | verbose; overridden by --quiet / --verbose

Run "octo chat --help" for the full flag list.`)
}

func taskHelp(w io.Writer) {
	fmt.Fprintln(w, `octo goal — autonomous task orchestration (M11). Plan a goal into a DAG of
subtasks, then dispatch one sub-agent per ready node in parallel.

Examples:
  octo goal start "refactor the cache layer"      Plan + run end-to-end
  octo goal start "..." --plan-only               Just plan; don't execute
  octo goal list                                  Show every task on disk (alias: ls)
  octo goal status last                           DAG state of the most recent task
  octo goal status a3b2c1d4                       DAG state by short ID
  octo goal show <id> <subtask-id>                Full result/error for one subtask
  octo goal resume <id>                           Re-run a Failed / Cancelled task
  octo goal cancel <id>                           Mark a task Cancelled

ID shortcuts (every <id> argument accepts these):
  last                     The most recently created task
  <8-char hex>             The trailing short ID shown by 'octo goal list'
  any unique substring     Resolves if it matches exactly one task

Common flags (for start / run / resume):
  --provider <name>        anthropic (default) | openai
  --model <name>           Override the default planner / sub-agent model
  --plan-only              start: produce the plan but don't run subtasks

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Required for the planner + sub-agents

Task state lives at ~/.octo/tasks/<id>.json — fsync'd on every transition, so a
kill -9 mid-run resumes cleanly via "octo goal resume".`)
}

func memoryHelp(w io.Writer) {
	fmt.Fprintln(w, `octo memory — inspect cross-session memory (the durable facts the agent
remembers between sessions).

Examples:
  octo memory list                   Show active memory entries
  octo memory list --archive         Show entries that have been consolidated into the summary

Layout:
  ~/.octo/memory/<slug>.md            One file per unconsolidated fact
  ~/.octo/memory/memory_summary.md    The global injected summary
  ~/.octo/memory/memory_summary__*.md Per-project summaries (injected only in that project)
  ~/.octo/memory/MEMORY.md            Searchable index of slugs

Memory is built up by the in-session 'remember' tool and consolidated at chat
startup once enough entries accumulate. Facts remembered inside a git repo are
scoped to that project; the rest are global. To disable memory injection for a
single session, run "octo chat --no-memory".`)
}

func initHelp(w io.Writer) {
	fmt.Fprintln(w, `octo init — one-shot agentic run that analyzes the current repo and writes
(or improves) a .octorules file at its root. The .octorules file is loaded
into the system prompt on every chat session in this directory.

Examples:
  octo init                          Generate/update .octorules
  octo init --provider openai        Use a different provider
  octo init --sandbox                Confine the analysis to repo + tmp, no network

Common flags:
  --provider <name>        anthropic (default) | openai
  --model <name>           Override the default model
  --plain                  Terse ↳ status lines instead of rich diff cards
  --sandbox                OS-enforced command confinement (macOS/Linux)
  --permission-mode <m>    interactive | strict (default)

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Required for the chosen provider.`)
}

func configHelp(w io.Writer) {
	fmt.Fprintln(w, `octo config — save your default provider, model, and (optionally) base URL to
~/.octo/config.json so a bare `+"`octo chat`"+` works without re-typing flags.

Precedence (highest first): CLI flag (--provider/--model) > env var > this file
> built-in default. API keys are read from the environment first; storing one
here is opt-in and lands in plaintext (mode 0600) — prefer the env var.

Usage:
  octo config             Interactive setup wizard (writes the file)
  octo config show        Print the effective provider/model and where each
                          comes from (never prints the key itself)
  octo config path        Print the config file path

File (~/.octo/config.json):
  {
    "provider": "openai",
    "model":    "gpt-4o-mini",
    "base_url": "https://api.deepseek.com"   // optional, for compatible 3rd parties
  }

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Override any stored key, per run.`)
}
