# After-turn follow-up suggestion

After each turn the TUI offers **one** LLM-generated follow-up — a next message
the user might send — as **ghost text** in the empty input box. `Tab` or `→`
accepts it (fills the input to edit or send); typing or starting a turn
dismisses it. On by default; `--no-suggest` / `OCTO_SUGGEST=0` turns it off.

## Generation

`Agent.Suggest(ctx, tools)` (internal/agent) makes a throwaway provider call: it
appends a one-line instruction to a **snapshot** of history (never the live
`History`) and asks for a single next user message, capped at `suggestMaxTokens`.
Because it's a snapshot, the conversation isn't polluted. The result is passed
through `cleanSuggestion` (first non-empty line, list/quote decoration stripped).
Its usage is intentionally **not** accrued into the session — it's an auxiliary
UI call, not part of a turn. Returns `""` (no error) when there's nothing to
suggest.

**Cache alignment is the whole reason it's cheap.** Anthropic's cache prefix is
ordered `tools → system → messages`. The agentic loop sends a tools block, so if
Suggest sent *no* tools (a plain `SendMessages`) its prefix would diverge at
block 0 and the entire history would be re-billed at full input price every
turn. So Suggest takes the **same `tools`** the loop uses and routes through the
`ToolSender` path: the `tools → system → history` prefix matches, the whole
history is reused at the cheap cache-read rate, and only the instruction plus the
one-line reply are fresh tokens. The instruction tells the model not to call
tools; if it returns a `tool_use` anyway, `Content` is empty and that turn simply
yields no suggestion. The TUI passes `cfg.tools`; with no ToolSender/tools,
Suggest falls back to `SendMessages` (uncached but still correct).

## TUI wiring (cmd/octo)

- On a clean `turnEndedMsg` (no error/interrupt) and when `cfg.suggest` is set,
  the model dispatches `suggestCmd()` — a `tea.Cmd` that runs `Suggest` off the
  event loop and returns a `suggestionMsg` (nil on empty/error, which bubbletea
  ignores).
- `suggestionMsg` is applied only while **idle with an empty input**, so a stale
  suggestion never clobbers in-progress typing or a running turn.
- Ghost text reuses the textarea **placeholder**: `setSuggestion` swaps the
  idle "Ask anything…" hint for the suggestion (one line); `clearSuggestion`
  restores it. `acceptSuggestion` fills the value and clears.
- `handleKey` intercepts `Tab`/`→` to accept **only when the input is empty**;
  with text present those keys keep their normal behaviour. Starting a turn
  (`startTurnEcho`) clears any pending suggestion.

Plain/headless mode is untouched: the suggestion is a TUI ghost-text affordance,
and the plain path has nowhere to render it.

## Constraints

- Only generated in the interactive TUI; one suggestion at a time.
- No persistence; no extra model call when disabled.
- Does not change `agent.AgentEvent` or the turn loop — it's a post-turn side
  call plus input-box rendering.
