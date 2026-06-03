# TUI (Terminal User Interface) Reference

octo's TUI is a bubbletea-based interactive interface launched by `octo chat` with no positional argument. It provides multi-turn conversation, live tool rendering, and rich keyboard shortcuts.

## Slash commands

Type `/` to open the completion menu (Tab/↑/↓ to navigate, Enter to accept). Available commands:

| Command | Description |
|---------|-------------|
| `/help` | Show this help message |
| `/init` | Analyze repo and generate/update `.octorules` (needs `--tools`) |
| `/save` | Save the session now (also auto-saves after each turn) |
| `/sessions` | List the 10 most recent sessions |
| `/skills` | List available skills (trigger with `/<name>`) |
| `/memory` | List what's remembered across sessions |
| `/mcp` | Show connected MCP servers and their surfaces |
| `/conduct` | Conduct a goal to completion (`/conduct list`, `/conduct resume <id>`) |
| `/exit` or `/quit` | Save and exit |

## Keyboard shortcuts

### Input editing

| Key | Action |
|-----|--------|
| **Enter** | Submit message / start turn |
| **Alt+Enter** / **Shift+Enter** / **Ctrl+J** | Insert newline in textarea |
| **↑** / **↓** | Browse input history (when cursor is on first/last line) or navigate lines |
| **Ctrl+V** | Paste image from clipboard as attachment |
| **Esc** | Clear input + discard attachments (idle); interrupt running turn; take back unsent message |

### Turn control

| Key | Action |
|-----|--------|
| **Ctrl+C** | Interrupt running turn; quit when idle |
| **Ctrl+D** | Quit (save and exit) |
| **Ctrl+Q** | Queue current input to run as a future turn |
| **Ctrl+X** | Cancel the most recently queued message (repeat to clear queue) |
| **Esc** (mid-turn, no output yet) | Take back — restore message to input box |
| **Esc** (mid-turn, with output) | Interrupt — stop the current turn |

### Permission mode

| Key | Action |
|-----|--------|
| **Shift+Tab** | Cycle permission mode: interactive → strict → auto → interactive |

### Completion menu

| Key | Action |
|-----|--------|
| **Tab** / **↓** | Next item |
| **↑** | Previous item |
| **Enter** | Accept selected item |
| **Esc** | Dismiss menu |
| **Type `/`** | Open completion menu (lists all commands and skills) |

### Suggestion (ghost text)

| Key | Action |
|-----|--------|
| **Tab** / **→** | Accept the pending follow-up suggestion |
| **Type anything** | Dismiss suggestion |

### Permission prompt modal

| Key | Action |
|-----|--------|
| **y** | Allow this once |
| **a** | Allow for this session (remembered) |
| **n** / **Esc** | Deny |

### Question modal

| Key | Action |
|-----|--------|
| **↑** / **↓** or **j** / **k** | Move selection |
| **Space** | Toggle multi-select option |
| **Enter** | Confirm selection |
| **Esc** | Cancel |

## Drag and drop

Drag an image file into the terminal — octo detects the path and attaches it automatically (works on terminals that paste the file path as text).

## Status bar

The bottom status bar shows:
- **cwd** — current working directory (abbreviated with `~`)
- **ctx** — context window usage percentage
- **perm** — current permission mode (interactive / strict / auto)
- **elapsed** — turn duration (while running)

Contextual hints appear below the status bar depending on state (e.g. "Enter steer · Ctrl+Q queue · Esc interrupt").
