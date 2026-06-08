package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"

	"github.com/Leihb/octo-agent/internal/sandbox"
	"github.com/Leihb/octo-agent/internal/trash"
)

// activeSandbox, when non-nil, confines every terminal command (foreground and
// background) to the given policy. nil means no OS sandbox — the default.
// Set once at startup via SetSandbox; mirrors the package-level defaultBg.
var activeSandbox *sandbox.Policy

// SetSandbox enables OS-level command confinement for the terminal tools.
// Pass nil to disable. cmd/octo calls this when --sandbox is requested.
func SetSandbox(p *sandbox.Policy) { activeSandbox = p }

// NetworkAllowed reports whether the active sandbox policy permits network
// access. When no sandbox is active (nil policy) network is allowed.
func NetworkAllowed() bool {
	if activeSandbox == nil {
		return true
	}
	return activeSandbox.AllowNetwork
}

// safeRmWrapper is injected before every POSIX shell command so that direct
// `rm` invocations move files to the project-scoped trash instead of
// permanently deleting them.  It reads $OCTO_TRASH_DIR (set by shellCommand)
// and writes .meta.json sidecars compatible with the trash package.
const safeRmWrapper = `__octo_safe_rm() {
  local _trash_dir="$OCTO_TRASH_DIR"
  [ -z "$_trash_dir" ] && return
  local _arg
  for _arg in "$@"; do
    case "$_arg" in -*) continue ;; esac
    if [ -e "$_arg" ] || [ -L "$_arg" ]; then
      local _ts _base _dest _orig
      _ts=$(date +%%Y%%m%%d-%%H%%M%%S)
      _base=$(basename "$_arg")
      _dest="$_trash_dir/${_ts}_${_base}"
      _orig="$_arg"
      case "$_orig" in /*) ;; *) _orig="$PWD/$_orig" ;; esac
      mkdir -p "$_trash_dir"
      cp -r "$PWD/$_arg" "$_dest" 2>/dev/null || continue
      printf '{"original":"%%s","deleted_at":"%%s","project":"%%s"}\n' \
        "$(printf '%%s' "$_orig" | sed 's/\\/\\\\/g; s/"/\\"/g')" \
        "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" \
        "$(printf '%%s' "$PWD" | sed 's/\\/\\\\/g; s/"/\\"/g')" \
        > "$_dest.meta.json"
    fi
  done
}
rm() { __octo_safe_rm "$@"; command rm "$@"; }
%s
`

// shellCommand builds the *exec.Cmd that runs `command` via the shell, wrapped
// in the active sandbox when one is set. Both TerminalTool and the background
// manager route through here so confinement is uniform.
//
// Shell by platform: POSIX `sh -c` on macOS/Linux; PowerShell on Windows (the
// OS sandbox is macOS/Linux-only, so the Windows branch is never sandboxed).
func shellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	if activeSandbox != nil {
		cmd, err := sandbox.Command(ctx, command, *activeSandbox)
		if err == nil && cmd != nil {
			applyWorkingDir(ctx, cmd)
		}
		return cmd, err
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// -NoProfile: reproducible env (don't run the user's $PROFILE).
		// -NonInteractive: never block on a PowerShell prompt mid-command.
		cmd = exec.CommandContext(ctx, resolvePowerShell(), "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		projectDir := WorkingDir(ctx)
		if projectDir == "" {
			projectDir, _ = os.Getwd()
		}
		if projectDir != "" {
			trashDir := trash.ProjectDir(projectDir)
			wrapped := fmt.Sprintf(safeRmWrapper, command)
			cmd = exec.CommandContext(ctx, "sh", "-c", wrapped)
			cmd.Env = append(os.Environ(), "OCTO_TRASH_DIR="+trashDir)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", command)
		}
	}
	if attr := setProcessGroupOpts(); attr != nil {
		cmd.SysProcAttr = attr
	}
	applyWorkingDir(ctx, cmd)
	return cmd, nil
}

// applyWorkingDir roots the command in the conductor-stamped working directory
// (a unit's worktree) when one is present in ctx. No-op otherwise, so every
// other caller keeps running in the process CWD.
func applyWorkingDir(ctx context.Context, cmd *exec.Cmd) {
	if dir := WorkingDir(ctx); dir != "" {
		cmd.Dir = dir
	}
}

// resolvePowerShell picks the Windows shell once: PowerShell 7+ (`pwsh`) when
// present — it's the modern, cross-platform build and supports `&&`/`||`
// pipeline chaining — else Windows PowerShell 5.1 (`powershell`), which ships
// with every supported Windows and is always available as the fallback.
var resolvePowerShell = sync.OnceValue(func() string {
	if path, err := exec.LookPath("pwsh"); err == nil {
		return path
	}
	return "powershell"
})
