package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Leihb/octo/internal/agent"
	"github.com/Leihb/octo/internal/provider"
	"github.com/Leihb/octo/internal/provider/anthropic"
	"github.com/Leihb/octo/internal/provider/openai"
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

// runChat handles `octo chat [flags] <message>`. It builds an Agent backed by
// the selected provider (Anthropic or OpenAI) and runs a single Turn — REPL /
// multi-turn loops land in M3 alongside session persistence.
func runChat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", providerAnthropic, "Provider: anthropic | openai")
	model := fs.String("model", "", "Model name (defaults to the provider's cheapest reasoning model)")
	system := fs.String("system", "", "System prompt (optional)")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens for the response (0 = provider default)")

	if err := fs.Parse(args); err != nil {
		// flag already printed the help/error; ParseError → exit 2.
		return 2
	}

	userInput := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if userInput == "" {
		fmt.Fprintln(stderr, "octo chat: provide a message as a positional argument")
		fmt.Fprintln(stderr, "Usage: octo chat [--provider anthropic|openai] [--model <name>] [--system <prompt>] <message>")
		return 2
	}

	resolvedModel := *model
	if resolvedModel == "" {
		resolvedModel = defaultModels[*providerName]
	}
	if resolvedModel == "" {
		fmt.Fprintf(stderr, "octo chat: unknown provider %q (use 'anthropic' or 'openai')\n", *providerName)
		return 2
	}

	prov, err := buildProvider(*providerName, stderr)
	if err != nil {
		// buildProvider has already printed the user-facing reason.
		return 1
	}

	a := agent.New(providerSender{p: prov}, resolvedModel)
	a.System = *system
	a.MaxTokens = *maxTokens

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
	return agent.Reply{
		Content:      resp.Content,
		Model:        resp.Model,
		StopReason:   resp.StopReason,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
	}, nil
}
