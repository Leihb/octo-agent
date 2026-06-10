---
name: cron-task-creator
description: Create, inspect, run, enable/disable, and delete octo's scheduled cron tasks — recurring agent prompts stored in ~/.octo/tasks/*.json and executed by the octo serve scheduler. Use when the user wants to schedule a recurring task, e.g. "run X every morning", "schedule a daily report", "set up a cron job", "定时任务", "每天自动跑".
---

# Create and manage octo cron tasks

octo can run an agent prompt on a schedule. Each scheduled task is a JSON file
in `~/.octo/tasks/`, loaded by the scheduler inside `octo serve`. When a task
fires, the scheduler runs one agent turn with the task's prompt (30-minute
timeout) and reuses the same session across runs, so the task accumulates
history from previous executions.

## Task schema

| Field | Required | Meaning |
|-------|----------|---------|
| `name` | yes | Human-readable task name (also addressable via the API) |
| `cron` | yes | Schedule expression — see format below |
| `prompt` | yes | The prompt sent to the agent on each run |
| `model` | no | Model override; defaults to the server's model |
| `agent` | no | `"general"` or `"coding"` |
| `directory` | no | Working directory hint, prepended to the task session's system prompt |
| `enabled` | yes | Whether the schedule is active |

## Cron expression format — 6 fields, seconds first

The scheduler uses robfig/cron **with a seconds field**. A standard 5-field
crontab line is **invalid** here — always prepend a seconds field:

```
seconds minutes hours day-of-month month day-of-week
```

| Want | Expression |
|------|------------|
| Every day at 09:00 | `0 0 9 * * *` |
| Every 30 minutes | `0 */30 * * * *` |
| Weekdays at 18:30 | `0 30 18 * * 1-5` |
| 1st of each month at 08:00 | `0 0 8 1 * *` |

Descriptors also work: `@hourly`, `@daily`, `@weekly`, `@every 90m`.
Times are interpreted in the server's local timezone.

## Workflow

1. **Gather** the schedule, the prompt, and any optional fields. If the user
   gave a vague schedule ("every morning"), pick a concrete time and confirm.
2. **Translate** the schedule to a 6-field expression and **echo it back in
   plain words** ("every weekday at 18:30") before creating anything.
3. **Write a self-contained prompt.** The task session has no access to this
   conversation — the prompt must carry all context: what to do, where, and
   what the output should look like.
4. **Create** the task (see below), then **verify** by listing tasks. Offer a
   one-off immediate run to test.

## Creating a task

**Preferred — via the running server.** If `octo serve` is up (default
`:8080`), POST to the API; the task is registered and starts firing
immediately:

```bash
curl -s -X POST http://127.0.0.1:8080/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{"name":"daily-report","cron":"0 0 9 * * *","prompt":"Summarize ..."}'
# → {"id":"task_1717999999999"}
curl -s http://127.0.0.1:8080/api/tasks   # verify
```

**Fallback — direct file write.** If the server is not running, write
`~/.octo/tasks/<id>.json` with `write_file` (id format: `task_<unix-millis>`,
filename must equal `<id>.json`):

```json
{
  "id": "task_1717999999999",
  "name": "daily-report",
  "cron": "0 0 9 * * *",
  "prompt": "Summarize ...",
  "enabled": true,
  "created_at": "2026-06-10T09:00:00Z"
}
```

The file is picked up the next time `octo serve` starts.

## Other operations

```bash
curl -s http://127.0.0.1:8080/api/tasks                          # list
curl -s -X POST   http://127.0.0.1:8080/api/tasks/{id}/run      # run now
curl -s -X DELETE http://127.0.0.1:8080/api/tasks/{id}          # delete
curl -s -X PATCH  http://127.0.0.1:8080/api/cron-tasks/{name} \
  -H 'Content-Type: application/json' -d '{"enabled":false}'    # disable
```

## Caveats — tell the user when relevant

- **Tasks only fire while `octo serve` is running.** No daemon, no serve → no
  runs. Missed schedules are not replayed on restart.
- **API changes take effect immediately; file edits don't.** Create, update,
  enable/disable, and delete through the API reschedule the running process on
  the spot. Editing a JSON file under `~/.octo/tasks/` by hand only takes
  effect the next time `octo serve` starts — prefer the API whenever the
  server is up.
- **Validate before creating.** A malformed cron expression is rejected at
  creation time by the API, but a hand-written JSON file with a bad expression
  fails silently at load (logged to stderr only) — double-check the 6-field
  format when writing files directly.
