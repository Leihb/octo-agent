// Package app is the single place that constructs provider clients and wires
// them into an agent. CLI, HTTP server, and IM bridge all bootstrap through
// here, so the provider wire packages are imported in exactly one spot and the
// one-directional dependency graph (provider → agent) stays intact.
//
// This file owns provider-client construction and the provider.Provider →
// agent.Sender adapter. Higher-level session assembly (executor, gate, MCP,
// sub-agents) lands in bootstrap.go as the migration proceeds.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/provider"
	"github.com/Leihb/octo-agent/internal/provider/anthropic"
	"github.com/Leihb/octo-agent/internal/provider/openai"
)

// Provider name constants (legacy).  New code should use vendor IDs directly.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// SenderOptions is everything needed to build an agent.Sender for a vendor.
// Key resolution and any user-facing help text stay with the caller (a CLI
// prints setup hints; a server returns an error) — this layer only constructs.
type SenderOptions struct {
	Provider string // vendor ID, e.g. "kimi", "deepseek", "anthropic", "openai"
	APIKey   string
	BaseURL  string // optional endpoint override; empty uses the vendor default

	// CacheKey is forwarded as the provider's prompt-cache key, stable across a
	// conversation's turns so the backend routes them to the same cache.
	CacheKey string
	// ThinkingBudget > 0 enables Anthropic extended thinking with this trace
	// budget; ignored by OpenAI-protocol vendors.
	ThinkingBudget int
	// ReasoningEffort ("low"|"medium"|"high") is forwarded to OpenAI-protocol
	// vendors as reasoning_effort; ignored by Anthropic-protocol vendors
	// (which use ThinkingBudget).
	ReasoningEffort string
	// ShowReasoning gates whether the reasoning/thinking trace is surfaced to
	// the agent event stream.
	ShowReasoning bool
}

// NewSender builds the provider client for opts and wraps it as an agent.Sender.
// It is the single entry point through which every transport obtains a sender.
func NewSender(opts SenderOptions) (agent.Sender, error) {
	p, err := buildClient(opts.Provider, opts.APIKey, opts.BaseURL)
	if err != nil {
		return nil, err
	}
	return sender{
		p:               p,
		cacheKey:        opts.CacheKey,
		thinkingBudget:  opts.ThinkingBudget,
		reasoningEffort: opts.ReasoningEffort,
		showReasoning:   opts.ShowReasoning,
	}, nil
}

// DefaultBaseURL returns the vendor's built-in API endpoint for the given
// provider ID, or "" for an unknown one. Exposed so callers can show the
// effective endpoint without importing the vendor packages themselves.
//
// Deprecated: prefer VendorBaseURL(id) from provider.go.
func DefaultBaseURL(providerName string) string {
	return VendorBaseURL(providerName)
}

// buildClient constructs the vendor client and applies an optional base-URL
// override. The caller is responsible for having resolved a non-empty key.
func buildClient(name, apiKey, baseURL string) (provider.Provider, error) {
	v := vendorByID(name)
	if v == nil {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	if v.CustomEndpoint && baseURL == "" {
		return nil, fmt.Errorf("provider %q requires a base URL (set %s_BASE_URL or base_url in config)",
			name, strings.ToUpper(name))
	}

	switch v.Protocol {
	case "anthropic":
		client, err := anthropic.New(apiKey)
		if err != nil {
			return nil, err
		}
		if baseURL != "" {
			client.BaseURL = baseURL
		}
		return client, nil
	case "openai":
		client, err := openai.New(apiKey)
		if err != nil {
			return nil, err
		}
		if baseURL != "" {
			client.BaseURL = baseURL
		}
		return client, nil
	default:
		return nil, fmt.Errorf("unknown protocol %q for provider %q", v.Protocol, name)
	}
}

// sender adapts a provider.Provider into agent.Sender. Keeping the adapter here
// means the agent package never imports provider — a one-directional dep graph
// that pays off as more provider implementations land.
type sender struct {
	p               provider.Provider
	cacheKey        string
	thinkingBudget  int
	reasoningEffort string
	showReasoning   bool
}

// reasoningSink returns the OnThinking callback to hand the provider: the
// agent's onThinking when reasoning display is enabled, else nil so the
// provider skips surfacing reasoning entirely.
func (s sender) reasoningSink(onThinking func(string)) func(string) {
	if !s.showReasoning || onThinking == nil {
		return nil
	}
	return onThinking
}

func (s sender) SendMessages(ctx context.Context, model, system string, msgs []agent.Message, maxTokens int) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("app: provider is nil")
	}
	resp, err := s.p.Send(ctx, provider.Request{
		Model:           model,
		SystemPrompt:    system,
		Messages:        msgs,
		MaxTokens:       maxTokens,
		CacheKey:        s.cacheKey,
		ThinkingBudget:  s.thinkingBudget,
		ReasoningEffort: s.reasoningEffort,
	})
	if err != nil {
		return agent.Reply{}, err
	}
	return replyFromResponse(resp), nil
}

// StreamMessages delegates to the provider's SendStream when it implements
// provider.StreamingProvider, else falls back to the buffered Send path and
// synthesises a single onChunk call with the full content.
func (s sender) StreamMessages(
	ctx context.Context,
	model, system string,
	msgs []agent.Message,
	maxTokens int,
	onChunk func(string),
	onThinking func(string),
) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("app: provider is nil")
	}
	req := provider.Request{
		Model:           model,
		SystemPrompt:    system,
		Messages:        msgs,
		MaxTokens:       maxTokens,
		CacheKey:        s.cacheKey,
		ThinkingBudget:  s.thinkingBudget,
		ReasoningEffort: s.reasoningEffort,
	}
	if sp, ok := s.p.(provider.StreamingProvider); ok {
		resp, err := sp.SendStream(ctx, req, provider.StreamCallbacks{
			OnText:     onChunk,
			OnThinking: s.reasoningSink(onThinking),
		})
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

// SendMessagesWithTools implements agent.ToolSender.
func (s sender) SendMessagesWithTools(
	ctx context.Context,
	model, system string,
	msgs []agent.Message,
	maxTokens int,
	toolDefs []agent.ToolDefinition,
) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("app: provider is nil")
	}
	resp, err := s.p.Send(ctx, provider.Request{
		Model:           model,
		SystemPrompt:    system,
		Messages:        msgs,
		MaxTokens:       maxTokens,
		CacheKey:        s.cacheKey,
		Tools:           toolDefs,
		ThinkingBudget:  s.thinkingBudget,
		ReasoningEffort: s.reasoningEffort,
	})
	if err != nil {
		return agent.Reply{}, err
	}
	return replyFromResponse(resp), nil
}

// StreamMessagesWithTools implements agent.ToolStreamingSender.
func (s sender) StreamMessagesWithTools(
	ctx context.Context,
	model, system string,
	msgs []agent.Message,
	maxTokens int,
	toolDefs []agent.ToolDefinition,
	onChunk func(string),
	onToolDelta agent.ToolInputDeltaFunc,
	onThinking agent.ThinkingDeltaFunc,
) (agent.Reply, error) {
	if s.p == nil {
		return agent.Reply{}, errors.New("app: provider is nil")
	}
	req := provider.Request{
		Model:           model,
		SystemPrompt:    system,
		Messages:        msgs,
		MaxTokens:       maxTokens,
		CacheKey:        s.cacheKey,
		Tools:           toolDefs,
		ThinkingBudget:  s.thinkingBudget,
		ReasoningEffort: s.reasoningEffort,
	}
	if sp, ok := s.p.(provider.StreamingProvider); ok {
		resp, err := sp.SendStream(ctx, req, provider.StreamCallbacks{
			OnText:      onChunk,
			OnToolDelta: onToolDelta,
			OnThinking:  s.reasoningSink(onThinking),
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

// TestConnection pings the provider with a minimal request to verify that
// the API key, base URL, and model all work. It returns a descriptive error
// on failure (auth, model not found, network, etc.).
func TestConnection(ctx context.Context, providerName, apiKey, baseURL, model string) error {
	p, err := buildClient(providerName, apiKey, baseURL)
	if err != nil {
		return err
	}
	_, err = p.Send(ctx, provider.Request{
		Model:     model,
		Messages:  []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
		MaxTokens: 1,
	})
	return err
}

// Compile-time assertions: sender satisfies all agent sender interfaces.
var (
	_ agent.Sender              = sender{}
	_ agent.StreamingSender     = sender{}
	_ agent.ToolSender          = sender{}
	_ agent.ToolStreamingSender = sender{}
)
