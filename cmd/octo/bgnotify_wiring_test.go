package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// noopSink is a ViewSink that ignores everything — lets runTurn run without a
// terminal or spinner.
type noopSink struct{}

func (noopSink) TurnStarted()                 {}
func (noopSink) Emit(agent.AgentEvent)        {}
func (noopSink) TurnEnded(agent.Reply, error) {}
func (noopSink) Notice(string)                {}
func (noopSink) Ask(context.Context, UserPrompt) (UserResponse, error) {
	return UserResponse{Cancelled: true}, nil
}

// TestRunTurn_PrependsIdleBgNote verifies the idle path: a background-completion
// notice pushed via Steer while no turn was running is drained at the start of
// the next turn and prepended to the user's message, so the model sees it.
// (capturingSender is defined in repl_test.go.)
func TestRunTurn_PrependsIdleBgNote(t *testing.T) {
	cs := &capturingSender{stubSender: stubSender{reply: "ok"}}
	a := agent.New(cs, "m")
	// Simulate a background process finishing while the REPL was idle.
	a.Steer(formatBgNote(tools.BgExit{ID: "bg_1", Command: "go build ./...", Status: "exited: 0", NewOutput: "done"}))

	cfg := replConfig{a: a} // no tools, no memStore, no hooks
	if _, err := runTurn(context.Background(), a, cfg, noopSink{}, "what's next?"); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	got := cs.lastUserContent()
	if !strings.Contains(got, "Background process bg_1") {
		t.Errorf("turn input missing the bg notice; got:\n%s", got)
	}
	if !strings.Contains(got, "what's next?") {
		t.Errorf("turn input missing the user message; got:\n%s", got)
	}
	// The notice must precede the user's text (it's prepended context).
	if i, j := strings.Index(got, "bg_1"), strings.Index(got, "what's next?"); i > j {
		t.Errorf("bg notice should precede the user text; got:\n%s", got)
	}
	// Drained exactly once — nothing left for a second turn.
	if a.HasPendingSteer() {
		t.Error("steer buffer should be empty after the turn drained it")
	}
}

// TestTUI_BgExitMsgShowsNotice confirms an async background-exit message renders
// a scrollback notice (a non-nil print command) rather than being dropped.
func TestTUI_BgExitMsgShowsNotice(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(bgExitMsg{e: tools.BgExit{ID: "bg_1", Command: "go test ./...", Status: "exited: 0"}})
	if cmd == nil {
		t.Fatal("bgExitMsg should produce a scrollback notice command")
	}
}
