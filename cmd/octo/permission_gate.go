package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/permission"
)

// newCLIGate builds the shared app permission gate wired to an interactive
// prompter: ask-class verdicts raise a KindPermission prompt through the view
// (stdin line today, modal in the TUI) and map the structured answer. A nil
// prompter yields a non-interactive gate (ask → deny).
func newCLIGate(engine *permission.Engine, ask userPrompter) agent.PermissionGate {
	return app.NewPermissionGate(engine, permissionAskFrom(ask))
}

// permissionAskFrom adapts a userPrompter (the view) into an app.PermissionAsk.
// A nil prompter returns nil, which the gate treats as non-interactive.
func permissionAskFrom(ask userPrompter) app.PermissionAsk {
	if ask == nil {
		return nil
	}
	return func(ctx context.Context, name string, input map[string]any) (bool, bool, error) {
		resp, err := ask.Ask(ctx, UserPrompt{Kind: KindPermission, ToolName: name, ToolInput: input})
		if err != nil {
			return false, false, err
		}
		return resp.Allow, resp.Always, nil
	}
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
	if s == string(permission.ModeAutoApprove) {
		return permission.ModeAutoApprove
	}
	return permission.ModeInteractive
}
