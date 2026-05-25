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
)

// defaultChatModel is the model used when --model is not supplied. Haiku is
// the cheapest reasoning-capable Anthropic model, the right default for a
// scaffold whose primary purpose is verifying the wire end-to-end.
const defaultChatModel = "claude-haiku-4-5-20251001"

// runChat handles `octo chat [flags] <message>`. It builds an Agent backed by
// the Anthropic Messages API and runs a single Turn — REPL / multi-turn
// loops land in M3 alongside session persistence.
func runChat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	model := fs.String("model", defaultChatModel, "Anthropic model name")
	system := fs.String("system", "", "System prompt (optional)")
	maxTokens := fs.Int("max-tokens", anthropic.DefaultMaxTokens, "max_tokens for the response")

	if err := fs.Parse(args); err != nil {
		// flag already printed the help/error; ParseError → exit 2.
		return 2
	}

	userInput := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if userInput == "" {
		fmt.Fprintln(stderr, "octo chat: provide a message as a positional argument")
		fmt.Fprintln(stderr, "Usage: octo chat [--model <name>] [--system <prompt>] <message>")
		return 2
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(stderr, "octo chat: ANTHROPIC_API_KEY environment variable is not set")
		return 1
	}

	client, err := anthropic.New(apiKey)
	if err != nil {
		fmt.Fprintf(stderr, "octo chat: %v\n", err)
		return 1
	}
	// Honour ANTHROPIC_BASE_URL (same env name the official Anthropic SDK uses)
	// so the same `octo chat` invocation can target an Anthropic-compatible
	// third party — e.g. DeepSeek at https://api.deepseek.com/anthropic.
	// Without the env var the client uses anthropic.DefaultBaseURL.
	if baseURL := os.Getenv("ANTHROPIC_BASE_URL"); baseURL != "" {
		client.BaseURL = baseURL
	}

	a := agent.New(providerSender{p: client}, *model)
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

// providerSender adapts a provider.Provider into agent.Sender. Keeping the
// adapter in cmd/octo means the agent package never imports provider — a
// one-directional dep graph that pays off as more provider implementations
// land in M2.
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
