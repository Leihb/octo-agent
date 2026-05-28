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
	case "task":
		taskHelp(w)
	case "memory":
		memoryHelp(w)
	case "init":
		initHelp(w)
	case "memoryd":
		memorydHelp(w)
	case "completion":
		completionHelp(w)
	default:
		return false
	}
	return true
}

func chatHelp(w io.Writer) {
	fmt.Fprintln(w, `octo chat — interactive AI session, or single-turn with a message argument.

Examples:
  octo chat                             Start a new REPL session
  octo chat "summarise the README"      Single-turn mode (no REPL)
  octo chat -c last                     Resume the most recent session
  octo chat -c a3b2c1d4                 Resume by short ID (any unique substring works)
  octo chat --list-sessions             Show recent sessions and exit
  octo chat --tools                     Enable terminal / edit_file / etc. (agentic loop)
  octo chat --provider openai           Use OpenAI instead of Anthropic
  octo chat --sandbox --tools           Confine commands to repo + tmp, no network

Common flags:
  -c, --continue <id>      Resume a session — 'last', short ID, or substring of an ID
  --tools                  Enable built-in tools (terminal, edit_file, web_search, …)
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
	fmt.Fprintln(w, `octo task — autonomous task orchestration (M11). Plan a goal into a DAG of
subtasks, then dispatch one sub-agent per ready node in parallel.

Examples:
  octo task start "refactor the cache layer"      Plan + run end-to-end
  octo task start "..." --plan-only               Just plan; don't execute
  octo task list                                  Show every task on disk (alias: ls)
  octo task status last                           DAG state of the most recent task
  octo task status a3b2c1d4                       DAG state by short ID
  octo task show <id> <subtask-id>                Full result/error for one subtask
  octo task resume <id>                           Re-run a Failed / Cancelled task
  octo task cancel <id>                           Mark a task Cancelled

ID shortcuts (every <id> argument accepts these):
  last                     The most recently created task
  <8-char hex>             The trailing short ID shown by 'octo task list'
  any unique substring     Resolves if it matches exactly one task

Common flags (for start / run / resume):
  --provider <name>        anthropic (default) | openai
  --model <name>           Override the default planner / sub-agent model
  --plan-only              start: produce the plan but don't run subtasks

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Required for the planner + sub-agents

Task state lives at ~/.octo/tasks/<id>.json — fsync'd on every transition, so a
kill -9 mid-run resumes cleanly via "octo task resume".`)
}

func memoryHelp(w io.Writer) {
	fmt.Fprintln(w, `octo memory — inspect cross-session memory (the durable facts the agent
remembers between sessions).

Examples:
  octo memory list                   Show active memory entries
  octo memory list --archive         Show entries that have been consolidated into the summary

Layout:
  ~/.octo/memory/active/<slug>.md    One file per unconsolidated fact
  ~/.octo/memory/summary.md          The injected summary (loaded into every system prompt)

Memory is built up by the in-session 'remember' tool and consolidated either
at chat startup or by the long-running daemon (see "octo help memoryd"). To
disable memory injection for a single session, run "octo chat --no-memory".`)
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

func memorydHelp(w io.Writer) {
	fmt.Fprintln(w, `octo memoryd — long-lived foreground daemon that takes over the boundary
extraction + consolidation passes the chat path otherwise runs at startup.
With the daemon alive, "octo chat" startup is effectively instant — the
extract/consolidate cost moves out of the user's critical path.

Examples:
  octo memoryd start                 Start the daemon (refuses if one's already running)
  octo memoryd status                Show PID, uptime, last-extracted session
  octo memoryd stop                  SIGTERM the daemon; waits for a clean shutdown

Tick + idle window:
  - Every 15s (default) the daemon scans recent sessions.
  - A session is extracted when its file mtime is older than 15 minutes
    (idle window) — guarantees we never race a still-active chat.
  - One session per tick by design — SIGTERM stops between extractions.

Environment:
  OCTO_MEMORYD_PROVIDER    Provider override (anthropic | openai). When unset
                           the daemon picks the first one with a key present.
  ANTHROPIC_API_KEY / OPENAI_API_KEY    Required (one of them) to start.

PID file:
  ~/.octo/memoryd.pid                Created at start, removed on clean shutdown.

Not supported on Windows. The chat path detects the daemon via the PID file
and skips its in-process memory pass when the daemon is alive.`)
}
