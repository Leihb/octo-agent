# Terminal tool family

The terminal tools let the agent run shell commands — synchronously, as
tracked background processes, or as detached daemons — and observe and control
those processes. They live in `internal/tools/` and all route process work
through one `BackgroundManager`.

## Tools

| Tool | Purpose |
|---|---|
| `terminal` | Run a command. Synchronous, `run_in_background:true`, or `detached:true`. |
| `terminal_output` | Snapshot the last N lines of a background process's output + status. |
| `terminal_list` | List this session's background processes (id, status, elapsed, command). |
| `terminal_input` | Write to a background process's stdin. |
| `kill_shell` | Signal/terminate a background process and return its final output. |

`terminal` is the only one that starts work; the rest address an existing
process by the `bg_N` id `terminal` returns.

## Three ways to run a command

`terminal` picks the mode from its flags (checked in this order):

1. **`detached:true`** — a daemon that deliberately outlives octo. Built by
   `detachedCommand`: a new session (`setsid` on POSIX,
   `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` on Windows) so the harness's
   process-group kill can't reach it, on `context.WithoutCancel(ctx)` so a turn
   ending can't kill it. stdout/stderr go to a log file (`log_file`, default
   `./nohup.out`), stdin to `/dev/null`. It is **not** tracked in any manager —
   fire-and-forget — so `terminal_output` / `kill_shell` / shutdown all ignore
   it; only the OS pid is returned. It still runs through the same shell
   wrapping and OS sandbox as any other command (detach controls lifetime, the
   sandbox controls what it may touch — orthogonal).

2. **`run_in_background:true`** — a tracked background process. Returns a
   `bg_N` id immediately, no timeout. Output is observable via
   `terminal_output`, the process via `terminal_list` / `kill_shell`, and it is
   **killed when the session ends**.

3. **Synchronous (default)** — runs with a 120 s timeout. Implemented as a
   hidden (`visible:false`) background process so that on timeout it is simply
   *promoted* to a visible background task (no kill, no restart) and its id
   returned. On normal completion it is reaped (`Remove`).

`guardCommand` blocks dangerous commands before any of these branches.

## BackgroundManager and the process lifecycle

`BackgroundManager` owns the tracked processes (`map[id]*bgProcess`). Each
`bgProcess` keeps a capped tail buffer (`maxBgOutputBytes`, 1 MiB) of combined
stdout+stderr, its status, and a `cancel` for its command context.

### Spawn

`Start` builds the command via `shellCommand` (shell wrapping + safe-rm trash
wrapper + OS sandbox when active), starts it in its **own process group**
(`Setpgid`), and tracks it. A reader goroutine drains output into the buffer; a
waiter goroutine `cmd.Wait()`s, closes the pipe and stdin, then runs the
completion hook.

### Terminate — single chokepoint

All termination goes through one private function, `terminate(p, signal)`,
which owns two rules so they can't drift between call sites:

- **Always signal the whole process group** (`kill(-pid)` on POSIX,
  `taskkill /T` on Windows) — never just the direct child. The direct child is
  the `sh -c` / `pwsh` wrapper; signalling only it orphans whatever it spawned.
- **Cancel the context only on `SIGKILL`** (so `exec.CommandContext` fires its
  own SIGKILL as a backstop). On `SIGTERM`/`SIGINT` cancelling would let exec
  race in an automatic SIGKILL and defeat the graceful stop.

`KillWithSignal` (one process), `KillAll` (all in a manager), and `Remove`
(reap on map removal) all call `terminate`.

### Reap on exit

`KillAllBackground` is wired into every entry point's shutdown — CLI/TUI REPL,
`octo serve` (`Server.Shutdown`), and the IM bridge (`octo channel`) — and
reaps `defaultBg` **and every per-session manager**, so no background process
outlives its host process. Detached daemons are exempt by construction (not
tracked).

## Per-session scoping

`defaultBg` is the process-global manager used by the CLI/TUI (one process = one
session). The web server and IM bridge instead give each conversation its **own**
manager so background processes are isolated — their own `bg_N` namespace,
invisible to other sessions — and reaped when the session ends.

This reuses the ctx-scoped service pattern (cf. `WithSubAgentManager`,
`WithTaskStore`):

- `WithBackgroundManager(ctx, mgr)` stamps the per-session manager onto the turn
  context. Each terminal tool resolves its manager via
  `resolveBackgroundManager(ctx, t.mgr)` — **ctx-scoped > tool-local field >
  `defaultBg`**.
- `SessionBackgroundManager(id)` / `CloseSessionBackgroundManager(id)` maintain a
  registry keyed by an opaque session id. The web server stamps it in
  `prepareToolTurn` (keyed by session id) and closes it on session delete; the
  IM bridge stamps it per chat (keyed by `"im:"+sessionKey`), persisting across
  messages in that chat.

The CLI/TUI never stamp a ctx manager, so they keep using `defaultBg` (and its
completion-push hook + `RunningBackground` panel, which the server/IM never
wire).

## Observability: push for completion, pull-snapshot for progress

Two distinct needs, two distinct mechanisms:

- **Completion is pushed.** When a tracked process exits, the manager's
  `onExit` hook fires with the final output; the REPL injects a
  `[BACKGROUND COMPLETED]` system-reminder into the conversation. The model
  never needs to poll to learn that a process finished or to get its result.

- **Progress is pulled, as a snapshot.** `terminal_output` returns the last N
  lines (`lines`, default 50) plus status via `bgProcess.tail`, which does
  **not** advance any cursor. Repeated calls return the same view, so there is
  no "new since last call" to chase and no incentive to loop. `terminal_list`
  is the other snapshot — the set of live/recent processes — for recovering a
  lost id.

The internal cursor read (`readNew`) still exists for the synchronous poll loop
and the completion push; only the model-facing `terminal_output` uses the
non-advancing snapshot.

## Cross-platform shell

`shellInvocation` / `shellCommand` select the shell once: `sh -c` on
macOS/Linux (with the safe-rm trash wrapper when a project dir is known),
PowerShell (`pwsh`, else `powershell`) on Windows. Process-group and detach
options are platform-specific (`internal/tools/terminal_kill.go` for POSIX,
`terminal_kill_windows.go` for Windows). `terminal_input`'s stdin delivery is
reliable only on POSIX — PowerShell's `-Command` mode does not deterministically
forward redirected stdin to a spawned native process.
