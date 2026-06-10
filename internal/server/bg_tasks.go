package server

import (
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// Background-process visibility for the web UI. The frontend renders a
// "N background task(s)" badge with a popover from background_tasks_update,
// and an inline chat notice from background_task_notice; these helpers are
// the server-side emitters feeding the session's per-session
// tools.BackgroundManager state into the WebSocket stream.

// backgroundTasksUpdate builds the badge payload from a live process list.
func backgroundTasksUpdate(sessionID string, infos []tools.BgInfo, now time.Time) wsEventBackgroundTaskUpdate {
	list := make([]wsBackgroundTask, 0, len(infos))
	for _, in := range infos {
		list = append(list, wsBackgroundTask{
			HandleID: in.ID,
			Command:  in.Command,
			Elapsed:  int(now.Sub(in.Start).Seconds()),
		})
	}
	return wsEventBackgroundTaskUpdate{
		Type:      "background_tasks_update",
		SessionID: sessionID,
		Running:   len(list),
		Tasks:     list,
	}
}

// broadcastBackgroundTasks pushes the session's current background-process
// list to its subscribers. Called after every tool call — a tool call is the
// only way a turn starts or kills a process — and from the exit hook, so the
// badge tracks starts, kills, and natural completions.
func (s *Server) broadcastBackgroundTasks(sessionID string) {
	infos := tools.SessionBackgroundManager(sessionID).ListRunning()
	s.wsHub.broadcast(sessionID, backgroundTasksUpdate(sessionID, infos, time.Now()))
}

// wireBackgroundTaskNotices registers the session manager's exit hook so a
// finished background process surfaces as a chat notice, the badge count
// drops, and the model is notified. Re-registered (idempotently) at each turn
// start; the hook outlives the turn, so a process finishing between turns
// still notifies subscribed tabs — broadcasting to a session with no
// subscribers is a no-op.
func (s *Server) wireBackgroundTaskNotices(sessionID string) {
	tools.SessionBackgroundManager(sessionID).SetOnExit(func(e tools.BgExit) {
		s.wsHub.broadcast(sessionID, wsEventBackgroundTaskNotice{
			Type:      "background_task_notice",
			SessionID: sessionID,
			Command:   e.Command,
			HandleID:  e.ID,
			Status:    bgNoticeStatus(e.Status),
		})
		s.broadcastBackgroundTasks(sessionID)
		s.notifyAgentBgExit(sessionID, e)
	})
}

// notifyAgentBgExit pushes a background-completion note to the model — parity
// with the CLI/TUI's SetBackgroundOnExit → Inbox wiring. Mid-turn the note
// goes straight into the running Agent's Inbox (drained between loop
// iterations); while idle it goes to the steer queue, which the next turn
// flushes into its Inbox at start. The note is a <system-reminder> block, so
// the web transcript never renders it as user speech.
func (s *Server) notifyAgentBgExit(sessionID string, e tools.BgExit) {
	note := tools.FormatBgNote(e)
	s.sessionAgentsMu.Lock()
	a := s.sessionAgents[sessionID]
	s.sessionAgentsMu.Unlock()
	if a != nil {
		a.Inbox.Enqueue(note)
		return
	}
	s.enqueueSteer(sessionID, agent.InboxItem{Text: note})
}

// bgNoticeStatus maps a BackgroundManager exit status ("exited: 0",
// "exited: signal: killed", …) onto the frontend notice levels
// (success / cancelled / failed).
func bgNoticeStatus(status string) string {
	switch {
	case status == "exited: 0":
		return "success"
	case strings.Contains(status, "killed"):
		return "cancelled"
	default:
		return "failed"
	}
}
