package main

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Leihb/octo-agent/internal/mcp"
)

// cliOAuthPrompt is the user-facing implementation of mcp.OAuthPrompt for
// `octo`. ShowAuthorization prints a small card telling the user
// where to go; Progress just bumps a dot counter so the user can see we're
// actively polling; Done closes the card.
//
// The card looks like:
//
//	┌─ Authorize "lark-project" ──────────────────────────────
//	│ Open: https://project.larksuite.com/b/auth/mcp
//	│ Code: ABCD-EFGH
//	│ Waiting for authorization… ······
//	└─────────────────────────────────────────────────────────
type cliOAuthPrompt struct {
	out        io.Writer
	serverName string

	mu        sync.Mutex
	progress  int
	completed bool
}

func newCLIOAuthPrompt(out io.Writer, serverName string) *cliOAuthPrompt {
	return &cliOAuthPrompt{out: out, serverName: serverName}
}

func (p *cliOAuthPrompt) ShowAuthorization(userCode, verificationURI, verificationURIComplete string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	border := strings.Repeat("─", 56)
	fmt.Fprintf(p.out, "\n┌─ Authorize %q %s\n", p.serverName, border)
	fmt.Fprintf(p.out, "│ Open: %s\n", verificationURI)
	fmt.Fprintf(p.out, "│ Code: %s\n", userCode)
	if verificationURIComplete != "" && verificationURIComplete != verificationURI {
		fmt.Fprintf(p.out, "│ One-step link (code pre-filled): %s\n", verificationURIComplete)
	}
	fmt.Fprint(p.out, "│ Waiting for authorization… ")
}

// Progress emits one dot per poll cycle. Wraps to a fresh line every 40
// dots so the indicator doesn't run past the terminal edge.
func (p *cliOAuthPrompt) Progress() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed {
		return
	}
	p.progress++
	if p.progress%40 == 0 {
		fmt.Fprint(p.out, "\n│ ")
	}
	fmt.Fprint(p.out, "·")
}

func (p *cliOAuthPrompt) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed {
		return
	}
	p.completed = true
	border := strings.Repeat("─", 56)
	fmt.Fprintf(p.out, "\n│ Authorized.\n└%s\n\n", border+"────────")
}

// Compile-time check that we still implement the interface.
var _ mcp.OAuthPrompt = (*cliOAuthPrompt)(nil)

// tuiOAuthPrompt surfaces the OAuth device flow through the TUI sink rather than
// the raw terminal, which bubbletea owns once the program is running. It only
// fires when a server has no usable cached token — the steady-state silent
// refresh path never calls ShowAuthorization, so background-connecting under
// the TUI normally produces no prompt at all.
type tuiOAuthPrompt struct {
	sink       *tuiSink
	serverName string
}

func newTUIOAuthPrompt(sink *tuiSink, serverName string) *tuiOAuthPrompt {
	return &tuiOAuthPrompt{sink: sink, serverName: serverName}
}

func (p *tuiOAuthPrompt) ShowAuthorization(userCode, verificationURI, verificationURIComplete string) {
	if p.sink == nil {
		return
	}
	link := verificationURI
	if verificationURIComplete != "" {
		link = verificationURIComplete
	}
	p.sink.Notice(fmt.Sprintf("MCP %q needs authorization — open %s (code: %s)", p.serverName, link, userCode))
}

// Progress is a no-op: a per-poll dot stream would spam the scrollback. The
// user already has the link from ShowAuthorization and Done confirms success.
func (p *tuiOAuthPrompt) Progress() {}

func (p *tuiOAuthPrompt) Done() {
	if p.sink != nil {
		p.sink.Notice(fmt.Sprintf("MCP %q authorized.", p.serverName))
	}
}

var _ mcp.OAuthPrompt = (*tuiOAuthPrompt)(nil)

// sinkWriter adapts the TUI sink into an io.Writer for ConnectAll's warn param,
// so a "server skipped" line shows up as a scrollback notice instead of
// corrupting the bubbletea frame.
type sinkWriter struct{ sink *tuiSink }

func (w sinkWriter) Write(b []byte) (int, error) {
	if w.sink != nil {
		if s := strings.TrimRight(string(b), "\n"); s != "" {
			w.sink.Notice(s)
		}
	}
	return len(b), nil
}
