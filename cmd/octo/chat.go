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
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/provider"
	"github.com/Leihb/octo-agent/internal/provider/anthropic"
	"github.com/Leihb/octo-agent/internal/provider/openai"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tools"
)

// Provider names accepted by `--provider`.
const (
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
)

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
	providerName := fs.String("provider", providerAnthropic, "Provider: anthropic | openai")
	model := fs.String("model", "", "Model name (defaults to the provider's cheapest reasoning model)")
	system := fs.String("system", "", "System prompt (optional)")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens for the response (0 = provider default)")
	stream := fs.Bool("stream", true, "Stream the reply (chunks printed as they arrive); --stream=false buffers")
	continueID := fs.String("c", "", "Session ID to resume (short flag)")
	continueIDLong := fs.String("continue", "", "Session ID to resume")
	noSave := fs.Bool("no-save", false, "Disable auto-save in REPL mode")
	listSessions := fs.Bool("list-sessions", false, "Print the 10 most recent sessions and exit")
	listSkills := fs.Bool("list-skills", false, "Print available skills (user + project) and exit")
	enableTools := fs.Bool("tools", false, "Enable built-in tools (bash) for agentic loop")
	noMemory := fs.Bool("no-memory", false, "Disable cross-session memory (the remember tool + memory injection)")
	plain := fs.Bool("plain", false, "Render tool events as one-line ↳ status lines instead of rich diff cards")
	permMode := fs.String("permission-mode", "interactive", "Tool permission handling: interactive (prompt on ask) | strict (deny on ask)")
	maxTurns := fs.Int("max-turns", 0, "Max provider round-trips per message in the agentic loop (0 = default 20)")
	maxCost := fs.Float64("max-cost", 0, "Stop the session once estimated cost (USD) reaches this; 0 = unlimited")
	compactThreshold := fs.Int("compact-threshold", 0, "Compact older history once a turn's input crosses this many tokens; 0 = auto (~75% of the model's context window), <0 = disabled")
	thinkingBudget := fs.Int("thinking-budget", 0, "Enable extended thinking with this token budget (Anthropic/Kimi); 0 = off")
	useSandbox := fs.Bool("sandbox", false, "Confine terminal commands to the project dir + tmp with no network (OS-enforced; macOS/Linux). Fails closed if unavailable.")
	sandboxAllowNet := fs.Bool("sandbox-allow-net", false, "Under --sandbox, permit network access (default: denied)")
	var sandboxWrite, sandboxRead stringList
	fs.Var(&sandboxWrite, "sandbox-write", "Under --sandbox, an extra writable directory (repeatable)")
	fs.Var(&sandboxRead, "sandbox-read", "Under --sandbox, an extra read-only directory (repeatable)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Validate --permission-mode up front. Fail closed on a typo rather than
	// silently falling back to the more-permissive interactive mode — a user
	// who asked for "strict" and got "interactive" by typo is a security
	// regression, not a convenience.
	if *permMode != string(permission.ModeInteractive) && *permMode != string(permission.ModeStrict) {
		fmt.Fprintf(stderr, "octo chat: invalid --permission-mode %q (want 'interactive' or 'strict')\n", *permMode)
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
		for _, s := range sessions {
			turns := s.TurnCount()
			plural := "s"
			if turns == 1 {
				plural = ""
			}
			fmt.Fprintf(stdout, "  %s  %-36s  %d turn%s\n", s.ID, s.Model, turns, plural)
		}
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

	resolvedModel := *model
	if resolvedModel == "" {
		resolvedModel = defaultModels[*providerName]
	}
	if resolvedModel == "" {
		fmt.Fprintf(stderr, "octo chat: unknown provider %q (use 'anthropic' or 'openai')\n", *providerName)
		return 2
	}

	// Single-turn mode requires a message.
	if !isREPL && resumeID != "" {
		fmt.Fprintln(stderr, "octo chat: -c/--continue requires interactive mode (omit the message argument)")
		return 2
	}

	prov, err := buildProvider(*providerName, stderr)
	if err != nil {
		return 1
	}

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

	// Cross-session memory (C9): the store backs the `remember` tool and (below)
	// renders this session's injection. --no-memory disables both. A store error
	// (e.g. unresolvable home dir) degrades to no memory rather than failing.
	var memStore *memory.Store
	if !*noMemory {
		if store, err := memory.NewStore(); err == nil {
			memStore = store
			tools.SetMemoryStore(store)
		}
	}

	if *useSandbox {
		opts := sandboxOpts{allowNet: *sandboxAllowNet, writeRoots: sandboxWrite, readRoots: sandboxRead}
		if err := activateSandbox(cwd, opts, stderr); err != nil {
			return 1
		}
	}

	// A stable per-process cache key lets OpenAI route every turn (and every
	// tool-loop iteration) of this conversation to the same prompt cache.
	a := agent.New(providerSender{
		p:              prov,
		cacheKey:       newCacheKey(),
		thinkingBudget: *thinkingBudget,
		thinkingOut:    stdout,
	}, resolvedModel)
	a.MaxTokens = *maxTokens
	a.MaxTurns = *maxTurns
	a.MaxCostUSD = *maxCost
	a.CompactThreshold = *compactThreshold

	// Boundary memory (REPL only): extract durable facts from the previous
	// session and consolidate if due, BEFORE rendering this session's injection
	// so freshly captured facts apply immediately rather than a session later.
	// Skips the session being resumed. Best-effort — errors don't block startup.
	if isREPL && memStore != nil {
		maybeProcessMemory(a, memStore, stdout, resumeID)
	}
	var memInjection string
	if memStore != nil {
		memInjection, _ = memStore.RenderInjection()
	}
	a.System = prompt.Compose(*system, cwd, env, skillsManifest, memInjection)

	// ── REPL mode ────────────────────────────────────────────────────────────
	if isREPL {
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
			a.System = prompt.Compose(sess.System, cwd, env, skillsManifest, memInjection)
		} else {
			sess = agent.NewSession(resolvedModel, *system)
		}

		cfg := replConfig{
			a:        a,
			session:  sess,
			noSave:   *noSave,
			plain:    *plain,
			stdin:    stdin,
			stdout:   stdout,
			stderr:   stderr,
			skillReg: skillReg,
			memStore: memStore,
		}
		if *enableTools {
			executor := tools.NewDefaultRegistry()
			// Register sub-agent dispatch BEFORE asking DefaultTools() for the
			// catalog, so launch_agent appears in the LLM-facing tool list.
			// The spawner closes over the parent agent so child token usage
			// rolls back into a's session counters.
			tools.SetSpawner(newAgentSpawner(a, executor, tools.DefaultTools))
			cfg.tools = tools.DefaultTools()
			cfg.executor = executor

			// Build the permission engine that gates every tool call.
			cwd, _ := os.Getwd()
			engine, err := permission.New(permissionConfigPath(), cwd, resolvePermissionMode(*permMode))
			if err != nil {
				fmt.Fprintf(stderr, "octo chat: permission config: %v\n", err)
				return 1
			}
			cfg.permEngine = engine
		}
		return runREPL(cfg)
	}

	// ── Single-turn mode (original M2 behaviour) ──────────────────────────────
	// Reap any background processes the turn spawned (REPL handles its own).
	defer tools.KillAllBackground()
	if *stream {
		reply, err := a.TurnStream(context.Background(), userInput, func(d string) {
			fmt.Fprint(stdout, d)
		})
		if err != nil {
			fmt.Fprintf(stderr, "\nocto chat: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout)
		printUsageLine(stderr, reply)
		return 0
	}

	reply, err := a.Turn(context.Background(), userInput)
	if err != nil {
		fmt.Fprintf(stderr, "octo chat: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, reply.Content)
	printUsageLine(stderr, reply)
	return 0
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

// buildProvider constructs a provider.Provider for the requested vendor,
// reading the appropriate env vars (key + optional base URL). On
// configuration errors it writes a user-facing message to stderr and
// returns a non-nil error.
func buildProvider(name string, stderr io.Writer) (provider.Provider, error) {
	switch name {
	case providerAnthropic:
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(stderr, "octo chat: ANTHROPIC_API_KEY environment variable is not set")
			return nil, errors.New("missing ANTHROPIC_API_KEY")
		}
		client, err := anthropic.New(apiKey)
		if err != nil {
			fmt.Fprintf(stderr, "octo chat: %v\n", err)
			return nil, err
		}
		if baseURL := os.Getenv("ANTHROPIC_BASE_URL"); baseURL != "" {
			client.BaseURL = baseURL
		}
		return client, nil

	case providerOpenAI:
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(stderr, "octo chat: OPENAI_API_KEY environment variable is not set")
			return nil, errors.New("missing OPENAI_API_KEY")
		}
		client, err := openai.New(apiKey)
		if err != nil {
			fmt.Fprintf(stderr, "octo chat: %v\n", err)
			return nil, err
		}
		if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
			client.BaseURL = baseURL
		}
		return client, nil

	default:
		fmt.Fprintf(stderr, "octo chat: unknown provider %q (use 'anthropic' or 'openai')\n", name)
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}

// providerSender adapts a provider.Provider into agent.Sender. Keeping the
// adapter in cmd/octo means the agent package never imports provider — a
// one-directional dep graph that pays off as more provider implementations
// land.
type providerSender struct {
	p provider.Provider
	// cacheKey is a stable per-conversation identifier forwarded as the
	// provider's prompt-cache key (OpenAI prompt_cache_key). Keeping it
	// constant across a session's turns lets the backend route them to the
	// same prompt cache. Empty is fine — providers omit the field.
	cacheKey string
	// thinkingBudget, when > 0, enables extended thinking (Anthropic) with this
	// token budget for the reasoning trace.
	thinkingBudget int
	// thinkingOut, when non-nil, is where the streamed thinking trace is printed
	// (dimmed). Nil disables thinking display.
	thinkingOut io.Writer
}

// thinkingRenderer returns an OnThinking callback that streams the reasoning
// trace dimmed, and a close function that ends the dim styling before the
// visible answer begins. Both are no-ops when thinking display is off.
func (s providerSender) thinkingRenderer() (onThinking func(string), closeThinking func()) {
	if s.thinkingOut == nil || s.thinkingBudget <= 0 {
		return nil, func() {}
	}
	var started, closed bool
	onThinking = func(d string) {
		if !started {
			fmt.Fprint(s.thinkingOut, "\x1b[2m\U0001F4AD ")
			started = true
		}
		fmt.Fprint(s.thinkingOut, d)
	}
	closeThinking = func() {
		if started && !closed {
			fmt.Fprint(s.thinkingOut, "\x1b[0m\n")
			closed = true
		}
	}
	return onThinking, closeThinking
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

func (s providerSender) SendMessages(ctx context.Context, model, system string, msgs []agent.Message, maxTokens int) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("providerSender: provider is nil")
	}
	resp, err := s.p.Send(ctx, provider.Request{
		Model:          model,
		SystemPrompt:   system,
		Messages:       msgs,
		MaxTokens:      maxTokens,
		CacheKey:       s.cacheKey,
		ThinkingBudget: s.thinkingBudget,
	})
	if err != nil {
		return agent.Reply{}, err
	}
	return replyFromResponse(resp), nil
}

// StreamMessages implements agent.StreamingSender by delegating to the
// underlying provider's SendStream — when the provider implements
// provider.StreamingProvider. If it doesn't (e.g. a future
// non-streaming-capable backend), we fall back to the buffered Send path
// and synthesise a single onChunk call with the full content so callers
// see the same shape either way.
func (s providerSender) StreamMessages(
	ctx context.Context,
	model, system string,
	msgs []agent.Message,
	maxTokens int,
	onChunk func(string),
) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("providerSender: provider is nil")
	}
	req := provider.Request{
		Model:          model,
		SystemPrompt:   system,
		Messages:       msgs,
		MaxTokens:      maxTokens,
		CacheKey:       s.cacheKey,
		ThinkingBudget: s.thinkingBudget,
	}
	if sp, ok := s.p.(provider.StreamingProvider); ok {
		onThinking, closeThinking := s.thinkingRenderer()
		resp, err := sp.SendStream(ctx, req, provider.StreamCallbacks{
			OnText: func(d string) {
				closeThinking()
				if onChunk != nil {
					onChunk(d)
				}
			},
			OnThinking: onThinking,
		})
		closeThinking()
		if err != nil {
			return agent.Reply{}, err
		}
		return replyFromResponse(resp), nil
	}

	resp, err := s.p.Send(ctx, req)
	if err != nil {
		return agent.Reply{}, err
	}
	if onChunk != nil && resp.Content != "" {
		onChunk(resp.Content)
	}
	return replyFromResponse(resp), nil
}

func replyFromResponse(resp provider.Response) agent.Reply {
	return agent.Reply{
		Content:          resp.Content,
		Blocks:           resp.Blocks,
		Model:            resp.Model,
		StopReason:       resp.StopReason,
		InputTokens:      resp.InputTokens,
		OutputTokens:     resp.OutputTokens,
		CacheReadTokens:  resp.CacheReadTokens,
		CacheWriteTokens: resp.CacheWriteTokens,
	}
}

// SendMessagesWithTools implements agent.ToolSender. It passes the tool
// definitions to the provider via provider.Request.Tools and returns the full
// content-block list (including tool_use blocks) in the Reply.
func (s providerSender) SendMessagesWithTools(
	ctx context.Context,
	model, system string,
	msgs []agent.Message,
	maxTokens int,
	tools []agent.ToolDefinition,
) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("providerSender: provider is nil")
	}
	resp, err := s.p.Send(ctx, provider.Request{
		Model:          model,
		SystemPrompt:   system,
		Messages:       msgs,
		MaxTokens:      maxTokens,
		CacheKey:       s.cacheKey,
		Tools:          tools,
		ThinkingBudget: s.thinkingBudget,
	})
	if err != nil {
		return agent.Reply{}, err
	}
	return replyFromResponse(resp), nil
}

// StreamMessagesWithTools implements agent.ToolStreamingSender. It passes
// tools to the provider and streams text deltas via onChunk; tool_use blocks
// are accumulated and returned in Reply.Blocks at the end of the stream.
func (s providerSender) StreamMessagesWithTools(
	ctx context.Context,
	model, system string,
	msgs []agent.Message,
	maxTokens int,
	tools []agent.ToolDefinition,
	onChunk func(string),
	onToolDelta agent.ToolInputDeltaFunc,
) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("providerSender: provider is nil")
	}
	req := provider.Request{
		Model:          model,
		SystemPrompt:   system,
		Messages:       msgs,
		MaxTokens:      maxTokens,
		CacheKey:       s.cacheKey,
		Tools:          tools,
		ThinkingBudget: s.thinkingBudget,
	}
	if sp, ok := s.p.(provider.StreamingProvider); ok {
		// Callbacks are forwarded. provider.StreamCallbacks is a per-event
		// union — thinking streams first, then text deltas and tool-input
		// deltas can interleave. closeThinking ends the dimmed trace before
		// the first visible token of either kind.
		onThinking, closeThinking := s.thinkingRenderer()
		resp, err := sp.SendStream(ctx, req, provider.StreamCallbacks{
			OnText: func(d string) {
				closeThinking()
				if onChunk != nil {
					onChunk(d)
				}
			},
			OnToolDelta: func(id, name, j string) {
				closeThinking()
				if onToolDelta != nil {
					onToolDelta(id, name, j)
				}
			},
			OnThinking: onThinking,
		})
		closeThinking()
		if err != nil {
			return agent.Reply{}, err
		}
		return replyFromResponse(resp), nil
	}

	// Buffered fallback.
	resp, err := s.p.Send(ctx, req)
	if err != nil {
		return agent.Reply{}, err
	}
	if onChunk != nil && resp.Content != "" {
		onChunk(resp.Content)
	}
	return replyFromResponse(resp), nil
}

// Compile-time assertions: providerSender satisfies all agent sender interfaces.
var (
	_ agent.Sender              = providerSender{}
	_ agent.StreamingSender     = providerSender{}
	_ agent.ToolSender          = providerSender{}
	_ agent.ToolStreamingSender = providerSender{}
)
