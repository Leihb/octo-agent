package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/channel"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/weixin"
	"github.com/Leihb/octo-agent/internal/channel/adapters/weixin/ilink"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/skills"
	"github.com/Leihb/octo-agent/internal/tools"
)

// runChannel handles `octo channel start` and `octo channel login`.
func runChannel(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: octo channel <login|start> [flags]")
		return 2
	}

	switch args[0] {
	case "login":
		return runChannelLogin(args[1:], stdin, stdout, stderr)
	case "start":
		return runChannelStart(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "octo channel: unknown subcommand %q\n", args[0])
		fmt.Fprintln(stderr, "Usage: octo channel <login|start>")
		return 2
	}
}

// runChannelLogin handles `octo channel login`.
// It shows a QR code URL, polls for scan status, and saves the bot_token.
func runChannelLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("channel login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "weixin", "Platform to log in to")
	force := fs.Bool("force", false, "Force re-login even if credentials exist")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *platform != "weixin" {
		fmt.Fprintf(stderr, "octo channel login: unsupported platform %q (only 'weixin' supported)\n", *platform)
		return 2
	}

	ctx := context.Background()
	client := ilink.NewClient()

	fmt.Fprintln(stdout, "Starting WeChat iLink login...")
	fmt.Fprintln(stdout, "")

	creds, err := ilink.Login(ctx, client, ilink.LoginOptions{
		Force: *force,
		OnQRURL: func(url string) {
			fmt.Fprintf(stdout, "📱 Scan this QR code in WeChat:\n%s\n", url)
		},
		OnScanned: func() {
			fmt.Fprintln(stdout, "✓ QR code scanned — confirm login in WeChat")
		},
		OnExpired: func() {
			fmt.Fprintln(stdout, "✗ QR code expired — requesting new one")
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "octo channel login: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "")
	fmt.Fprintf(stdout, "✓ Logged in as %s\n", creds.UserID)
	fmt.Fprintf(stdout, "Credentials saved to %s\n", ilink.DefaultCredPath())
	return 0
}

// runChannelStart handles `octo channel start`.
func runChannelStart(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("channel start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "Provider: anthropic | openai")
	model := fs.String("model", "", "Model name")
	system := fs.String("system", "", "System prompt (optional)")
	bindMode := fs.String("bind-mode", "chat_user", "Session binding: chat_user | chat | user")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens for the response")
	maxTurns := fs.Int("max-turns", 0, "Max provider round-trips per message")
	noTools := fs.Bool("no-tools", false, "Disable built-in tools")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Load channel config.
	chCfg, err := channel.LoadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "octo channel: %v\n", err)
		return 1
	}
	if len(chCfg.EnabledPlatforms()) == 0 {
		fmt.Fprintln(stderr, "octo channel: no enabled platforms in ~/.octo/channels.yml")
		fmt.Fprintln(stderr, "Run `octo config` to set up channels.")
		return 1
	}

	// Resolve provider/model.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo channel: %v\n", err)
		return 1
	}
	provName, resolvedModel, ok := resolveProviderModel(*providerName, *model, cfg)
	if !ok {
		fmt.Fprintf(stderr, "octo channel: unknown provider %q\n", provName)
		return 2
	}
	// Validate credentials once (printing setup help on a missing key); the
	// per-session sender is built in the factory below with a fresh cache key.
	apiKey, err := resolveAPIKey(provName, cfg, stderr)
	if err != nil {
		return 1
	}
	baseURL := resolveBaseURL(provName, cfg)

	// Build agent factory.
	cwd, _ := os.Getwd()
	env := buildEnvContext(cwd)
	skillReg := skills.Discover(cwd)
	skillsManifest := skills.RenderManifest(skillReg)
	tools.SetSkills(skillReg)

	agentFactory := func() *agent.Agent {
		// The key was validated above; NewSender only re-errors on client
		// construction, which doesn't happen for an already-resolved key.
		llmSender, _ := app.NewSender(app.SenderOptions{
			Provider: provName,
			APIKey:   apiKey,
			BaseURL:  baseURL,
			CacheKey: newCacheKey(),
		})
		a := agent.New(llmSender, resolvedModel)
		a.CWD = cwd
		a.MaxTokens = *maxTokens
		a.MaxTurns = *maxTurns
		a.System = prompt.Compose(*system, cwd, env, skillsManifest, "", true)
		return a
	}

	mode := channel.BindingMode(*bindMode)
	mgr := channel.NewManager(chCfg, agentFactory, mode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, name := range chCfg.EnabledPlatforms() {
		pc := chCfg.Platform(name)
		if pc == nil {
			continue
		}
		ctor, err := channel.Find(name)
		if err != nil {
			fmt.Fprintf(stderr, "octo channel: %v\n", err)
			continue
		}
		ad, err := ctor(pc)
		if err != nil {
			fmt.Fprintf(stderr, "octo channel: failed to create %s adapter: %v\n", name, err)
			continue
		}
		if errs := ad.ValidateConfig(pc); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(stderr, "octo channel: %s config error: %s\n", name, e)
			}
			continue
		}

		go func(a channel.Adapter, platform string) {
			_ = a.Start(ctx, func(ev channel.InboundEvent) {
				ev.Platform = platform
				if handleCommand(mgr, a, ev) {
					return
				}
				handleAgentMessage(ctx, mgr, a, ev, !*noTools)
			})
		}(ad, name)
	}

	fmt.Fprintln(stdout, "octo channel: running. Press Ctrl-C to stop.")

	// Block on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh

	fmt.Fprintln(stdout, "\nocto channel: shutting down...")
	_ = mgr.Stop()
	return 0
}

// handleCommand processes slash commands. Returns true if the event was a command.
func handleCommand(mgr *channel.Manager, ad channel.Adapter, ev channel.InboundEvent) bool {
	text := strings.TrimSpace(ev.Text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	reply := mgr.CommandRouter(ev)
	if reply != "" {
		ad.SendText(ev.ChatID, reply, ev.MessageID)
	}
	return true
}

// handleAgentMessage runs the agent for a non-command inbound message.
func handleAgentMessage(ctx context.Context, mgr *channel.Manager, ad channel.Adapter, ev channel.InboundEvent, toolsOn bool) {
	sess := mgr.GetOrCreateSession(ev)
	if sess == nil {
		return
	}

	ctrl := channel.NewUIController(ad, ev.ChatID, ev.MessageID)

	var toolDefs []agent.ToolDefinition
	var executor agent.ToolExecutor
	if toolsOn {
		toolDefs = tools.DefaultTools()
		executor = tools.NewDefaultRegistry()
	}

	_, _ = channel.RunAgent(ctx, sess, toolDefs, executor, ctrl, ev.Text)
}
