package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// `octo completion <shell>` and the hidden `octo __complete <words…>` work
// together to provide TAB-completion in bash / zsh / fish.
//
// User wires it up by running, e.g.:
//
//	source <(octo completion bash)
//
// The script is tiny on purpose: it just delegates each TAB to
// `octo __complete <command-line-so-far>` and uses the returned newline-
// separated candidate list. All routing logic — which arg of which
// subcommand we're on, whether to emit IDs vs. flags vs. literal choices —
// lives in this Go file, so we maintain ONE flow instead of three diverging
// shell rewrites.
//
// Dynamic completion sources:
//   - session IDs (full + short) for `octo -c <TAB>`
// Both always include "last" as the first candidate.

// runCompletion handles `octo completion <shell>`: prints the shell snippet
// the user sources into their shell rc. Returns CLI exit code.
func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "Usage: octo completion bash | zsh | fish | powershell")
		return 2
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(stdout, bashCompletionScript)
	case "zsh":
		fmt.Fprint(stdout, zshCompletionScript)
	case "fish":
		fmt.Fprint(stdout, fishCompletionScript)
	case "powershell":
		fmt.Fprint(stdout, powershellCompletionScript)
	default:
		fmt.Fprintf(stderr, "octo completion: unsupported shell %q (want: bash, zsh, fish, powershell)\n", args[0])
		return 2
	}
	return 0
}

// runComplete is the hidden `octo __complete <words…>` invoked by the shell
// scripts. words is the full command line up to and including the partial
// word the user is completing — e.g. ["octo", "-c", "a3"]. The
// shell does prefix-matching against the printed candidates, so we don't
// filter by the partial here; we just emit every plausible candidate for
// the current position.
func runComplete(args []string, stdout io.Writer) int {
	for _, c := range completionCandidates(args) {
		fmt.Fprintln(stdout, c)
	}
	return 0
}

// completionCandidates is the pure-Go routing core, factored out so it's
// trivially unit-testable without going through stdout.
func completionCandidates(words []string) []string {
	// Position 0 is the program name; position 1 is the subcommand.
	if len(words) < 2 {
		return topLevelCommands
	}
	// Still typing the subcommand itself — offer the full top-level list
	// (shell will prefix-match against the partial). If the partial starts
	// with a dash the user is typing a flag, which means chat mode (a bare
	// octo runs the session directly).
	if len(words) == 2 {
		if strings.HasPrefix(words[1], "-") {
			return chatCandidates(words, "")
		}
		return topLevelCommands
	}
	prev := words[len(words)-2]
	cmd := words[1]

	switch cmd {
	case "memory":
		return memoryCandidates(words)
	case "sessions":
		// Takes no arguments; suggest nothing rather than chat flags.
		return nil
	case "skills":
		if len(words) == 3 {
			return []string{"list", "add", "update", "path"}
		}
		return nil
	case "init":
		return initCandidates(prev)
	case "config":
		if len(words) == 3 {
			return []string{"show", "path"}
		}
	case "help":
		// `octo help <TAB>` → list of help targets.
		if len(words) == 3 {
			return []string{"config", "memory", "init", "completion", "mcp"}
		}
	case "completion":
		if len(words) == 3 {
			return []string{"bash", "zsh", "fish", "powershell"}
		}
	}
	// No recognised subcommand: chat mode. Any positional text after "octo"
	// is treated as the chat message, so flags and their values still
	// complete as session flags.
	return chatCandidates(words, prev)
}

func chatCandidates(words []string, prev string) []string {
	// Value-completion for flags that take a known fixed set.
	switch prev {
	case "-c", "--continue":
		return sessionIDCandidates()
	case "--provider":
		return []string{"anthropic", "openai"}
	case "--permission-mode":
		return []string{"interactive", "strict", "auto"}
	case "--reasoning-effort":
		return []string{"low", "medium", "high"}
	case "--model", "--system", "--max-tokens", "--max-tokens-escalate", "--max-turns",
		"--compact-threshold", "--compact-auto-pct", "--compact-batch-threshold",
		"--sandbox-write", "--sandbox-read":
		// These take freeform values; nothing useful to suggest.
		return nil
	}
	// Default: offer the flag set for chat.
	_ = words
	return chatFlags
}

func memoryCandidates(words []string) []string {
	if len(words) == 3 {
		return []string{"list"}
	}
	if words[2] == "list" && len(words) == 4 {
		return []string{"--archive"}
	}
	return nil
}

func initCandidates(prev string) []string {
	switch prev {
	case "--provider":
		return []string{"anthropic", "openai"}
	case "--permission-mode":
		return []string{"interactive", "strict", "auto"}
	}
	return initFlags
}

// sessionIDCandidates returns "last" plus the short + full IDs of every
// saved session (up to 50, newest first — beyond that the user can paste
// the full ID directly). Errors are swallowed: completion never fails the
// command line, it just offers fewer suggestions.
func sessionIDCandidates() []string {
	out := []string{"last"}
	sessions, err := agent.ListSessions(50)
	if err != nil {
		return out
	}
	for _, s := range sessions {
		out = append(out, s.ShortID(), s.ID)
	}
	return out
}

// completionHelp prints the user-facing instructions for `octo help
// completion`. Kept here rather than in help.go so the strings sit next to
// the scripts they reference.
func completionHelp(w io.Writer) {
	fmt.Fprintln(w, strings.TrimSpace(`
octo completion — print the shell-completion snippet for bash, zsh, fish, or powershell.

Examples:
  source <(octo completion bash)            # one-shot for this shell session
  octo completion bash > ~/.octo/octo.bash  # permanent: source from .bashrc
  octo completion zsh  > ~/.octo/_octo      # zsh: drop into fpath and run compinit
  octo completion fish > ~/.config/fish/completions/octo.fish
  octo completion powershell | Out-String | Invoke-Expression   # PowerShell; add to $PROFILE to persist

What it completes:
  - Top-level subcommands (config, memory, init, …) and session flags.
  - Subcommands of memory / help / completion.
  - Session IDs after "octo -c " — full + short + "last".
  - Fixed values for --provider (anthropic|openai) and --permission-mode
    (interactive|strict|auto).

The shell snippet just delegates to the hidden "octo __complete" subcommand;
the routing logic lives in the binary, so the same snippet keeps working as
new flags / subcommands are added.`))
	fmt.Fprintln(w)
}

// ── Static lists ─────────────────────────────────────────────────────────

var topLevelCommands = []string{
	"config", "init", "memory", "sessions", "skills",
	"version", "help", "completion",
}

// chatFlags + initFlags are intentionally not the full flag list — we ship
// the most useful ones for TAB completion. Long-tail flags (e.g.
// --max-tokens-escalate) still work; users just type them in full. Keeping the
// list focused avoids drowning the completion popup with rarely-used flags.
var chatFlags = []string{
	"-c", "--continue", "--tools", "--no-tools", "--provider", "--model",
	"--no-save", "--no-memory", "--no-suggest", "--sandbox", "--sandbox-allow-net",
	"--permission-mode",
	"--quiet", "--verbose", "--plain", "--stream", "--system",
	"--reasoning-effort", "--show-reasoning",
	"--compact-auto-pct", "--compact-batch-threshold",
	"--help",
}

var initFlags = []string{
	"--provider", "--model", "--plain", "--sandbox",
	"--sandbox-allow-net", "--permission-mode", "--help",
}

// ── Shell scripts ────────────────────────────────────────────────────────

const bashCompletionScript = `# octo bash completion. Generated by 'octo completion bash'.
# Install: source <(octo completion bash)
#   or:    octo completion bash > /usr/local/etc/bash_completion.d/octo
_octo_completions() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local words=("${COMP_WORDS[@]:0:$((COMP_CWORD+1))}")
    local IFS=$'\n'
    local candidates
    candidates=$(octo __complete "${words[@]}" 2>/dev/null)
    COMPREPLY=( $(compgen -W "$candidates" -- "$cur") )
}
complete -F _octo_completions octo
`

const zshCompletionScript = `#compdef octo
# octo zsh completion. Generated by 'octo completion zsh'.
# Install: octo completion zsh > "${fpath[1]}/_octo"  (and run 'compinit')
_octo() {
    local -a candidates
    candidates=( ${(f)"$(octo __complete "${(@)words[1,CURRENT]}" 2>/dev/null)"} )
    _describe 'octo' candidates
}
compdef _octo octo
`

const fishCompletionScript = `# octo fish completion. Generated by 'octo completion fish'.
# Install: octo completion fish > ~/.config/fish/completions/octo.fish
function __octo_complete
    octo __complete (commandline -opc) (commandline -ct) 2>/dev/null
end
complete -c octo -f -a '(__octo_complete)'
`

const powershellCompletionScript = `# octo PowerShell completion. Generated by 'octo completion powershell'.
# Install (this session): octo completion powershell | Out-String | Invoke-Expression
# Persist: add that line to your profile ($PROFILE).
Register-ArgumentCompleter -Native -CommandName octo -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    # Pass the command line up to (and including) the partial word, matching the
    # bash/zsh scripts. When the cursor is after a trailing space the partial is
    # empty and is not yet an AST element, so append it; otherwise the partial is
    # already the last element. The binary emits every candidate for the
    # position; PowerShell prefix-filters here.
    $elements = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })
    if ($wordToComplete -eq '') { $elements += '' }
    (octo __complete @elements 2>$null) |
        Where-Object { $_ -like "$wordToComplete*" } |
        ForEach-Object {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
}
`
