package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

func TestBgNoticeStatus(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"exited: 0", "success"},
		{"exited: exit status 1", "failed"},
		{"exited: signal: killed", "cancelled"},
		{"exited: something else", "failed"},
	}
	for _, c := range cases {
		if got := bgNoticeStatus(c.in); got != c.want {
			t.Errorf("bgNoticeStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBackgroundTasksUpdate_Payload(t *testing.T) {
	now := time.Now()
	infos := []tools.BgInfo{
		{ID: "bg_1", Command: "sleep 30", Start: now.Add(-12 * time.Second), Status: "running"},
		{ID: "bg_2", Command: "tail -f log", Start: now.Add(-3 * time.Second), Status: "running"},
	}
	ev := backgroundTasksUpdate("sess-1", infos, now)

	if ev.Type != "background_tasks_update" {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.Running != 2 || len(ev.Tasks) != 2 {
		t.Fatalf("Running = %d, Tasks = %d, want 2/2", ev.Running, len(ev.Tasks))
	}
	if ev.Tasks[0].HandleID != "bg_1" || ev.Tasks[0].Command != "sleep 30" || ev.Tasks[0].Elapsed != 12 {
		t.Errorf("Tasks[0] = %+v", ev.Tasks[0])
	}

	// Empty list must still marshal with running 0 and a non-null tasks array
	// so the frontend hides the badge instead of choking on null.
	b, err := json.Marshal(backgroundTasksUpdate("sess-1", nil, now))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["running"] != float64(0) {
		t.Errorf("running = %v, want 0", raw["running"])
	}
	if _, ok := raw["tasks"].([]any); !ok {
		t.Errorf("tasks = %v (%T), want JSON array", raw["tasks"], raw["tasks"])
	}
}

// TestWireBackgroundTaskNotices_BroadcastsExit is the regression guard for the
// web-UI "background tasks invisible" gap: the server defined the
// background_task_notice / background_tasks_update event types but never
// emitted them, so the frontend badge and notices never appeared.
func TestWireBackgroundTaskNotices_BroadcastsExit(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	srv.initWS()

	const sid = "bg-notice-test-session"
	defer tools.CloseSessionBackgroundManager(sid)

	conn := &wsConn{
		hub:        srv.wsHub,
		send:       make(chan []byte, 256),
		subscribed: map[string]struct{}{},
	}
	srv.wsHub.register <- conn
	srv.wsHub.subscribe(conn, sid)

	srv.wireBackgroundTaskNotices(sid)

	if _, err := tools.SessionBackgroundManager(sid).Start("echo done"); err != nil {
		t.Fatalf("start: %v", err)
	}

	var gotNotice, gotUpdate bool
	deadline := time.After(5 * time.Second)
	for !(gotNotice && gotUpdate) {
		select {
		case b := <-conn.send:
			var ev map[string]any
			if err := json.Unmarshal(b, &ev); err != nil {
				continue
			}
			switch ev["type"] {
			case "background_task_notice":
				gotNotice = true
				if ev["status"] != "success" {
					t.Errorf("notice status = %v, want success", ev["status"])
				}
				if ev["command"] != "echo done" {
					t.Errorf("notice command = %v", ev["command"])
				}
				if ev["session_id"] != sid {
					t.Errorf("notice session_id = %v", ev["session_id"])
				}
			case "background_tasks_update":
				gotUpdate = true
				if ev["running"] != float64(0) {
					t.Errorf("update running = %v, want 0 after exit", ev["running"])
				}
			}
		case <-deadline:
			t.Fatalf("timed out; notice=%v update=%v", gotNotice, gotUpdate)
		}
	}
}

// notifyAgentBgExit must reach the model on both paths: the running Agent's
// Inbox mid-turn, the steer queue while idle. Parity with the CLI/TUI's
// SetBackgroundOnExit → Inbox wiring.
func TestNotifyAgentBgExit_MidTurnGoesToInbox(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	a := agent.New(&recordingSender{}, "stub-model")
	srv.sessionAgentsMu.Lock()
	srv.sessionAgents["sess-1"] = a
	srv.sessionAgentsMu.Unlock()

	srv.notifyAgentBgExit("sess-1", tools.BgExit{ID: "bg_1", Command: "make build", Status: "exited: 0", NewOutput: "done"})

	items := a.Inbox.Drain()
	if len(items) != 1 || !strings.Contains(items[0].Text, "[BACKGROUND COMPLETED]") || !strings.Contains(items[0].Text, "bg_1") {
		t.Fatalf("inbox = %+v, want one bg note", items)
	}
	// Nothing should have leaked into the idle steer queue.
	if leftover := srv.drainSteer("sess-1"); len(leftover) != 0 {
		t.Errorf("steer queue = %+v, want empty", leftover)
	}
}

func TestNotifyAgentBgExit_IdleGoesToSteerQueue(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	srv.notifyAgentBgExit("sess-1", tools.BgExit{ID: "bg_2", Command: "make test", Status: "exited: exit status 1"})

	items := srv.drainSteer("sess-1")
	if len(items) != 1 || !strings.Contains(items[0].Text, "[BACKGROUND COMPLETED]") || !strings.Contains(items[0].Text, "bg_2") {
		t.Fatalf("steer queue = %+v, want one bg note", items)
	}
}

// An idle-time note must not just sit in the steer queue — it kicks a turn so
// the model reacts immediately (parity with the TUI's idle auto-turn). The
// kicked turn consumes the note; the reminder never renders as a user bubble
// (doAgentTurn strips <system-reminder> spans from the broadcast).
func TestDeliverModelNote_IdleKicksTurn(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	srv.wsHub = newWSHub()
	srv.turnRunning = map[string]bool{}
	srv.liveStates = map[string]*sessionLiveState{}
	srv.interrupts = map[string]context.CancelFunc{}

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv.deliverModelNote(sess.ID, "<system-reminder>\n[BACKGROUND COMPLETED]\nBackground process bg_1 (`make build`) exited: 0.\n</system-reminder>")

	// The kicked turn runs asynchronously; wait for it to finish.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu := srv.sessionTurnLock(sess.ID)
		mu.Lock()
		running := srv.turnRunning[sess.ID]
		mu.Unlock()
		if !running {
			// Either finished or never started — check results below.
			loaded, err := agent.LoadSession(sess.ID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(loaded.Messages) >= 2 {
				if leftover := srv.drainSteer(sess.ID); len(leftover) != 0 {
					t.Errorf("steer queue = %+v, want drained", leftover)
				}
				return // turn ran: user note + assistant reply persisted
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("kicked turn never completed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// prepareToolTurn must hand back the SAME session-scoped manager on every
// turn of a session (async mode), so spawns survive turn boundaries — and a
// sid-less context still gets the old per-turn synchronous manager.
func TestPrepareToolTurn_SessionScopedAsyncManager(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Tools: true})
	a := agent.New(&stubSender{}, "stub-model")

	ctx := context.WithValue(context.Background(), ctxKeySessionID{}, "sess-mgr-test")
	t.Cleanup(func() { tools.CloseSessionSubAgentManager("sess-mgr-test") })

	_, _, m1, err := srv.prepareToolTurn(ctx, a)
	if err != nil {
		t.Fatalf("prepareToolTurn: %v", err)
	}
	_, _, m2, err := srv.prepareToolTurn(ctx, a)
	if err != nil {
		t.Fatalf("prepareToolTurn: %v", err)
	}
	if m1 != m2 {
		t.Error("same session should reuse one sub-agent manager across turns")
	}
	if m1.Synchronous() {
		t.Error("session-scoped manager should be async")
	}

	_, _, anon, err := srv.prepareToolTurn(context.Background(), a)
	if err != nil {
		t.Fatalf("prepareToolTurn (no sid): %v", err)
	}
	if !anon.Synchronous() {
		t.Error("sid-less context should fall back to a synchronous manager")
	}
	if anon == m1 {
		t.Error("sid-less manager must not be the session-scoped one")
	}
}
