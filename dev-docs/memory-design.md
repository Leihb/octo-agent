# Cross-session memory

octo's memory is a per-repo directory of plain markdown that the agent manages
with its own file tools — the Claude Code model. There is no dedicated
remember/forget tool, no typed-entry store, and no code-driven consolidation:
the agent reads, writes, edits, and deletes memory files directly, so editing
and deletion are first-class and instant.

This is the agent's *automatic* layer. The *hand-written* layers — `~/.octo/soul.md`,
`~/.octo/user.md`, `~/.octo/octorules.md`, and per-repo `.octorules` — are
separate and described in `identity-files-design.md`.

## Layout

```
~/.octo/memory/<repo-slug>/
  MEMORY.md      index, injected into the system prompt every session
  <topic>.md     detail files the agent creates and reads on demand
```

- **Per repo.** The directory is keyed by the git top-level of the working
  directory (`memory.ProjectRoot`), so each project has its own memory and
  facts don't bleed across repos. Outside a git repo the working directory is
  used. The slug is the repo basename plus a short hash of the full path, so two
  checkouts that share a basename don't collide.
- **MEMORY.md is the index.** It is loaded into the system prompt at session
  start, truncated to the first 200 lines / 25 KB (whichever comes first),
  mirroring Claude Code's cap. Topic files are not loaded up front — the agent
  reads them on demand with its file tools when MEMORY.md points at one.

## Injection

At session start `cmd/octo` resolves the directory, creates it, and injects
`memory.RenderInjection(dir)` into the composed system prompt (the `memory`
layer of `prompt.Compose`). The injection is a short instruction block —
*where* memory lives and *how* to manage it — followed by the current MEMORY.md
(or an "empty" marker so a fresh project knows where to start). It is framed as
background context, not user instructions, and is frozen for the session: what
the agent writes now surfaces in the *next* session, not the current one.

The session-prompt guidance (`internal/prompt/base.md`, "Memory" section)
covers when to save (lasting preferences, corrections + the why, validated
judgment, external resources), what not to save (one-off task state, anything
derivable from the repo, secrets), grounding answers in memory with a brief
inline attribution, and verifying a remembered file/flag still exists before
acting on it.

## Writing — file tools, whitelisted directory

The agent saves with `write_file` (append to MEMORY.md or a topic file), edits
with `edit_file`, and removes with `terminal` (`rm`/`mv`). The memory directory
lives outside the working directory, where the permission engine's default
`write_file`/`edit_file` rules only auto-allow `$CWD/**`. So `cmd/octo` passes
the directory to `permission.New(..., allowWriteRoots...)`, which prepends an
`allow { path: [<memDir>, <memDir>/**] }` rule to those tools — the agent
manages its memory without a prompt on every save, while CWD and
secret-path rules still apply everywhere else.

## Inspecting

- `octo memory list` — list the project's memory files; `octo memory path` —
  print the directory.
- `/memory` in the TUI — the same listing.

These are viewers/locators only; the files are the source of truth and the
agent owns them.

## Why this shape

The earlier design was a typed one-file-per-fact store written through a
`remember` tool and folded into consolidated summaries by a background
sub-agent. It had no way to remove a fact once consolidated — a wrong or
obsolete entry lived in the summary prose with no addressable handle, re-injected
every session. The file model removes that gap by construction: memory is just
files, so correcting or forgetting is an ordinary edit or delete. It also drops
a large amount of machinery (typed entries, summaries, state, archive, git
auto-commit, the remember/forget tools, the per-turn nudge) in favour of the
tools the agent already has.
