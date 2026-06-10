# Server Self-Restart

`octo serve` can restart itself: the process is split into a tiny supervisor
parent and a worker child, connected by an exit-code contract. A restart
request drains the worker gracefully and exits with a reserved code; the
parent re-spawns the worker from the binary path on disk. This is the
mechanism behind two scenarios:

- **Binary upgrade** — a new `octo` binary is placed at the same path, then a
  restart picks it up.
- **Config change** — anything resolved once at startup (provider, model,
  system prompt, `~/.octo` config, `channels.yml`) is re-read by the fresh
  worker.

## Goals

- Restart triggered from inside the server: HTTP API, web UI, or the agent
  itself responding to an IM/web instruction ("重启一下服务").
- One code path on Linux, macOS, and Windows — spawn + wait only, no
  `exec(2)`, no fd passing.
- No external supervisor required, but compose cleanly with one
  (systemd/launchd) when present.
- Bounded graceful drain; session persistence is the safety net beyond the
  bound.

## Non-goals

- **Zero-downtime restart** (nginx-style listener handoff, tableflip). octo
  serve is a personal agent server; a few seconds of port downtime is
  acceptable, web clients already reconnect SSE/WS and replay. tableflip also
  has no Windows support.
- **Crash supervision.** The parent does not health-check or auto-restart a
  crashed worker; restart is always a deliberate request. A non-restart exit
  code propagates and the parent exits with it.
- **Fetching or building binaries.** The restart machinery picks up whatever
  binary is at the path; producing that binary (e.g. the agent running
  `make build`, or a future self-update command) is outside this design.

## Process model

`octo serve` runs in supervisor mode by default. The parent:

1. Spawns the worker: same binary path (captured via `os.Executable` at
   startup), same argv, with `OCTO_SERVE_WORKER=1` in the environment.
   stdin/stdout/stderr pass through, so logs look exactly like today.
2. Waits for the worker to exit.
3. Dispatches on the exit code:
   - `42` (`server.ExitRestart`) — re-spawn from the stored binary path and
     loop. The path is re-opened on each spawn, so a replaced binary takes
     effect.
   - anything else — exit with the same code. `0` stays `0`, crashes
     propagate.

When `OCTO_SERVE_WORKER=1` is set, `runServe` skips the supervisor and runs
the server directly — this is both how the parent's child runs and the opt-out
for users who run octo under systemd/launchd and want their own supervision
(`RestartForceExitStatus=42` / `KeepAlive` then own the respawn). A
`--no-supervisor` flag sets the same mode explicitly.

### Signals

On POSIX, terminal Ctrl-C delivers SIGINT to the whole foreground process
group, so parent and worker both receive it. The parent treats a received
SIGINT/SIGTERM as "quitting": it stops respawning, waits for the worker (whose
existing signal handler runs `Shutdown`), and exits with the worker's code.
When the parent is signalled directly (e.g. `kill <parent-pid>`), it forwards
the signal to the worker. On Windows the parent kills the worker process on
interrupt; the drain guarantees below make that safe.

## Restart triggers

- `POST /api/restart` — `requireAuth`, responds `202 Accepted` immediately,
  then starts the drain. This is the endpoint the web UI calls.
- `restart_server` tool — registered only by the server, injected through the
  same setter pattern as the WebSocket asker (`tools.SetAsker`), so the CLI
  and sub-agents never see it. The tool returns success text immediately
  ("restart scheduled after this turn"); the drain naturally waits for the
  calling turn to finish, so the agent's final reply ("restarting now…")
  reaches the user before the connection drops.

The tool goes through the per-turn permission gate like any other
state-changing tool; on IM channels the interactive ask flow covers
confirmation.

## Drain sequence

A restart request flips the server into draining state:

1. **Stop intake.** New turn requests get `503` with a retry hint; IM
   adapters stop (`stopChannels`) so no new IM turns start.
2. **Wait for in-flight turns**, tracked by the existing `turnRunning` map,
   bounded by a drain timeout (default 30s).
3. **Shutdown.** The existing `Shutdown` path runs: kill background
   processes, kill session sub-agents, MCP cleanup, `http.Shutdown`.
4. `ListenAndServe` returns `ErrRestartRequested`; `runServe` returns exit
   code `42`.

The timeout is a hard bound, not a negotiation: a turn still running at 30s is
abandoned. Round-granularity session persistence plus the SSE replay buffer
already cap the damage at one round, and clients reconnect to the new worker —
restart inherits the crash-durability guarantees rather than building its own.

## Upgrade flow

1. New binary lands at the path the parent stored at startup.
   - POSIX: rename-over-running-binary is safe; the old process keeps its
     inode.
   - Windows: a running exe can't be overwritten but *can* be renamed — the
     upgrade step renames `octo.exe` → `octo.exe.old`, moves the new binary
     into place, and the next worker start removes a leftover `.old`.
2. Restart is triggered (API or tool).
3. The parent re-spawns from the path; the new binary serves.

The parent itself never upgrades mid-flight — it is a wait-loop of a few dozen
lines and is expected to be version-compatible across worker upgrades. A
parent-side change requires a full stop/start, which is fine: that's the
frequency of supervisor changes, not of worker changes.

## Config change flow

The worker resolves provider, model, system prompt, skills, env context, and
memory dirs once in `server.New`; channels come from `channels.yml` at
`startChannels`. A restart re-runs all of it. The parent re-spawns with the
original argv, so flag-level settings survive the restart unchanged while
file-based config is re-read. Config surfaces that already hot-reload (the MCP
panel rewriting `mcp.json`) keep working without a restart; restart is the
catch-all for everything else.

## Components

| Piece | Where | What |
|---|---|---|
| Supervisor loop | `cmd/octo/serve.go` | spawn/wait/dispatch, signal handling, `OCTO_SERVE_WORKER` detection, `--no-supervisor` |
| Exit code | `internal/server` | `ExitRestart = 42`, `ErrRestartRequested` |
| Drain state | `internal/server/server.go` | draining flag, 503 gate, turn-drain wait, timeout |
| Restart endpoint | `internal/server` | `POST /api/restart` behind `requireAuth` |
| Restart tool | `internal/tools` + server wiring | `restart_server`, server-injected, permission-gated |

## Testing

- Drain logic: unit tests with fake in-flight turns covering finish-before-
  timeout and timeout-abandons paths.
- Supervisor loop: re-exec helper-process pattern (`TestMain` dispatching on
  an env var, the same trick `os/exec` tests use) — child exits 42 then 0,
  parent observed to respawn exactly once. No live network, per project rule.
- Endpoint: `httptest` — auth required, 202 response, draining 503 on
  subsequent turn requests.
- CI matrix (Linux/macOS/Windows) exercises the spawn/wait path on all three;
  the Windows rename dance is covered by the upgrade step's own test.
