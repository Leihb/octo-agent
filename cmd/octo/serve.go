package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Leihb/octo-agent/internal/server"
)

// runServe handles `octo serve`.
func runServe(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "Bind address (e.g. :8080, 127.0.0.1:8080)")
	provider := fs.String("provider", "", "Provider: anthropic | openai")
	model := fs.String("model", "", "Model name")
	system := fs.String("system", "", "System prompt")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens for responses")
	tools := fs.Bool("tools", true, "Enable agentic tool loop")
	cors := fs.String("cors", "", "CORS allowed origins (comma-separated, * for any)")
	noChannel := fs.Bool("no-channel", false, "Disable IM channel (DingTalk, Feishu)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var corsOrigins []string
	if *cors != "" {
		for _, o := range splitComma(*cors) {
			o = strings.TrimSpace(o)
			if o != "" {
				corsOrigins = append(corsOrigins, o)
			}
		}
	}

	cfg := server.Config{
		Addr:        *addr,
		Provider:    *provider,
		Model:       *model,
		System:      *system,
		MaxTokens:   *maxTokens,
		Tools:       *tools,
		CORSOrigins: corsOrigins,
		NoChannel:   *noChannel,
	}

	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "octo serve: %v\n", err)
		return 1
	}

	host := *addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	fmt.Fprintf(stdout, "octo server listening on http://%s\n", host)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		fmt.Fprintln(stdout, "\nocto serve: shutting down...")
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(stderr, "octo serve: %v\n", err)
		return 1
	}
	return 0
}

func splitComma(s string) []string {
	return strings.Split(s, ",")
}
