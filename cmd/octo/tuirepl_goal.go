package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/taskgraph"
	tea "github.com/charmbracelet/bubbletea"
)

// ── /goal: task-DAG orchestration inside the TUI ──
//
// /goal brings the `octo goal` DAG planner+scheduler into the interactive
// session. The flow is plan → review → confirm → run, all driven through the
// bubbletea message loop (PlanTask and Scheduler.Run are blocking network/agent
// calls, so they run in background goroutines that Send results back rather than
// blocking Update). The confirm step reuses the existing Question modal.

type goalPlannedMsg struct {
	task *taskgraph.Task
	err  error
}
type goalRunMsg struct{ task *taskgraph.Task }
type goalCancelledMsg struct{ task *taskgraph.Task }
type goalDoneMsg struct {
	task *taskgraph.Task
	err  error
}

// dispatchGoal routes the text after "/goal". A leading list/resume keyword
// picks the matching sub-action; anything else is treated as the goal text to
// plan.
func (m *tuiModel) dispatchGoal(args string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(args)
	if len(fields) == 0 || strings.ToLower(fields[0]) == "help" {
		return m, tea.Println(goalUsage())
	}
	switch strings.ToLower(fields[0]) {
	case "list", "ls":
		var b bytes.Buffer
		runTaskList(nil, &b, &b)
		return m, tea.Println(strings.TrimRight(b.String(), "\n"))
	case "resume":
		if len(fields) < 2 {
			return m, tea.Println(noticeStyle.Render("Usage: /goal resume <id>"))
		}
		return m.goalResume(fields[1])
	default:
		return m, m.startGoalPlan(args)
	}
}

func goalUsage() string {
	return noticeStyle.Render(strings.Join([]string{
		"Usage:",
		"  /goal <text>        Plan a goal as a task DAG, confirm, then run it",
		"  /goal list          List planned / run goals",
		"  /goal resume <id>   Re-run a goal's failed / skipped subtasks",
	}, "\n"))
}

// startGoalPlan kicks off the planner side-call in the background, then persists
// the resulting DAG. It occupies the session (turnRunning) so no other turn
// starts while the goal is in flight. The result returns via goalPlannedMsg.
func (m *tuiModel) startGoalPlan(goal string) tea.Cmd {
	m.turnRunning = true
	m.streaming = false
	m.turnStart = time.Now()
	m.spinnerFrame = 0
	m.running = nil
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	a := m.a
	prog := m.sink.prog
	go func() {
		defer cancel()
		res, err := a.PlanTask(ctx, goal)
		if err != nil {
			prog.Send(goalPlannedMsg{err: err})
			return
		}
		if len(res.Subtasks) == 0 {
			prog.Send(goalPlannedMsg{err: fmt.Errorf("planner returned no subtasks — refine the goal and try again")})
			return
		}
		subs := make([]taskgraph.Subtask, 0, len(res.Subtasks))
		for i, ps := range res.Subtasks {
			subs = append(subs, taskgraph.Subtask{
				ID:          i + 1,
				Description: ps.Description,
				BlockedBy:   ps.BlockedBy,
				Status:      taskgraph.SubtaskPending,
			})
		}
		store, err := taskgraph.NewStore()
		if err != nil {
			prog.Send(goalPlannedMsg{err: err})
			return
		}
		task, err := store.Create(goal, subs)
		prog.Send(goalPlannedMsg{task: task, err: err})
	}()
	return tea.Batch(
		tea.Println(promptStyle.Render("you> ")+"/goal "+goal),
		tea.Println(noticeStyle.Render("Planning…")),
		tickCmd(),
	)
}

// onGoalPlanned renders the planned DAG and opens a confirm modal. A one-shot
// goroutine bridges the modal's answer channel back into goalRunMsg /
// goalCancelledMsg so the existing Question modal machinery is reused verbatim.
func (m *tuiModel) onGoalPlanned(msg goalPlannedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.turnRunning = false
		m.cancelTurn = nil
		return m, tea.Println(errorStyle.Render("goal: " + msg.err.Error()))
	}

	var b bytes.Buffer
	printPlannedDAG(&b, msg.task)
	planLine := tea.Println(strings.TrimRight(b.String(), "\n"))

	ch := make(chan UserResponse, 1)
	m.openModal(askMsg{
		prompt: UserPrompt{
			Kind:     KindQuestion,
			Header:   "goal",
			Question: fmt.Sprintf("Run this plan (%s)?", msg.task.ShortID()),
			Options:  []string{"Run it", "Cancel"},
		},
		resp: ch,
	})
	task := msg.task
	prog := m.sink.prog
	go func() {
		r := <-ch
		if r.Cancelled || len(r.Choices) == 0 || r.Choices[0] != "Run it" {
			prog.Send(goalCancelledMsg{task: task})
			return
		}
		prog.Send(goalRunMsg{task: task})
	}()
	return m, planLine
}

// startGoalRun drives the scheduler in the background. Scheduler progress is
// written line-by-line into the scrollback via teaScrollbackWriter; completion
// returns via goalDoneMsg. Reused by both fresh-plan confirmation and resume.
func (m *tuiModel) startGoalRun(taskID string) tea.Cmd {
	m.turnRunning = true
	m.streaming = false
	m.turnStart = time.Now()
	m.spinnerFrame = 0
	m.running = nil
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	prog := m.sink.prog
	go func() {
		defer cancel()
		store, err := taskgraph.NewStore()
		if err != nil {
			prog.Send(goalDoneMsg{err: err})
			return
		}
		w := &teaScrollbackWriter{prog: prog}
		sch := taskgraph.NewScheduler(store, &spawnerExecutor{}, w)
		runErr := sch.Run(ctx, taskID)
		task, _ := store.Get(taskID)
		prog.Send(goalDoneMsg{task: task, err: runErr})
	}()
	return tea.Batch(
		tea.Println(noticeStyle.Render("Running…")),
		tickCmd(),
	)
}

// onGoalDone reports the final task state and releases the session.
func (m *tuiModel) onGoalDone(msg goalDoneMsg) (tea.Model, tea.Cmd) {
	m.turnRunning = false
	m.cancelTurn = nil
	var cmds []tea.Cmd
	if msg.task != nil {
		var b bytes.Buffer
		printTaskStatus(&b, msg.task)
		cmds = append(cmds, tea.Println(strings.TrimRight(b.String(), "\n")))
	}
	switch {
	case msg.err != nil && errors.Is(msg.err, context.Canceled):
		cmds = append(cmds, tea.Println(noticeStyle.Render("^C interrupted")))
	case msg.err != nil:
		cmds = append(cmds, tea.Println(errorStyle.Render("goal: "+msg.err.Error())))
	case msg.task != nil:
		cmds = append(cmds, tea.Println(noticeStyle.Render("Goal "+msg.task.ShortID()+" finished.")))
	}
	return m, tea.Batch(cmds...)
}

// goalResume resets a task's failed/skipped subtasks to pending and re-runs it.
// Mirrors the reset in `octo goal resume` (task.go) so the two paths agree.
func (m *tuiModel) goalResume(idArg string) (tea.Model, tea.Cmd) {
	store, err := taskgraph.NewStore()
	if err != nil {
		return m, tea.Println(errorStyle.Render("goal: " + err.Error()))
	}
	id, err := store.ResolveID(idArg)
	if err != nil {
		return m, tea.Println(errorStyle.Render("goal: " + err.Error() + "  (try /goal list)"))
	}
	t, err := store.Update(id, resetForResume)
	if err != nil {
		return m, tea.Println(errorStyle.Render("goal: " + err.Error()))
	}
	return m, tea.Batch(
		tea.Println(noticeStyle.Render(fmt.Sprintf("Resuming %s — re-running pending subtasks…", t.ShortID()))),
		m.startGoalRun(t.ID),
	)
}

// teaScrollbackWriter adapts the scheduler's io.Writer progress output into
// scrollback lines: each '\n'-terminated line becomes a noticeMsg. A trailing
// unterminated fragment is held until the next write (the scheduler writes whole
// lines, so nothing is lost in practice).
type teaScrollbackWriter struct {
	prog interface{ Send(tea.Msg) }
	buf  []byte
}

func (w *teaScrollbackWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = append([]byte(nil), w.buf[i+1:]...)
		w.prog.Send(noticeMsg{text: line})
	}
	return len(p), nil
}
