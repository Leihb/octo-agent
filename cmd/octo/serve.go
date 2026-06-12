package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Leihb/octo-agent/internal/server"
)

// serveLogLevel reads OCTO_LOG_LEVEL (debug|info|warn|error); defaults to info.
func serveLogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OCTO_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// runServe handles `octo serve`.
func runServe(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "Bind address (e.g. 127.0.0.1:8080; :8080 to expose on all interfaces)")
	accessKey := fs.String("access-key", "", "Access key for non-localhost clients (default: from OCTO_ACCESS_KEY / config.yml, else auto-generated and persisted)")
	provider := fs.String("provider", "", "Provider: anthropic | openai")
	model := fs.String("model", "", "Model name")
	system := fs.String("system", "", "System prompt")
	maxTokens := fs.Int("max-tokens", 0, "max_tokens for responses")
	tools := fs.Bool("tools", true, "Enable agentic tool loop")
	cors := fs.String("cors", "", "CORS allowed origins (comma-separated, * for any)")
	noChannel := fs.Bool("no-channel", false, "Disable IM channel (DingTalk, Feishu)")
	noMemory := fs.Bool("no-memory", false, "Disable cross-session memory injection")
	noSupervisor := fs.Bool("no-supervisor", false, "Run the server directly, without the self-restart supervisor (exit code 42 still signals a restart request to an external supervisor)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if shouldSupervise(*noSupervisor, os.Getenv(serveWorkerEnv)) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		return superviseLoop(spawnServeWorker(args, stdout, stderr), sigCh, stderr)
	}

	// Structured operational logging for the serve worker: slog text handler to
	// stderr, level from OCTO_LOG_LEVEL. Capturing this output (a file, the
	// systemd journal) is the service manager's job — see packaging/.
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: serveLogLevel()})))

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
		NoMemory:    *noMemory,
		AccessKey:   *accessKey,
		UpdateCheck: true,
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
	if !bindIsLoopback(*addr) {
		// Exposed bind: non-loopback clients must present the access key.
		// The bootstrap URL is the distribution channel — the web UI adopts
		// the query parameter into a cookie and strips it from the URL.
		fmt.Fprintln(stdout, "access key required for non-localhost clients")
		fmt.Fprintf(stdout, "open: http://%s/?access_key=%s\n", displayURLHost(*addr), url.QueryEscape(srv.AccessKey()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		fmt.Fprintln(stdout, "\nocto serve: shutting down...")
		_ = srv.Shutdown(ctx)
	}()

	err = srv.ListenAndServe()
	switch {
	case err == nil:
	case errors.Is(err, server.ErrRestartRequested):
		fmt.Fprintln(stdout, "octo serve: restarting...")
	default:
		fmt.Fprintf(stderr, "octo serve: %v\n", err)
	}
	return serveExitCode(err)
}

// shouldSupervise decides whether `octo serve` runs as the supervisor parent
// (the default) or as the worker. The worker marker env covers both the
// supervisor's own child and users running under an external supervisor;
// --no-supervisor is the explicit flag form of the same opt-out.
func shouldSupervise(noSupervisor bool, workerEnv string) bool {
	return !noSupervisor && workerEnv != "1"
}

// serveExitCode maps the worker's ListenAndServe result onto the process
// exit-code contract: clean stop → 0, restart request → ExitRestart,
// anything else → 1.
func serveExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, server.ErrRestartRequested):
		return server.ExitRestart
	default:
		return 1
	}
}

func splitComma(s string) []string {
	return strings.Split(s, ",")
}

// bindIsLoopback reports whether a bind address only accepts loopback
// clients. An empty host (":8080") and the wildcard addresses listen on all
// interfaces, so they are not loopback.
func bindIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// displayURLHost picks the host clients should use to reach the server: a
// specific bind host as-is; for a wildcard bind, the machine's primary
// non-loopback IPv4, falling back to a placeholder.
func displayURLHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return net.JoinHostPort(host, port)
	}
	if ip := primaryLANIP(); ip != "" {
		return net.JoinHostPort(ip, port)
	}
	return "<host>:" + port
}

// primaryLANIP returns the first non-loopback IPv4 interface address, or ""
// when none is up.
func primaryLANIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.IsLoopback() || ip.To4() == nil || !ip.IsGlobalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}
