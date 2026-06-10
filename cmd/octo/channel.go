package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	// The IM adapters self-register into the channel registry at init time.
	// These imports keep them linked into the binary for every subcommand —
	// octo serve runs them, and the channels web panel / task notify look
	// them up by name.
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/dingtalk"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/discord"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/feishu"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/telegram"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/wecom"
	_ "github.com/Leihb/octo-agent/internal/channel/adapters/weixin"
	"github.com/Leihb/octo-agent/internal/channel/adapters/weixin/ilink"
)

// runChannel handles `octo channel login`. The IM bridge itself runs inside
// `octo serve` — the former `octo channel start` command was folded into it.
func runChannel(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: octo channel login [flags]")
		return 2
	}

	switch args[0] {
	case "login":
		return runChannelLogin(args[1:], stdin, stdout, stderr)
	case "start":
		fmt.Fprintln(stderr, "octo channel start was removed: IM channels now run inside `octo serve`.")
		fmt.Fprintln(stderr, "Enable platforms in ~/.octo/channels.yml (or the web Channels panel) and run `octo serve`.")
		return 2
	default:
		fmt.Fprintf(stderr, "octo channel: unknown subcommand %q\n", args[0])
		fmt.Fprintln(stderr, "Usage: octo channel login")
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
