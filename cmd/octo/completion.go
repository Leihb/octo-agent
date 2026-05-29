package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/taskgraph"
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
//   - session IDs (full + short) for `octo chat -c <TAB>`
//   - task IDs (full + short) for `octo goal status|show|resume|cancel|run <TAB>`
// Both always include "last" as the first candidate.

// runCompletion handles `octo completion <shell>`: prints the shell snippet
// the user sources into their shell rc. Returns CLI exit code.
func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "Usage: octo completion bash | zsh | fish")
		return 2
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(stdout, bashCompletionScript)
	case "zsh":
		fmt.Fprint(stdout, zshCompletionScript)
	case "fish":
		fmt.Fprint(stdout, fishCompletionScript)
	default:
		fmt.Fprintf(stderr, "octo completion: unsupported shell %q (want: bash, zsh, fish)\n", args[0])
		return 2
	}
	return 0
}

// runComplete is the hidden `octo __complete <words…>` invoked by the shell
// scripts. words is the full command line up to and including the partial
// word the user is completing — e.g. ["octo", "chat", "-c", "a3"]. The
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
	// (shell will prefix-match against the partial).
	if len(words) == 2 {
		return topLevelCommands
	}
	prev := words[len(words)-2]
	cmd := words[1]

	switch cmd {
	case "chat":
		return chatCandidates(words, prev)
	case "goal":
		return taskCandidates(words, prev)
	case "memory":
		return memoryCandidates(words)
	case "init":
		return initCandidates(prev)
	case "config":
		if len(words) == 3 {
			return []string{"show", "path"}
		}
	case "help":
		// `octo help <TAB>` → list of help targets.
		if len(words) == 3 {
			return []string{"chat", "config", "goal", "memory", "init", "completion", "mcp"}
		}
	case "completion":
		if len(words) == 3 {
			return []string{"bash", "zsh", "fish"}
		}
	}
	return nil
}

func chatCandidates(words []string, prev string) []string {
	// Value-completion for flags that take a known fixed set.
	switch prev {
	case "-c", "--continue":
		return sessionIDCandidates()
	case "--provider":
		return []string{"anthropic", "openai"}
	case "--permission-mode":
		return []string{"interactive", "strict"}
	case "--model", "--system", "--max-tokens", "--max-turns",
		"--max-cost", "--compact-threshold", "--thinking-budget",
		"--sandbox-write", "--sandbox-read":
		// These take freeform values; nothing useful to suggest.
		return nil
	}
	// Default: offer the flag set for chat.
	_ = words
	return chatFlags
}

func taskCandidates(words []string, prev string) []string {
	// `octo goal <TAB>` → subcommand list.
	if len(words) == 3 {
		return taskSubcommands
	}
	sub := words[2]
	switch sub {
	case "status", "show", "resume", "cancel", "run":
		// First positional after the verb is the task ID. For "show" the
		// second positional is the subtask-id — left freeform for v1.
		if len(words) == 4 {
			return taskIDCandidates()
		}
		// Flag values for `task run` / `task resume` (they accept
		// --provider / --model).
		if sub == "run" || sub == "resume" {
			return taskRunFlagCandidates(prev)
		}
	case "start":
		// `octo goal start "<goal>" [flags]` — the goal is freeform.
		return taskRunFlagCandidates(prev)
	}
	return nil
}

func taskRunFlagCandidates(prev string) []string {
	switch prev {
	case "--provider":
		return []string{"anthropic", "openai"}
	case "--model":
		return nil
	}
	return []string{"--provider", "--model", "--plan-only"}
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
		return []string{"interactive", "strict"}
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

// taskIDCandidates is the symmetric helper for tasks.
func taskIDCandidates() []string {
	out := []string{"last"}
	store, err := taskgraph.NewStore()
	if err != nil {
		return out
	}
	tasks, err := store.List()
	if err != nil {
		return out
	}
	for _, t := range tasks {
		out = append(out, t.ShortID(), t.ID)
	}
	return out
}

// completionHelp prints the user-facing instructions for `octo help
// completion`. Kept here rather than in help.go so the strings sit next to
// the scripts they reference.
func completionHelp(w io.Writer) {
	fmt.Fprintln(w, strings.TrimSpace(`
octo completion — print the shell-completion snippet for bash, zsh, or fish.

Examples:
  source <(octo completion bash)            # one-shot for this shell session
  octo completion bash > ~/.octo/octo.bash  # permanent: source from .bashrc
  octo completion zsh  > ~/.octo/_octo      # zsh: drop into fpath and run compinit
  octo completion fish > ~/.config/fish/completions/octo.fish

What it completes:
  - Top-level subcommands (chat, task, memory, init, …).
  - Subcommands of task / memory / help / completion.
  - Session IDs after "octo chat -c " — full + short + "last".
  - Task IDs after "octo goal status|show|resume|cancel|run " — full + short + "last".
  - Fixed values for --provider (anthropic|openai) and --permission-mode
    (interactive|strict).

The shell snippet just delegates to the hidden "octo __complete" subcommand;
the routing logic lives in the binary, so the same snippet keeps working as
new flags / subcommands are added.`))
	fmt.Fprintln(w)
}

// ── Static lists ─────────────────────────────────────────────────────────

var topLevelCommands = []string{
	"chat", "config", "init", "memory", "goal",
	"version", "help", "completion",
}

var taskSubcommands = []string{
	"start", "run", "list", "ls", "status", "show", "resume", "cancel", "help",
}

// chatFlags + initFlags are intentionally not the full flag list — we ship
// the most useful ones for TAB completion. Long-tail flags (e.g.
// --thinking-budget) still work; users just type them in full. Keeping the
// list focused avoids drowning the completion popup with rarely-used flags.
var chatFlags = []string{
	"-c", "--continue", "--tools", "--no-tools", "--provider", "--model",
	"--no-save", "--no-memory", "--sandbox", "--sandbox-allow-net",
	"--permission-mode", "--list-sessions", "--list-skills",
	"--quiet", "--verbose", "--plain", "--stream", "--system",
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
