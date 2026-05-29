package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Leihb/octo-agent/internal/permission"
)

// cliPermissionGate adapts a permission.Engine into an agent.PermissionGate.
// It resolves allow/deny verdicts directly and, for ask verdicts in
// interactive mode, raises a KindPermission prompt through the view (stdin
// line today, modal in the TUI) and maps the structured answer.
//
// In strict mode the engine has already collapsed ask → deny, so the prompt
// path is never reached — Check just returns the denial with its reason.
type cliPermissionGate struct {
	engine *permission.Engine
	ask    userPrompter // the view; raises the approval prompt
}

// Check implements agent.PermissionGate.
func (g *cliPermissionGate) Check(ctx context.Context, name string, input map[string]any) (bool, string) {
	switch g.engine.Check(name, input) {
	case permission.Allow:
		return true, ""
	case permission.Deny:
		return false, g.engine.DenialReason(name, input)
	case permission.Ask:
		return g.prompt(ctx, name, input)
	}
	return false, g.engine.DenialReason(name, input)
}

// prompt raises the approval request and maps the answer: allow once, allow +
// remember for the session, or deny (incl. empty / no view).
func (g *cliPermissionGate) prompt(ctx context.Context, name string, input map[string]any) (bool, string) {
	if g.ask == nil {
		return false, fmt.Sprintf("permission_denied: user declined to run %s", name)
	}
	resp, err := g.ask.Ask(ctx, UserPrompt{Kind: KindPermission, ToolName: name, ToolInput: input})
	if err != nil || !resp.Allow {
		return false, fmt.Sprintf("permission_denied: user declined to run %s", name)
	}
	if resp.Always {
		g.engine.Remember(name, input, permission.Allow)
	}
	return true, ""
}

// permissionConfigPath returns ~/.octo/permissions.yml. An empty string is
// returned (and the engine falls back to embedded defaults) when the home
// directory can't be resolved.
func permissionConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".octo", "permissions.yml")
}

// resolvePermissionMode maps the --permission-mode flag string onto a
// permission.Mode. Unknown values fall back to interactive (the safe,
// CLI-friendly default) and the caller is expected to have validated.
func resolvePermissionMode(s string) permission.Mode {
	if s == string(permission.ModeStrict) {
		return permission.ModeStrict
	}
	return permission.ModeInteractive
}
