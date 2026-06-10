package tools

import "runtime"

// shellEnvNoteWindows is the Windows shell guidance injected into the session
// environment context. It lives next to shellCommand (sandbox.go) — the code
// that actually picks PowerShell — so the behavior and its description can't
// drift apart, and is shared by the CLI and server context builders.
//
// Beyond dialect, it covers the three traps that strand a session that
// installs a tool mid-conversation:
//   - each terminal command inherits octo's startup PATH, so a fresh install
//     is invisible until PATH is refreshed in-command or octo restarts;
//   - installers raise a UAC dialog only a user at the screen can approve;
//   - the default execution policy blocks Node's npm.ps1 shim.
const shellEnvNoteWindows = "- Shell: PowerShell. Use PowerShell syntax and cmdlets " +
	"(Get-ChildItem, Get-Content, Select-String, Remove-Item, $env:VAR), not POSIX sh. " +
	"Chain commands with `;` rather than `&&` (Windows PowerShell 5.1 lacks `&&`). " +
	"Prefer the built-in read_file / glob / grep tools over shelling out — they're identical across platforms.\n" +
	"- Installing tools mid-session: every terminal command runs in a fresh shell inheriting octo's " +
	"startup PATH, so a tool installed via winget/MSI is NOT found afterwards even though the install " +
	"succeeded. Refresh PATH inside the same command — " +
	"`$env:Path = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User')` " +
	"— or invoke the binary by full path (e.g. `& \"$env:ProgramFiles\\nodejs\\node.exe\" -v`). " +
	"If oddities persist, ask the user to restart octo.\n" +
	"- Installers raise a UAC elevation dialog the user must approve at the screen — warn them to expect it. " +
	"When winget is unavailable (older Windows 10) or the user is a novice, prefer handing them a download " +
	"link (e.g. the nodejs.org LTS installer) to click through the GUI installer, then verify afterwards.\n" +
	"- The default PowerShell execution policy blocks Node's npm.ps1 shim ('running scripts is disabled') — " +
	"invoke `npm.cmd` / `npx.cmd` explicitly instead.\n"

// shellEnvNoteDarwin covers the macOS equivalents: sudo prompts hang the
// non-interactive shell, mid-session installs miss this session's PATH
// (Apple Silicon Homebrew lives in /opt/homebrew/bin), a fresh Mac pops the
// Xcode CLT dialog on first git/compiler use, and novices do better with a
// GUI .pkg than with installing Homebrew first.
const shellEnvNoteDarwin = "- Shell notes (macOS): never run `sudo` through the terminal tool — the password " +
	"prompt can't be answered here and the command hangs; use a GUI installer or Homebrew instead. " +
	"A tool installed mid-session may not be on this session's PATH (commands inherit octo's startup " +
	"environment; Apple Silicon Homebrew installs to /opt/homebrew/bin) — invoke it by full path " +
	"(e.g. `/opt/homebrew/bin/node -v`) or ask the user to restart octo. " +
	"On a fresh Mac the first `git`/compiler use pops the Xcode Command Line Tools install dialog while " +
	"the command itself fails — tell the user to click Install, wait for it to finish, then retry. " +
	"For novice users without Homebrew, hand them a GUI installer link (e.g. the nodejs.org LTS .pkg) " +
	"rather than installing Homebrew first.\n"

// ShellEnvNote returns platform-shell guidance for the session environment
// context, or "" on platforms where the default POSIX assumptions hold.
func ShellEnvNote() string {
	switch runtime.GOOS {
	case "windows":
		return shellEnvNoteWindows
	case "darwin":
		return shellEnvNoteDarwin
	}
	return ""
}
