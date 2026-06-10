package server

import (
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
