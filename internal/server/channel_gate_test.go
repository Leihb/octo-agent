package server

import (
	"context"
	"testing"
)

// TestBuildChannelGate_NonInteractive verifies the IM bridge gate the server
// builds for in-process channels: safe tools are allowed, while ask-class
// tools resolve to deny — a chat channel has no prompt to confirm over.
// (Ported from the standalone `octo channel start` command when the bridge
// merged into serve.)
func TestBuildChannelGate_NonInteractive(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	srv.cwd = t.TempDir()

	gate, err := srv.buildChannelGate()
	if err != nil {
		t.Fatalf("buildChannelGate: %v", err)
	}

	ctx := context.Background()

	// A safe, auto-allowed command runs.
	if allow, _ := gate.Check(ctx, "terminal", map[string]any{"command": "ls -la"}); !allow {
		t.Error("expected `ls -la` to be allowed")
	}

	// An ask-class command (sudo) has no allow rule → implicit ask → strict
	// mode + nil asker → deny.
	if allow, reason := gate.Check(ctx, "terminal", map[string]any{"command": "sudo rm /tmp/x"}); allow {
		t.Error("expected `sudo` to be denied in the non-interactive gate")
	} else if reason == "" {
		t.Error("expected a denial reason for the model to see")
	}

	// A hard-deny command is refused regardless of mode.
	if allow, _ := gate.Check(ctx, "terminal", map[string]any{"command": "rm -rf /"}); allow {
		t.Error("expected `rm -rf /` to be denied")
	}
}
