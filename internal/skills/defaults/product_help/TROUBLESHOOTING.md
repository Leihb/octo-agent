# Troubleshooting

Common issues and their fixes.

## Installation

### `octo: command not found`

- Ensure the binary is on your `$PATH`:
  ```bash
  export PATH="$PATH:/path/to/octo/bin"
  ```
- Or use the full path: `./octo`

### macOS "cannot be opened because the developer cannot be verified"

- Right-click the binary → Open, or run:
  ```bash
  xattr -d com.apple.quarantine /path/to/octo
  ```

## Configuration

### `octo` says "no API key"

- Check that the env var is set: `echo $ANTHROPIC_API_KEY` (or `OPENAI_API_KEY`)
- Or run `octo config` to store the key in `~/.octo/config.yaml` (mode 0600)
- Check `octo config show` to see where the provider/model resolve from

### Provider/model not what I expected

Precedence (highest first): CLI flag > env var > config file > built-in default.

- `--provider` / `--model` flags override everything
- `OCTO_PROVIDER` / `ANTHROPIC_MODEL` env vars override the config file
- The config file's model is only honored when the resolved provider matches

## TUI

### TUI doesn't start (falls back to plain REPL)

- TUI requires a TTY. Piped input (`echo "hello" | octo`) runs headless
- Check `octo --help` for `--no-tui` or TTY-related flags

### Image paste (Ctrl+V) doesn't work

- Only works on **macOS** today (uses AppleScript to read clipboard)
- Ensure the clipboard actually contains an image, not a file reference
- Try dragging the image into the terminal instead

### Input history not persisting

- History is saved to `~/.octo/history` (or `$OCTO_HISTORY_FILE`)
- Ensure `~/.octo` is writable

## MCP

### "No MCP servers connected"

- Verify `~/.octo/mcp.json` exists and is valid JSON
- Check that the server command is on `$PATH` (or use absolute path)
- Run with `--verbose` to see connection errors

### MCP server keeps disconnecting

- Some servers (especially stdio-based) crash on malformed input — check their logs
- OAuth servers: delete `~/.octo/mcp-tokens/<server>.json` to force re-auth

## Permissions

### "permission_denied" on safe operations

- Check current mode with `Shift+Tab` (cycles interactive → strict → auto)
- In strict mode, all unmatched operations are denied — switch to interactive
- Or add an allow rule to `~/.octo/permissions.yml`

### Accidentally denied "always allow this session"

- Restart octo — session memory is cleared on exit
- Or switch permission mode to auto (Shift+Tab) temporarily

## Sessions

### Session not found when resuming

- Sessions are saved to `~/.octo/sessions/`
- Resume with `octo -c <session-id>`
- List recent sessions with `/sessions` in TUI

### Session file corrupted

- Sessions are JSON — validate with `python3 -m json.tool ~/.octo/sessions/<id>.json`
- Corrupted sessions are skipped at load; remove the file to clear the error

## Performance

### Slow first turn / long "Thinking" spinner

- Large repos: the initial `.octorules` + file tree scan can take time
- Use `--quiet` to reduce rendering overhead
- Check context usage in the status bar — high ctx% triggers compaction which pauses the stream

### "Context too long" error

- History compaction runs automatically when context usage is high
- If it still fails, start a new session (`/exit` then `octo`)
- Or use a model with a larger context window

## Git

### Co-authored-by not appearing

- Check `octo config show` — `coauthor` should be "on"
- Only applies to commits written by octo (via `terminal: git commit`)
- The trailer is appended automatically; if you edit the message manually, it may be lost

## Getting help

- Run `octo help <command>` for detailed command docs
- Run `/help` in the TUI for keyboard shortcuts
- Check the status bar hints — they change based on current state
