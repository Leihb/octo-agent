# Output-truncation recovery

When a model response is cut off by the per-response output-token cap
(`max_tokens`), the agent loop **retries the same round once at a higher cap**
instead of ending the turn with a discarded, half-formed response. This is what
lets a single large artifact — a full HTML page emitted as one `write_file` tool
call — land even when it exceeds the default cap.

## The failure it fixes

A tool call's arguments are model **output tokens**: when octo writes a file,
the model emits the entire file content as the `write_file` call's argument JSON.
A large file plus any preceding text can exceed the output cap, so the response
is truncated mid-JSON. Two things then conspire:

- The provider reports truncation, not a tool call — `finish_reason="length"`
  (OpenAI) / `stop_reason="max_tokens"` (Anthropic), never `tool_use`.
- The truncated argument JSON is unparseable, so the assembled tool-use block
  carries empty input (`openai/stream.go` ignores the unmarshal error).

The loop only continues on `tool_use`, so a truncated turn fell through to the
"final reply" branch and ended — the half-written file never dispatched, nothing
landed. On large artifacts this is intermittent: it depends on how verbose the
model is that run.

## Design

### One canonical truncation sentinel

Adapters normalise their wire value to a single agent-level constant,
`StopReasonMaxTokens = "max_tokens"`, the same way they already map
`tool_calls`/`tool_use`:

| Provider | wire value | normalised to |
|---|---|---|
| Anthropic | `stop_reason: "max_tokens"` | `"max_tokens"` (unchanged) |
| OpenAI    | `finish_reason: "length"`   | `"max_tokens"` |

The loop then checks one thing.

### Escalate-and-retry, from the same history

After each model call, if the reply is truncated and an escalation cap is
configured above the current cap, the loop re-issues the **same round** at the
escalated cap. Crucially it retries from the **unchanged history** — the
truncated reply is never appended — so a half-written tool call is simply
regenerated with more room. This sidesteps the hard, provider-specific problem
of resuming a partial `tool_use` block (OpenAI-compatible backends reject a
malformed tool call in history); no partial state is ever kept.

Escalation fires **at most once per round**. Each model call's usage is
accounted (both the truncated attempt and the retry cost real tokens).

### Provider-aware escalation default

octo has no per-model capability table, so the escalation target is a
conservative default per protocol, overridable by flag/env:

| Provider | first attempt | escalation target |
|---|---|---|
| OpenAI protocol | provider default (4096) | 16384 |
| Anthropic protocol | provider default (4096) | 32768 |

- `--max-tokens-escalate N` / `OCTO_MAX_TOKENS_ESCALATE=N` overrides it;
  `0` disables escalation entirely.
- Escalation only ever raises the cap: if the caller already set `--max-tokens`
  above the escalation target, no escalation happens.

**Model-ceiling backoff.** Some models cap below the escalation target (Claude 3
tops out at 4096). Escalating past a model's ceiling returns a `max_tokens`
error; the loop detects it (`isMaxTokensTooLargeErr`, a best-effort match on the
provider error text), keeps the original truncated reply, and falls through to a
graceful stop rather than failing the turn.

### Graceful stop when still truncated

If escalation is disabled, hits the model ceiling, or the escalated cap is still
not enough, the loop ends the turn through the existing `budgetStop` path with
`StopReason` `max_tokens` and a clear message — history keeps the progress, the
caller gets a non-error explanation, and no half-formed tool call is dispatched.

## Seam

The provider call in `runLoop` (`internal/agent/agent.go`) goes through a `send`
closure. Its signature carries the per-call cap so the loop can re-issue at a
different value:

```
send func(ctx, msgs []Message, maxTokens int) (Reply, error)
```

`Run` and `RunStream` build the closure over the active `ToolSender` /
`ToolStreamingSender`, passing the cap straight through to the provider request.
The escalation policy lives entirely in `runLoop`; the `Sender` interfaces are
unchanged, so provider adapters need no new methods.

## Known limitation (streaming re-emit)

In the streaming path the truncated attempt has already emitted its deltas to the
handler before the retry runs, so an escalated retry re-streams. For the primary
case — a `write_file` call, whose content surfaces as a tool card rather than
prose — the visible result is the final file, so impact is minor. Suppressing
the re-emit is left to a later change.

## Not yet implemented: resume-and-chunk

A second tier — keep the partial output, feed back a "you were cut off; resume
mid-thought and write in smaller pieces" message, and continue (à la Claude
Code's multi-turn recovery) — is **not** built. It needs per-provider handling
of the partial `tool_use` block (safe on Anthropic, needs cleanup on
OpenAI-compatible backends) and is a separate change. Layer 1 (escalate-retry)
covers the common large-artifact case without it.
```
