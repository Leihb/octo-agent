package app

import (
	"context"
	"io"
	"time"

	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/mcp"
	"github.com/Leihb/octo-agent/internal/tools"
	"github.com/Leihb/octo-agent/internal/version"
)

// mcpConnectTimeout bounds the whole non-interactive connect pass. It caps the
// per-server windows in mcp.connectOne (notably the 5-minute OAuth budget),
// since a non-interactive transport can't complete a device flow anyway — an
// OAuth server without a cached/refreshable token is skipped within this bound
// rather than hanging startup.
const mcpConnectTimeout = 60 * time.Second

// ConnectMCP loads the MCP server config under cwd, connects every server
// non-interactively, and registers the resulting surface so DefaultToolsFor /
// the tool executor pick it up. It is the server/IM counterpart to the CLI's
// own (interactive, OAuth-prompting) connect path — here authPromptFor is nil,
// so servers needing a fresh device flow are skipped (mcp.deviceFlow nil-guards
// the prompt) while stdio, header-auth, and cached-OAuth servers connect.
//
// Returns a cleanup that unregisters and closes the registry; it is always
// non-nil (a no-op when no servers connected), so callers can defer it
// unconditionally. warn receives per-server skip diagnostics.
func ConnectMCP(ctx context.Context, cwd string, warn io.Writer) (func(), error) {
	cfg, err := mcp.LoadConfig(cwd)
	if err != nil {
		return func() {}, err
	}
	if cfg == nil || len(cfg.Servers) == 0 {
		return func() {}, nil
	}

	connectCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel() // only bounds the connect pass; live connections aren't tied to it

	reg := mcp.ConnectAll(connectCtx, cfg, mcpInfo(), nil /* no interactive OAuth */, warn, nil)
	if reg.Len() == 0 {
		reg.Close()
		return func() {}, nil
	}

	tools.SetMCPRegistry(reg)
	return func() {
		tools.SetMCPRegistry(nil)
		reg.Close()
	}, nil
}

// mcpInfo is the client identity every octo entry point presents in the MCP
// initialize handshake.
func mcpInfo() mcp.Implementation {
	return mcp.Implementation{Name: "octo", Version: version.Version}
}

// SwapMCP reloads the MCP config under cwd, connects a fresh registry, and
// atomically replaces the active one (closing the old registry after the
// swap so in-flight readers holding it can finish dialing). Unlike ConnectMCP
// it installs the registry even when zero servers connected — the per-server
// connect errors it records are what a management UI displays. With zero
// servers configured the active registry is cleared. On config error the
// active registry is left untouched.
//
// Callers that expose this concurrently (the web server) must serialise calls
// themselves; two overlapping swaps would race on which registry survives.
func SwapMCP(ctx context.Context, cwd string, warn io.Writer) error {
	cfg, err := mcp.LoadConfig(cwd)
	if err != nil {
		return err
	}
	old := tools.ActiveMCPRegistry()
	var reg *mcp.Registry
	if cfg != nil && len(cfg.Servers) > 0 {
		connectCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
		defer cancel()
		reg = mcp.ConnectAll(connectCtx, cfg, mcpInfo(), nil /* no interactive OAuth */, warn, nil)
	}
	tools.SetMCPRegistry(reg)
	if old != nil {
		old.Close()
	}
	return nil
}

// mcpOAuthConnectTimeout bounds a connect that runs an interactive device
// flow: the user has to visit a URL and approve, so it gets minutes, not
// seconds. Slightly above connectOne's internal 5-minute OAuth window so
// that window — and its clearer error — wins.
const mcpOAuthConnectTimeout = 6 * time.Minute

// ConnectMCPServer connects (or reconnects) a single named server into the
// active registry, installing an empty registry first if none is active —
// the incremental path the web management API uses so adding one server
// doesn't restart every other connection. The connect error (if any) is
// recorded on the registry and returned.
func ConnectMCPServer(ctx context.Context, name string, entry mcp.ServerEntry, childStderr io.Writer) error {
	return ConnectMCPServerAuth(ctx, name, entry, nil /* no interactive OAuth */, childStderr)
}

// ConnectMCPServerAuth is ConnectMCPServer with an interactive OAuth prompt.
// A non-nil prompt on an oauth entry widens the connect window to fit a
// device flow.
func ConnectMCPServerAuth(ctx context.Context, name string, entry mcp.ServerEntry, prompt mcp.OAuthPrompt, childStderr io.Writer) error {
	reg := tools.ActiveMCPRegistry()
	if reg == nil {
		reg = mcp.NewRegistry()
		tools.SetMCPRegistry(reg)
	}
	timeout := mcpConnectTimeout
	if prompt != nil && entry.Auth == "oauth" {
		timeout = mcpOAuthConnectTimeout
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return reg.Connect(connectCtx, name, entry, mcpInfo(), prompt, childStderr)
}

// DisconnectMCPServer drops one server's live connection (and recorded
// connect error) from the active registry, if any.
func DisconnectMCPServer(name string) {
	if reg := tools.ActiveMCPRegistry(); reg != nil {
		reg.Remove(name)
	}
}

// ShutdownMCP clears the active registry and closes it. The counterpart of
// SwapMCP for process shutdown.
func ShutdownMCP() {
	old := tools.ActiveMCPRegistry()
	tools.SetMCPRegistry(nil)
	if old != nil {
		old.Close()
	}
}

// ToolSearchConfigFrom maps the persisted tools.tool_search config block onto
// the tools-package config. Unknown/empty "enabled" defaults to auto; numeric
// zero values are left for SetToolSearchConfig to backfill with its documented
// defaults. Shared by every entry point so Tool Search behaves identically.
func ToolSearchConfigFrom(c config.ToolSearchConfig) tools.ToolSearchConfig {
	mode := tools.ToolSearchAuto
	switch c.Enabled {
	case "on":
		mode = tools.ToolSearchOn
	case "off":
		mode = tools.ToolSearchOff
	}
	return tools.ToolSearchConfig{
		Mode:           mode,
		ThresholdPct:   c.ThresholdPct,
		SearchLimit:    c.SearchDefaultLimit,
		MaxSearchLimit: c.MaxSearchLimit,
	}
}
