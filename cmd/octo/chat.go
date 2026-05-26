package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
	"github.com/Leihb/octo-agent/internal/provider/anthropic"
	"github.com/Leihb/octo-agent/internal/provider/openai"
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
	enableTools := fs.Bool("tools", false, "Enable built-in tools (bash) for agentic loop")

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

	a := agent.New(providerSender{p: prov}, resolvedModel)
	a.System = *system
	a.MaxTokens = *maxTokens

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
			if sess.System != "" {
				a.System = sess.System
			}
		} else {
			sess = agent.NewSession(resolvedModel, *system)
		}

		cfg := replConfig{
			a:       a,
			session: sess,
			noSave:  *noSave,
			stdin:   stdin,
			stdout:  stdout,
			stderr:  stderr,
		}
		if *enableTools {
			cfg.tools = tools.DefaultTools()
			cfg.executor = tools.DefaultRegistry{}
		}
		return runREPL(cfg)
	}

	// ── Single-turn mode (original M2 behaviour) ──────────────────────────────
	if *stream {
		_, err := a.TurnStream(context.Background(), userInput, func(d string) {
			fmt.Fprint(stdout, d)
		})
		if err != nil {
			fmt.Fprintf(stderr, "\nocto chat: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout)
		return 0
	}

	reply, err := a.Turn(context.Background(), userInput)
	if err != nil {
		fmt.Fprintf(stderr, "octo chat: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, reply.Content)
	return 0
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
type providerSender struct{ p provider.Provider }

func (s providerSender) SendMessages(ctx context.Context, model, system string, msgs []agent.Message, maxTokens int) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("providerSender: provider is nil")
	}
	resp, err := s.p.Send(ctx, provider.Request{
		Model:        model,
		SystemPrompt: system,
		Messages:     msgs,
		MaxTokens:    maxTokens,
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
		Model:        model,
		SystemPrompt: system,
		Messages:     msgs,
		MaxTokens:    maxTokens,
	}
	if sp, ok := s.p.(provider.StreamingProvider); ok {
		// Text-only path: no tool deltas applicable.
		resp, err := sp.SendStream(ctx, req, provider.StreamCallbacks{OnText: onChunk})
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
		Content:      resp.Content,
		Blocks:       resp.Blocks,
		Model:        resp.Model,
		StopReason:   resp.StopReason,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
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
		Model:        model,
		SystemPrompt: system,
		Messages:     msgs,
		MaxTokens:    maxTokens,
		Tools:        tools,
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
		Model:        model,
		SystemPrompt: system,
		Messages:     msgs,
		MaxTokens:    maxTokens,
		Tools:        tools,
	}
	if sp, ok := s.p.(provider.StreamingProvider); ok {
		// Both callbacks are forwarded. provider.StreamCallbacks is a
		// per-event union — text deltas and tool-input deltas can
		// interleave on the wire (the Anthropic stream actually does that
		// when the LLM mixes prose with tool calls).
		resp, err := sp.SendStream(ctx, req, provider.StreamCallbacks{
			OnText:      onChunk,
			OnToolDelta: onToolDelta,
		})
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
