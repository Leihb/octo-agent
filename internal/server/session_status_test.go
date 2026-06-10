package server

import (
	"context"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
)

// TestSessionStatus_ReflectsRunningTurn guards the interrupt-button contract:
// the frontend shows the stop button only while status == "running", and a
// session reports "running" exactly while its turn's interrupt cancel func is
// registered.
func TestSessionStatus_ReflectsRunningTurn(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.initWS()
	srv.interrupts = make(map[string]context.CancelFunc)

	if got := srv.sessionStatus("s1"); got != "idle" {
		t.Fatalf("status before turn = %q, want idle", got)
	}

	_, cancel := context.WithCancel(context.Background())
	srv.registerInterrupt("s1", cancel)
	if got := srv.sessionStatus("s1"); got != "running" {
		t.Fatalf("status during turn = %q, want running", got)
	}
	if got := srv.sessionStatus("other"); got != "idle" {
		t.Fatalf("status of other session = %q, want idle", got)
	}

	// handleWSInterrupt cancels and deregisters — status flips back to idle.
	srv.handleWSInterrupt("s1")
	if got := srv.sessionStatus("s1"); got != "idle" {
		t.Fatalf("status after interrupt = %q, want idle", got)
	}

	// The session list payload carries the live status too (page reload
	// mid-turn must still show the button).
	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, cancel2 := context.WithCancel(context.Background())
	srv.registerInterrupt(sess.ID, cancel2)
	defer cancel2()
	item := srv.toSessionItem(sess, "manual", "")
	if item.Status != "running" {
		t.Fatalf("session list status = %q, want running", item.Status)
	}
}
