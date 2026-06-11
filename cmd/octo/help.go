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
	case "serve":
		serveHelp(w)
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
  octo                             # auto-connect every configured server
  /mcp                             # REPL: list connected servers + surface counts

Failure handling — a server that won't start, fails initialize, or times out
during the handshake is logged on stderr and skipped. The session keeps going
with the survivors. The connect timeout is 10s per server.

What's not yet implemented (v1): server-initiated notifications, resource
subscriptions, sampling (server → client LLM calls), roots.`)
}

func memoryHelp(w io.Writer) {
	fmt.Fprintln(w, `octo memory — locate and inspect cross-session memory.

Memory is a per-project directory of plain markdown files the agent manages
itself with its file tools (no dedicated remember/forget tool). MEMORY.md is
the index, injected into the system prompt each session; topic files beside it
hold detail and load on demand.

Commands:
  octo memory list     List the project's and inherited memory files (default)
  octo memory path     Print the project's and inherited memory directories

Layout:
  ~/.octo/memories/<repo-slug>/MEMORY.md   Project index, injected every session
  ~/.octo/memories/<repo-slug>/<topic>.md  Project detail files
  ~/.octo/memories/<home-slug>/MEMORY.md   Inherited (home) index, available in every project

The project directory is keyed by git repo root. Home-directory memories are
inherited into every project. To disable memory injection for a single session,
run "octo --no-memory".`)
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
  --permission-mode <m>    interactive | strict (default) | auto

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Required for the chosen provider.`)
}

func serveHelp(w io.Writer) {
	fmt.Fprintln(w, `octo serve — start the HTTP server (REST API + SSE + Web UI).

The server binds to localhost:8080 by default.

Examples:
  octo serve                           Start on :8080
  octo serve --addr 127.0.0.1:3000     Bind to a specific address
  octo serve --no-channel              Skip IM platform bridges
  octo serve --no-tools                Disable the agentic tool loop

Common flags:
  --addr <host:port>       Bind address (default :8080)
  --provider <name>        anthropic (default) | openai
  --model <name>           Override the default model
  --system <text>          Custom system prompt
  --max-tokens <n>         Per-response token cap
  --tools                  Enable agentic tool loop (default true)
  --no-tools               Disable agentic tool loop
  --cors <origins>         CORS allowed origins (comma-separated, * for any)
  --no-channel             Disable IM channel startup

Environment:
  ANTHROPIC_API_KEY        Required when --provider=anthropic
  OPENAI_API_KEY           Required when --provider=openai
  ANTHROPIC_BASE_URL       Override the Anthropic endpoint
  OPENAI_BASE_URL          Override the OpenAI endpoint

Run "octo serve --help" for the full flag list.`)
}

func configHelp(w io.Writer) {
	fmt.Fprintln(w, `octo config — save your default provider, model, and (optionally) base URL to
~/.octo/config.yml so a bare `+"`octo`"+` works without re-typing flags.

Precedence (highest first): CLI flag (--provider/--model) > env var > this file
> built-in default. API keys are read from the environment first; storing one
here is opt-in and lands in plaintext (mode 0600) — prefer the env var.

Usage:
  octo config                      Interactive setup wizard (writes the file)
  octo config show                 Print the effective provider/model and where each
                                   comes from (never prints the key itself)
  octo config path                 Print the config file path
File (~/.octo/config.yml):
  provider: openai
  model: gpt-4o-mini

Self-hosted / third-party endpoints use the compatible catch-all vendors —
the only providers that take a base_url (and require one):
  provider: openai_compatible          # or anthropic_compatible
  model: deepseek-chat
  base_url: https://my-gateway.example

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Override any stored key, per run.`)
}
