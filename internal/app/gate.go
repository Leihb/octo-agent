package app

import (
	"context"
	"fmt"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/tools"
)

// PermissionAsk prompts the user to approve one tool call (already unwrapped to
// its real tool name). It returns whether to allow the call and whether to
// remember that decision for the rest of the session. A nil PermissionAsk makes
// the gate non-interactive: ask-class verdicts resolve straight to deny.
type PermissionAsk func(ctx context.Context, toolName string, toolInput map[string]any) (allow, remember bool, err error)

// NewPermissionGate builds the single agent.PermissionGate every transport
// uses. The engine resolves allow/deny/ask policy. Pass a non-nil ask for an
// interactive transport (the CLI prompts the user on ask-class verdicts); pass
// nil for a non-interactive one (HTTP server, IM bridge), where ask resolves to
// deny — the same posture the old per-transport gates had.
func NewPermissionGate(engine *permission.Engine, ask PermissionAsk) agent.PermissionGate {
	return &permissionGate{engine: engine, ask: ask}
}

type permissionGate struct {
	engine *permission.Engine
	ask    PermissionAsk
}

// Check implements agent.PermissionGate.
func (g *permissionGate) Check(ctx context.Context, name string, input map[string]any) (bool, string) {
	// A Tool Search mcp_call wraps the real MCP tool — evaluate policy (and
	// prompt) against the wrapped tool, not the opaque "mcp_call" bridge.
	if real, realInput, ok := tools.ToolCallTarget(name, input); ok {
		name, input = real, realInput
	}
	switch g.engine.Check(name, input) {
	case permission.Allow:
		return true, ""
	case permission.Deny:
		return false, g.engine.DenialReason(name, input)
	case permission.Ask:
		if g.ask == nil {
			// Non-interactive: resolve ask → deny with the policy's reason.
			return false, g.engine.DenialReason(name, input)
		}
		allow, remember, err := g.ask(ctx, name, input)
		if err != nil || !allow {
			return false, fmt.Sprintf("permission_denied: user declined to run %s", name)
		}
		if remember {
			g.engine.Remember(name, input, permission.Allow)
		}
		return true, ""
	}
	return false, g.engine.DenialReason(name, input)
}
