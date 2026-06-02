# Headless one-shot & the two REPL surfaces

`octo chat` has exactly two front-ends over one agent core:

- **Interactive TUI** — an interactive terminal drives the bubbletea TUI
  (`runTUI`). This is the only multi-turn, interactive surface.
- **Headless one-shot** — everything else runs a single agentic turn and exits
  (`runOnce`), à la `claude -p`. The full tool loop runs; tools are on by
  default.

There is no interactive plain line-REPL: if you're at a terminal you get the
TUI, and if you're not you get a one-shot.

## Routing

```
useTUI := noPositionalMessage && stdinIsTTY && !--no-tui && !OCTO_TUI=0 && no --prompt-file
```

`useTUI` true → `runTUI`. Otherwise → `runOnce`, with the prompt resolved in
order:

1. positional message — `octo chat "fix the build"`
2. `--prompt-file <path>` — the file's contents, newlines intact
3. all of piped stdin — `echo "..." | octo chat`, `octo chat < issue.txt`

No prompt source → an error (octo never blocks reading a tty for a headless
run). `--no-tui` / `OCTO_TUI=0` force the one-shot path even on a terminal.

## What the one-shot shares with the TUI

Both build the same agentic context — built-in tools, MCP servers, the
sub-agent dispatcher, the permission engine, skills, and the session-start
memory injection — and both execute through `runTurn`, so memory nudges and
pre/post hooks fire identically. They differ only in shell:

- The TUI renders live cards and owns its own input; it persists the session
  and supports `-c`/`--continue` resume and slash commands.
- The one-shot renders through `plainView` (streamed text + terse `↳` tool
  lines), does not persist a session, and exits after one turn. Permission /
  Ask prompts are interactive when stdin is a tty (a message typed at a
  terminal) and auto-deny when stdin is a consumed pipe — the headless posture.

`--stream=false` runs the same agentic loop but prints only the final reply
text, keeping captured stdout clean.

Memory consolidation (the boundary-summary pass) runs only in the TUI, so a
headless run stays deterministic and fast.

## Why

The TUI is hostile to pipes, tests, CI, and eval harnesses: it grabs stdin,
emits escape sequences, and repaints. Headless consumers need a clean,
scriptable, single-shot surface. Folding the old tool-less single-turn mode and
the old line-per-turn piped REPL into one agentic one-shot removed a footgun
(piping a multi-line prompt used to be shredded into one turn per line) and gave
`octo chat "msg"` the full tool loop. octo-eval drives this path via
`--prompt-file`.
