package server

import (
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/Leihb/octo-agent/internal/permission"
)

// TestChannelPerTurnGate verifies the IM per-turn gate end to end: the same
// engine + chat-ask combination handleChannelMessage builds. Allow rules run
// without prompting, ask-class tools prompt in the chat and follow the reply,
// hard denies never prompt.
func TestChannelPerTurnGate(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	srv.cwd = t.TempDir()

	sess := &channel.Session{}
	ad := &drainTestAdapter{}
	ev := channel.InboundEvent{ChatID: "c1", MessageID: "m1"}

	engine, err := permission.New(permissionConfigPath(), srv.cwd, resolvePermissionMode(), srv.memDir, srv.homeMemDir)
	if err != nil {
		t.Fatalf("permission engine: %v", err)
	}
	gate := app.NewPermissionGate(engine, srv.channelPermissionAsk(sess, ad, ev))

	ctx := context.Background()

	// A safe, auto-allowed command runs without prompting the chat.
	if allow, _ := gate.Check(ctx, "terminal", map[string]any{"command": "ls -la"}); !allow {
		t.Error("expected `ls -la` to be allowed")
	}
	if n := len(ad.texts()); n != 0 {
		t.Fatalf("allow-class check must not prompt; got %d prompts", n)
	}

	// An ask-class command (sudo) prompts in the chat; a non-affirmative
	// reply denies.
	checkDone := make(chan struct{})
	var allow bool
	var reason string
	go func() {
		allow, reason = gate.Check(ctx, "terminal", map[string]any{"command": "sudo rm /tmp/x"})
		close(checkDone)
	}()
	waitFor(t, func() bool { return len(ad.texts()) == 1 })
	if !strings.Contains(ad.texts()[0], "terminal") {
		t.Errorf("prompt %q should name the tool", ad.texts()[0])
	}
	if !sess.DeliverAskReply("不行") {
		t.Fatal("ask slot not armed")
	}
	<-checkDone
	if allow {
		t.Error("expected `sudo` to be denied after a non-affirmative reply")
	}
	if reason == "" {
		t.Error("expected a denial reason for the model to see")
	}

	// A hard-deny command is refused without prompting.
	before := len(ad.texts())
	if allow, _ := gate.Check(ctx, "terminal", map[string]any{"command": "rm -rf /"}); allow {
		t.Error("expected `rm -rf /` to be denied")
	}
	if len(ad.texts()) != before {
		t.Error("hard-deny must not prompt the chat")
	}
}
