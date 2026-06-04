package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/hooks"
	"github.com/Leihb/octo-agent/internal/mcp"
	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tasks"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/version"
)

// Provider names accepted by `--provider`.
const (
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
)

// unattendedMaxTurns is the agentic-loop cap applied when --max-turns is left
// at its auto-sentinel (0) and there's no interactive human to continue past
// the limit (piped stdin, --prompt-file). -1 means unlimited because a
// headless task can't be told to keep going.
const unattendedMaxTurns = -1

// resolveMaxTurns picks the agentic-loop cap. An explicit --max-turns (any
// non-zero flagVal) always wins. Otherwise an unattended run — seeded via
// --prompt-file, or reading piped/non-tty stdin, where nobody can type
// "continue" past the limit — gets unattendedMaxTurns; an interactive run
// returns 0 so the agent applies its own (lower) checkpoint default.
func resolveMaxTurns(flagVal int, seeded, interactive bool) int {
	if flagVal != 0 {
		return flagVal
	}
	if seeded || !interactive {
		return unattendedMaxTurns
	}
	return 0
}

// Provider-aware defaults for the escalated output cap retried on truncation.
// Conservative per protocol since octo has no per-model capability table; a
// model whose ceiling is below the target backs off at the API (see the agent
// loop's isMaxTokensTooLargeErr). See dev-docs/truncation-recovery.md.
const (
	escalateMaxTokensOpenAI    = 16384
	escalateMaxTokensAnthropic = 32768
)

// resolveMaxTokensEscalate picks the escalated cap: an explicit flag (>= 0)
// wins, then OCTO_MAX_TOKENS_ESCALATE, then the provider-aware default. 0
// disables escalation.
func resolveMaxTokensEscalate(flagVal int, provName string) int {
	if flagVal >= 0 {
		return flagVal
	}
	if env := strings.TrimSpace(os.Getenv("OCTO_MAX_TOKENS_ESCALATE")); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n >= 0 {
			return n
		}
	}
	if provName == providerOpenAI {
		return escalateMaxTokensOpenAI
	}
	return escalateMaxTokensAnthropic
}

// openMCPLogFile opens ~/.octo/logs/mcp.log (append) to receive stdio MCP
// servers' child stderr while the TUI owns the screen, so their diagnostics are
// recoverable rather than corrupting the frame. Returns nil on any failure; the
// caller then discards child stderr — never the terminal.
func openMCPLogFile() *os.File {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	dir := filepath.Join(home, ".octo", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "mcp.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// suggestEnabled resolves whether to offer after-turn follow-up suggestions:
// on by default, off when --no-suggest is set or OCTO_SUGGEST is falsey.
func suggestEnabled(noSuggestFlag bool) bool {
	if noSuggestFlag {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OCTO_SUGGEST"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// tuiDisabledByEnv reports whether OCTO_TUI is set to a falsey value, the env
// equivalent of --no-tui (handy for dumb terminals / CI without editing the
// command line).
func tuiDisabledByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OCTO_TUI"))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// resolveCoauthor determines whether the agent should append a Co-authored-by
// line to git commit messages. Precedence: --no-coauthor flag > OCTO_COAUTHOR
// env > config file > default (true).
func resolveCoauthor(noCoauthorFlag bool, cfg config.Config) bool {
	if noCoauthorFlag {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OCTO_COAUTHOR"))) {
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return true
	}
	if cfg.Coauthor != nil {
		return *cfg.Coauthor
	}
	return true
}

// resolveShowReasoning determines whether the reasoning/thinking trace is
// streamed to the terminal. Precedence: an explicit --show-reasoning flag >
// config file > default (true).
func resolveShowReasoning(flagSet, flagVal bool, cfg config.Config) bool {
	if flagSet {
		return flagVal
	}
	if cfg.ShowReasoning != nil {
		return *cfg.ShowReasoning
	}
	return true
}

// toolSearchConfigFrom maps the persisted tools.tool_search block onto the
// tools-package config. Unknown/empty "enabled" defaults to auto; numeric zero
// values are left for SetToolSearchConfig to backfill with documented defaults.
func toolSearchConfigFrom(c config.ToolSearchConfig) tools.ToolSearchConfig {
	mode := tools.ToolSearchAuto
	switch c.Enabled {
	case "on":
		mode = tools.ToolSearchOn
	case "off":
		mode = tools.ToolSearchOff
	}
	return tools.ToolSearchConfig{
		Mode:           mode,
		ThresholdPct:   c.ThresholdPct,
		SearchLimit:    c.SearchDefaultLimit,
		MaxSearchLimit: c.MaxSearchLimit,
	}
}

// resolveReasoningEffort picks the reasoning intensity: --reasoning-effort flag
// > config file > "" (off).
func resolveReasoningEffort(flagVal string, cfg config.Config) string {
	if flagVal != "" {
		return flagVal
	}
	return cfg.ReasoningEffort
}

// validReasoningEffort reports whether e is an accepted reasoning intensity.
func validReasoningEffort(e string) bool {
	switch e {
	case "", "low", "medium", "high":
		return true
	}
	return false
}

// anthropicThinkingBudget maps a unified reasoning-effort level to an Anthropic
// extended-thinking token budget. "" (off) yields 0, which disables thinking.
func anthropicThinkingBudget(effort string) int {
	switch effort {
	case "low":
		return 4096
	case "medium":
		return 16384
	case "high":
		return 32768
	}
	return 0
}

// defaultModels maps each provider to the model used when `--model` isn't
// supplied. Both defaults are the cheapest reasoning-capable model in the
// respective vendor's catalogue at the time of writing — the right pick for
// a scaffold whose primary purpose is verifying the wire end-to-end.
var defaultModels = map[string]string{
	providerAnthropic: "claude-haiku-4-5-20251001",
	providerOpenAI:    "gpt-4o-mini",
}

// runChat handles `octo chat [flags] [message]`.
//
// With a positional message argument: single-turn mode (M2 behaviour).
// Without a message argument: enters the interactive REPL (M3).
//
// New M3 flags:
//
//	-c / --continue <id>   Resume a saved session by ID (REPL mode only)
//	--no-save              Disable auto-save in REPL mode
//	--list-sessions        Print the 10 most recent sessions and exit
func runChat(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "Provider: anthropic | openai (default from `octo config`, else anthropic)")
	model := fs.String("model", "", "Model name (else ANTHROPIC_MODEL/OPENAI_MODEL env, then `octo config`, then the provider's cheapest reasoning model)")
	system := fs.String("system", "", "System prompt (optional)")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens for the response (0 = provider default)")
	maxTokensEscalate := fs.Int("max-tokens-escalate", -1, "Per-response cap retried once when a reply is truncated by the output cap (-1 = provider-aware default, 0 = disable). Also OCTO_MAX_TOKENS_ESCALATE.")
	stream := fs.Bool("stream", true, "Stream the reply (chunks printed as they arrive); --stream=false buffers")
	continueID := fs.String("c", "", "Resume a session — accepts 'last', a short ID, or a substring of an ID")
	continueIDLong := fs.String("continue", "", "Resume a session — accepts 'last', a short ID, or a substring of an ID")
	noSave := fs.Bool("no-save", false, "Disable session auto-save in the interactive TUI (headless one-shots never persist)")
	listSessions := fs.Bool("list-sessions", false, "Print the 10 most recent sessions and exit")
	listSkills := fs.Bool("list-skills", false, "Print available skills (user + project) and exit")
	enableTools := fs.Bool("tools", true, "Built-in tools (terminal, edit_file, …) for the agentic loop. On by default; use --no-tools to disable.")
	noTools := fs.Bool("no-tools", false, "Disable the built-in tools (and MCP/skill execution) — plain chat only")
	noMemory := fs.Bool("no-memory", false, "Disable cross-session memory (the remember tool + memory injection)")
	plain := fs.Bool("plain", false, "Render tool events as one-line ↳ status lines instead of rich diff cards")
	promptFile := fs.String("prompt-file", "", "Read the prompt from this file (newlines preserved) and run it as one headless agentic turn, then exit. For scripting/eval.")
	noTUI := fs.Bool("no-tui", false, "Force the headless one-shot path on a terminal instead of the interactive TUI (also OCTO_TUI=0). The prompt comes from a positional message, --prompt-file, or piped stdin.")
	noSuggest := fs.Bool("no-suggest", false, "Disable the after-turn follow-up suggestion (ghost text accepted with Tab/→). Also OCTO_SUGGEST=0.")
	quietFlag := fs.Bool("quiet", false, "Strip all status chrome (no spinner, no banner, no cache line). Also OCTO_VERBOSITY=quiet.")
	verboseFlag := fs.Bool("verbose", false, "Print extra context (provider/model/endpoint, always-on cache line). Also OCTO_VERBOSITY=verbose.")
	permMode := fs.String("permission-mode", "", "Tool permission handling: interactive (prompt on ask) | strict (deny on ask) | auto (allow on ask). Empty = use `octo config` value, else interactive.")
	noCoauthor := fs.Bool("no-coauthor", false, "Disable appending Co-authored-by to git commit messages. Also OCTO_COAUTHOR=0.")
	maxTurns := fs.Int("max-turns", 0, "Max provider round-trips per message in the agentic loop (0 = auto: 100 interactive, unlimited unattended/--prompt-file)")
	compactThreshold := fs.Int("compact-threshold", 0, "Compact older history once a turn's input crosses this many tokens; 0 = auto (~75% of the model's context window), <0 = disabled")
	reasoningEffort := fs.String("reasoning-effort", "", "Reasoning intensity: low | medium | high (empty = off). OpenAI → reasoning_effort; Anthropic → mapped thinking budget. Also from `octo config`.")
	showReasoning := fs.Bool("show-reasoning", true, "Stream the reasoning/thinking trace to the terminal (dimmed). Use --show-reasoning=false to hide it. Also from `octo config`.")
	useSandbox := fs.Bool("sandbox", false, "Confine terminal commands to the project dir + tmp with no network (OS-enforced; macOS/Linux). Fails closed if unavailable.")
	sandboxAllowNet := fs.Bool("sandbox-allow-net", false, "Under --sandbox, permit network access (default: denied)")
	var sandboxWrite, sandboxRead stringList
	fs.Var(&sandboxWrite, "sandbox-write", "Under --sandbox, an extra writable directory (repeatable)")
	fs.Var(&sandboxRead, "sandbox-read", "Under --sandbox, an extra read-only directory (repeatable)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --list-sessions: print and exit, no provider needed.
	if *listSessions {
		sessions, err := agent.ListSessions(10)
		if err != nil {
			fmt.Fprintf(stderr, "octo chat: %v\n", err)
			return 1
		}
		if len(sessions) == 0 {
			fmt.Fprintln(stdout, "No saved sessions.")
			return 0
		}
		fmt.Fprintln(stdout, "Recent sessions (newest first):")
		fmt.Fprintln(stdout, formatSessionList(sessions))
		return 0
	}

	// --list-skills: discover and print, no provider needed.
	if *listSkills {
		cwd, _ := os.Getwd()
		reg := skills.Discover(cwd)
		if reg.Len() == 0 {
			fmt.Fprintln(stdout, "No skills found (looked in ~/.octo/skills and ./.octo/skills).")
			return 0
		}
		fmt.Fprintln(stdout, "Available skills (trigger with /<name>):")
		for _, s := range reg.List() {
			fmt.Fprintf(stdout, "  /%-16s [%-7s] %s\n", s.Name, s.Source, s.Description)
		}
		return 0
	}

	// Resolve -c / --continue (short wins if both somehow set).
	resumeID := *continueIDLong
	if *continueID != "" {
		resumeID = *continueID
	}

	userInput := strings.TrimSpace(strings.Join(fs.Args(), " "))
	isREPL := userInput == ""

	// --prompt-file supplies the one-shot prompt from a file. It's mutually
	// exclusive with a positional message — both are prompt sources, and
	// accepting both would silently drop one.
	var seedPrompt string
	if *promptFile != "" {
		if !isREPL {
			fmt.Fprintln(stderr, "octo chat: --prompt-file cannot be combined with a positional message")
			return 2
		}
		b, err := os.ReadFile(*promptFile)
		if err != nil {
			fmt.Fprintf(stderr, "octo chat: --prompt-file: %v\n", err)
			return 1
		}
		seedPrompt = strings.TrimRight(string(b), "\n")
		if strings.TrimSpace(seedPrompt) == "" {
			fmt.Fprintln(stderr, "octo chat: --prompt-file is empty")
			return 2
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo chat: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo config` to rewrite ~/.octo/config.yaml.")
		return 1
	}

	// Resolve permission mode: explicit flag > config file > interactive default.
	resolvedPermMode := *permMode
	if resolvedPermMode == "" {
		resolvedPermMode = cfg.PermissionMode
	}
	if resolvedPermMode == "" {
		resolvedPermMode = string(permission.ModeInteractive)
	}
	// Validate up front. Fail closed on a typo rather than silently falling
	// back to the more-permissive interactive mode.
	if resolvedPermMode != string(permission.ModeInteractive) && resolvedPermMode != string(permission.ModeStrict) && resolvedPermMode != string(permission.ModeAutoApprove) {
		fmt.Fprintf(stderr, "octo chat: invalid --permission-mode %q (want 'interactive', 'strict', or 'auto')\n", resolvedPermMode)
		return 2
	}

	provName, resolvedModel, ok := resolveProviderModel(*providerName, *model, cfg)
	if !ok {
		fmt.Fprintf(stderr, "octo chat: unknown provider %q (use 'anthropic' or 'openai')\n", provName)
		return 2
	}

	// Install the Tool Search config so DefaultToolsFor can decide whether to
	// defer MCP schemas behind the search/describe/call bridge for this model.
	tools.SetToolSearchConfig(toolSearchConfigFrom(cfg.Tools.ToolSearch))

	// Resolve reasoning controls: --reasoning-effort sets the intensity (OpenAI
	// reasoning_effort / mapped Anthropic budget); --show-reasoning gates whether
	// the trace is streamed to the terminal. Both fall back to `octo config`.
	resolvedEffort := resolveReasoningEffort(*reasoningEffort, cfg)
	if !validReasoningEffort(resolvedEffort) {
		fmt.Fprintf(stderr, "octo chat: invalid --reasoning-effort %q (want 'low', 'medium', or 'high')\n", resolvedEffort)
		return 2
	}
	showReasoningFlagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "show-reasoning" {
			showReasoningFlagSet = true
		}
	})
	resolvedShowReasoning := resolveShowReasoning(showReasoningFlagSet, *showReasoning, cfg)

	// Single-turn mode requires a message.
	if !isREPL && resumeID != "" {
		fmt.Fprintln(stderr, "octo chat: -c/--continue requires interactive mode (omit the message argument)")
		return 2
	}

	// Resolve -c shortcuts ("last", short ID, prefix/substring) against the
	// on-disk session store. The legacy full-ID path still works because
	// ResolveSessionID short-circuits on an exact filename match before
	// falling back to substring matching.
	if resumeID != "" {
		resolved, err := agent.ResolveSessionID(resumeID)
		if err != nil {
			fmt.Fprintf(stderr, "octo chat: %v\n", err)
			fmt.Fprintln(stderr, "Run `octo chat --list-sessions` to see what's available.")
			return 2
		}
		resumeID = resolved
	}

	// Built-in tools are on by default; --no-tools opts out. MCP servers and
	// skill execution ride on the same switch, so a single flag governs the
	// whole agentic surface.
	toolsOn := *enableTools && !*noTools

	// Resuming a tool-using session with tools off is a footgun: the model
	// sees prior tool_use blocks in history but gets no tools array, and
	// falls back to emitting tool calls as text (a wall of
	// `<tool_calls><invoke name="...">...` XML). With tools on by default
	// this can only happen when the user explicitly passes --no-tools, so we
	// respect their choice but warn once.
	if isREPL && resumeID != "" && !toolsOn {
		if peek, perr := agent.LoadSession(resumeID); perr == nil && peek.UsedTools() {
			fmt.Fprintln(stderr, "Warning: this session used tools before; --no-tools may make the model emit tool calls as text.")
		}
	}

	llmSender, err := buildSender(provName, cfg, stderr, senderTuning{
		thinkingBudget:  anthropicThinkingBudget(resolvedEffort),
		reasoningEffort: resolvedEffort,
		showReasoning:   resolvedShowReasoning,
	})
	if err != nil {
		return 1
	}

	// Verbose: surface the resolved provider / model / endpoint so a
	// misrouted base URL (e.g. ANTHROPIC_BASE_URL pointed at a third party) is
	// visible at a glance. To stderr so it never pollutes single-turn stdout;
	// shown for every path (single-turn, plain REPL, TUI).
	if resolveVerbosity(*quietFlag, *verboseFlag).verbose() {
		fmt.Fprintf(stderr, "octo: provider=%s model=%s endpoint=%s\n",
			provName, resolvedModel, effectiveEndpoint(provName, cfg))
	}

	// Resolve coauthor: flag > env > config > default (true).
	coauthor := resolveCoauthor(*noCoauthor, cfg)

	// Compose the system prompt once (base + project .octorules + user --system)
	// and freeze it for the session — recomputing mid-session would bust the
	// provider's system+tools prompt cache. The session stores only the raw
	// user layer; base/project are recomposed fresh each run.
	cwd, _ := os.Getwd()
	env := buildEnvContext(cwd)

	// Discover skills (user + project) once at session start. The manifest goes
	// into the frozen system prompt (L1); the registry backs the `skill` tool so
	// it can serve full bodies on demand (L2).
	skillReg := skills.Discover(cwd)
	skillsManifest := skills.RenderManifest(skillReg)
	tools.SetSkills(skillReg)

	// Cross-session memory (Claude Code model): a per-repo directory of markdown
	// files the agent manages with its own file tools. memDir is created up
	// front, injected into the system prompt (below), and whitelisted for writes
	// when the permission engine is built. --no-memory disables it; a resolve
	// error degrades to no memory rather than failing.
	var memDir string
	if !*noMemory {
		if d, err := memory.Dir(memory.ProjectRoot(cwd)); err == nil {
			if memory.EnsureDir(d) == nil {
				memDir = d
			}
		}
	}

	if *useSandbox {
		opts := sandboxOpts{allowNet: *sandboxAllowNet, writeRoots: sandboxWrite, readRoots: sandboxRead}
		if err := activateSandbox(cwd, opts, stderr); err != nil {
			return 1
		}
	}

	// The sender (built above with a stable per-process cache key) lets OpenAI
	// route every turn — and every tool-loop iteration — of this conversation
	// to the same prompt cache.
	a := agent.New(llmSender, resolvedModel)
	a.CWD = cwd
	a.MaxTokens = *maxTokens
	a.MaxTokensEscalate = resolveMaxTokensEscalate(*maxTokensEscalate, provName)
	a.MaxTurns = resolveMaxTurns(*maxTurns, seedPrompt != "", stdinIsTTY(stdin))
	a.CompactThreshold = *compactThreshold

	// Build the tool executor up-front (REPL mode only — single-turn mode
	// doesn't dispatch tools) and register the sub-agent dispatcher BEFORE the
	// startup memory pass runs. This lets maybeProcessMemory's consolidation
	// step use a sub-agent (M10, #6) with read-only filesystem tools, falling
	// back to the side-call path when no Spawner is wired.
	//
	// The line reader is built here too so the same instance is shared with
	// the REPL loop, the permission gate, and the asker (no double-buffering
	// of stdin). On an interactive terminal we use chzyer/readline for
	// history + line editing; otherwise (tests, pipes) we fall back to a
	// scanner over stdin. Registering the asker NOW also matters for
	// DefaultTools() gating: the ask_user_question tool only appears when
	// an asker is registered, and DefaultTools is computed below before
	// runREPL starts.
	var (
		toolExecutor tools.DefaultRegistry
		replReader   lineReader
		replView     ViewSink
		subAgentMgr  *tools.SubAgentManager
	)
	// Two surfaces only. An interactive terminal drives the bubbletea TUI;
	// everything else is a headless one-shot — octo's claude -p mode. A
	// positional message, --prompt-file, piped/redirected stdin, --no-tui, or
	// OCTO_TUI=0 all take the one-shot path (tests, octo-eval, CI included).
	useTUI := isREPL && stdinIsTTY(stdin) && !*noTUI && !tuiDisabledByEnv() && seedPrompt == ""

	// Resuming a session (-c) is an interactive affordance — it only makes sense
	// in the TUI. A headless one-shot starts fresh.
	if resumeID != "" && !useTUI {
		fmt.Fprintln(stderr, "octo chat: -c/--continue needs an interactive terminal (run it without piping/--no-tui)")
		return 2
	}

	// Resolve the single prompt for the one-shot path: positional message →
	// --prompt-file → all of piped stdin. The TUI reads its own input.
	var oneShotPrompt string
	if !useTUI {
		oneShotPrompt = userInput
		if oneShotPrompt == "" {
			oneShotPrompt = seedPrompt
		}
		if oneShotPrompt == "" && !stdinIsTTY(stdin) {
			b, rerr := io.ReadAll(stdin)
			if rerr != nil {
				fmt.Fprintf(stderr, "octo chat: read stdin: %v\n", rerr)
				return 1
			}
			oneShotPrompt = strings.TrimSpace(string(b))
		}
		if oneShotPrompt == "" {
			fmt.Fprintln(stderr, "octo chat: no prompt — pass a message, use --prompt-file, or pipe input on stdin")
			return 2
		}
	}

	// Agentic setup — shared by both paths, which each run the full tool loop.
	toolExecutor = tools.NewDefaultRegistry()
	// Sub-agents share the parent's model for the Tool Search threshold decision
	// (a model override is rare and still resolves to a sane window).
	spawner := app.NewSpawner(a, toolExecutor, func() []agent.ToolDefinition {
		return tools.DefaultToolsFor(a.Model)
	})
	tools.SetSpawner(spawner)
	subAgentMgr = tools.NewSubAgentManager(spawner)
	if useTUI {
		// bubbletea owns stdin and renders its own input; the asker and gate
		// are wired to the TUI sink inside runTUI.
		defer tools.SetAsker(nil)
	} else {
		// A TTY reader (a positional message typed at a terminal) makes
		// permission / Ask prompts interactive. Over a pipe stdin is already
		// drained into the prompt, so the reader hits EOF and plainView
		// auto-denies — the headless posture.
		if stdinIsTTY(stdin) {
			rl, err := newReadlineReader(defaultHistoryFile())
			if err != nil {
				fmt.Fprintf(stderr, "octo chat: line editor unavailable (%v); falling back to plain input\n", err)
				replReader = newScannerLineReader(stdin, stdout)
			} else {
				replReader = rl
			}
		} else {
			replReader = newScannerLineReader(stdin, stdout)
		}
		replView = newPlainView(replReader, stdout, stderr, resolveVerbosity(*quietFlag, *verboseFlag), *plain)
		tools.SetAsker(newREPLAsker(replView))
		defer tools.SetAsker(nil)
	}

	// Session-scoped task tracker backing the task_create / task_update /
	// task_list tools. Lost on exit by design.
	tools.SetTaskStore(tasks.New())
	defer tools.SetTaskStore(nil)

	// MCP servers: load config, connect, register so DefaultTools and
	// DefaultRegistry pick them up. Best-effort — a misconfigured or missing
	// server is logged on stderr and the session keeps going. Skipped when
	// tools are off (the agent would never invoke them).
	//
	// Connecting is synchronous for the headless one-shot — its single turn
	// needs the full tool surface before it runs. The interactive TUI instead
	// defers connection to a background tea.Cmd (mcpBoot, handled in runTUI) so
	// the banner + input paint immediately; tools register when the registry
	// arrives a moment later.
	var mcpBoot *mcpBootstrap
	if toolsOn {
		mcpCfg, err := mcp.LoadConfig(cwd)
		if err != nil {
			fmt.Fprintf(stderr, "octo chat: mcp config: %v\n", err)
		} else if len(mcpCfg.Servers) > 0 {
			info := mcp.Implementation{Name: "octo", Version: version.Version}
			if useTUI {
				// A stdio server's subprocess writes diagnostics to its stderr at
				// arbitrary times during the session, from an exec copy goroutine.
				// Under the bubbletea TUI a direct terminal write corrupts the
				// frame, so route it to a log file (recoverable, never the
				// screen). The file is closed when runChat returns, i.e. after
				// runTUI exits.
				var childStderr io.Writer
				if f := openMCPLogFile(); f != nil {
					childStderr = f
					defer f.Close()
				} else {
					childStderr = io.Discard
				}
				mcpBoot = &mcpBootstrap{cfg: mcpCfg, info: info, childErr: childStderr}
			} else {
				// Headless: connect now and register before the one-shot turn.
				// Leave childStderr nil — the transport defaults to os.Stderr, the
				// only writer safe for the copy goroutine to share concurrently.
				mcpReg := mcp.ConnectAll(
					context.Background(),
					mcpCfg,
					info,
					func(serverName string) mcp.OAuthPrompt {
						return newCLIOAuthPrompt(stdout, serverName)
					},
					stderr,
					nil,
				)
				if mcpReg.Len() > 0 {
					tools.SetMCPRegistry(mcpReg)
					defer func() {
						tools.SetMCPRegistry(nil)
						mcpReg.Close()
					}()
				}
			}
		}
	}

	// Inject the project's MEMORY.md (plus the manage-it-yourself instruction)
	// into the system prompt. The agent reads/writes the rest of the memory
	// directory on demand with its file tools — no consolidation pass.
	var memInjection string
	if memDir != "" {
		memInjection = memory.RenderInjection(memDir)
	}
	a.System = prompt.Compose(*system, cwd, env, skillsManifest, memInjection, coauthor)

	// Attention layer: re-surface MEMORY.md's structured rules (## 必须遵守 /
	// ## 触发提醒) on the message stream at the point of action. This rides each
	// user turn rather than the cached system prompt, so the prompt prefix stays
	// byte-stable. A MEMORY.md without those sections yields no hook — unchanged
	// behaviour.
	if memDir != "" {
		if rules := memory.ParseRules(memDir); rules.HasAny() {
			inj := memory.NewInjector(rules)
			a.UserInputHook = inj.Reminder
		}
	}

	// Permission engine — gates every tool call; shared by both paths. The
	// memory directory (outside CWD) is whitelisted for writes so the agent can
	// manage its memory files without a prompt on every save.
	var permEngine *permission.Engine
	if toolsOn {
		eng, perr := permission.New(permissionConfigPath(), cwd, resolvePermissionMode(resolvedPermMode), memDir)
		if perr != nil {
			fmt.Fprintf(stderr, "octo chat: permission config: %v\n", perr)
			return 1
		}
		permEngine = eng
	}

	// ── Interactive TUI ───────────────────────────────────────────────────────
	if useTUI {
		var sess *agent.Session
		if resumeID != "" {
			sess, err = agent.LoadSession(resumeID)
			if err != nil {
				fmt.Fprintf(stderr, "octo chat: %v\n", err)
				return 1
			}
			// Restore history and override model/system from saved session.
			a.History = sess.ToHistory()
			if sess.Model != "" {
				a.Model = sess.Model
			}
			// Recompose from the session's raw user layer so base/project/env
			// pick up any changes since the session was created.
			a.System = prompt.Compose(sess.System, cwd, env, skillsManifest, memInjection, coauthor)
		} else {
			sess = agent.NewSession(resolvedModel, *system)
		}

		cfg := replConfig{
			a:          a,
			session:    sess,
			noSave:     *noSave,
			suggest:    suggestEnabled(*noSuggest),
			plain:      *plain,
			verbosity:  resolveVerbosity(*quietFlag, *verboseFlag),
			stdin:      stdin,
			stdout:     stdout,
			stderr:     stderr,
			skillReg:   skillReg,
			memDir:     memDir,
			reader:     replReader,          // shared with the asker / permission gate
			view:       replView,            // same surface for turn render + Ask prompts
			hooks:      hooks.LoadFromEnv(), // C9 Phase 3: external retrieval layer hooks
			permEngine: permEngine,
			mcpBoot:    mcpBoot, // nil unless tools on with servers configured
		}
		if toolsOn {
			// Built-ins only at first paint — the MCP registry is still nil
			// (mcpBoot connects it in the background). mcpReadyMsg recomputes
			// this list once the servers are live.
			cfg.tools = tools.DefaultToolsFor(resolvedModel)
			cfg.executor = toolExecutor
			cfg.subAgentMgr = subAgentMgr
		}
		return runTUI(cfg)
	}

	// ── Headless one-shot (claude -p) ─────────────────────────────────────────
	// One agentic turn, then exit. The session is ephemeral — one-shot runs are
	// not persisted (resuming with -c stays a TUI affordance).
	replCfg := replConfig{
		a:          a,
		session:    agent.NewSession(resolvedModel, *system),
		noSave:     true,
		plain:      *plain,
		verbosity:  resolveVerbosity(*quietFlag, *verboseFlag),
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		skillReg:   skillReg,
		memDir:     memDir,
		reader:     replReader,
		view:       replView,
		hooks:      hooks.LoadFromEnv(),
		permEngine: permEngine,
	}
	if toolsOn {
		replCfg.tools = tools.DefaultToolsFor(resolvedModel)
		replCfg.executor = toolExecutor
		replCfg.subAgentMgr = subAgentMgr
	}
	return runOnce(replCfg, oneShotPrompt, *stream)
}

// printUsageLine writes a one-line token/cache summary to w when the backend
// reported any usage. Goes to stderr so it doesn't pollute piped stdout.
func printUsageLine(w io.Writer, reply agent.Reply) {
	if reply.InputTokens == 0 && reply.OutputTokens == 0 && reply.CacheReadTokens == 0 {
		return
	}
	fmt.Fprintf(w, "[usage] in %d / out %d / cache %d read, %d write\n",
		reply.InputTokens, reply.OutputTokens, reply.CacheReadTokens, reply.CacheWriteTokens)
}

// senderTuning carries the optional reasoning/thinking knobs a caller wants on
// the sender. The zero value (no extended reasoning) is what the IM bridge and
// server use.
type senderTuning struct {
	thinkingBudget  int
	reasoningEffort string
	showReasoning   bool
}

// buildSender resolves credentials/endpoint (env-first, then the persisted
// config) and returns an agent.Sender built through internal/app — the single
// place that constructs provider clients. On a configuration error it writes a
// user-facing message to stderr and returns a non-nil error. A fresh
// prompt-cache key is generated per call.
func buildSender(name string, cfg config.Config, stderr io.Writer, tuning senderTuning) (agent.Sender, error) {
	apiKey, err := resolveAPIKey(name, cfg, stderr)
	if err != nil {
		return nil, err
	}
	s, err := app.NewSender(app.SenderOptions{
		Provider:        name,
		APIKey:          apiKey,
		BaseURL:         resolveBaseURL(name, cfg),
		CacheKey:        newCacheKey(),
		ThinkingBudget:  tuning.thinkingBudget,
		ReasoningEffort: tuning.reasoningEffort,
		ShowReasoning:   tuning.showReasoning,
	})
	if err != nil {
		fmt.Fprintf(stderr, "octo chat: %v\n", err)
		return nil, err
	}
	return s, nil
}

// resolveAPIKey returns the API key for the requested vendor: the provider's
// env var, else cfg.APIKey when the stored config is for this same provider. On
// a missing key it prints provider-specific setup help to stderr and returns a
// non-nil error.
func resolveAPIKey(name string, cfg config.Config, stderr io.Writer) (string, error) {
	switch name {
	case providerAnthropic:
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" && cfg.Provider == providerAnthropic {
			apiKey = cfg.APIKey
		}
		if apiKey == "" {
			fmt.Fprintln(stderr, "octo: ANTHROPIC_API_KEY is not set.")
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, "To use Anthropic (default):")
			fmt.Fprintln(stderr, "  1. Get a key at https://console.anthropic.com/")
			fmt.Fprintln(stderr, "  2. export ANTHROPIC_API_KEY=sk-ant-...")
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, "Or run `octo config` to save a default provider/key.")
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, "Or use OpenAI:")
			fmt.Fprintln(stderr, "  export OPENAI_API_KEY=sk-...")
			fmt.Fprintln(stderr, "  octo chat --provider openai")
			return "", errors.New("missing ANTHROPIC_API_KEY")
		}
		return apiKey, nil

	case providerOpenAI:
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" && cfg.Provider == providerOpenAI {
			apiKey = cfg.APIKey
		}
		if apiKey == "" {
			fmt.Fprintln(stderr, "octo: OPENAI_API_KEY is not set.")
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, "To use OpenAI:")
			fmt.Fprintln(stderr, "  1. Get a key at https://platform.openai.com/api-keys")
			fmt.Fprintln(stderr, "  2. export OPENAI_API_KEY=sk-...")
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, "Or run `octo config` to save a default provider/key.")
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, "Or use Anthropic (the default):")
			fmt.Fprintln(stderr, "  export ANTHROPIC_API_KEY=sk-ant-...")
			fmt.Fprintln(stderr, "  octo chat                 # no --provider flag needed")
			return "", errors.New("missing OPENAI_API_KEY")
		}
		return apiKey, nil

	default:
		fmt.Fprintf(stderr, "octo chat: unknown provider %q (use 'anthropic' or 'openai')\n", name)
		return "", fmt.Errorf("unknown provider %q", name)
	}
}

// newCacheKey returns a random hex token used as the prompt-cache key for one
// conversation/process. Falls back to a timestamp if the system RNG is
// unavailable (still stable for the process).
func newCacheKey() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("octo-%d", time.Now().UnixNano())
	}
	return "octo-" + hex.EncodeToString(b[:])
}
