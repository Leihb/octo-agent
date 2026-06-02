package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/conductor"
	tea "github.com/charmbracelet/bubbletea"
)

// ── /conduct: unattended long-horizon orchestration inside the TUI ──
//
// Mirrors /goal's plan → confirm → run flow (tuirepl_goal.go), but drives the
// conductor loop (living ledger, verification gate, max-turns continuation)
// instead of the one-shot taskgraph scheduler. The spawner is already active
// for the session (chat.go), so workers run through tools.ActiveSpawner.

type conductPlannedMsg struct {
	id     string
	report string
	err    error
}
type conductRunMsg struct{ id string }
type conductCancelledMsg struct{ id string }
type conductDoneMsg struct {
	id  string
	err error
}

func (m *tuiModel) dispatchConduct(args string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(args)
	if len(fields) == 0 || strings.ToLower(fields[0]) == "help" {
		m.println(conductUsage())
		return m, nil
	}
	switch strings.ToLower(fields[0]) {
	case "list", "ls":
		var b bytes.Buffer
		runConductList(&b, &b)
		m.println(strings.TrimRight(b.String(), "\n"))
		return m, nil
	case "resume":
		if len(fields) < 2 {
			m.println(noticeStyle.Render("Usage: /conduct resume <id>"))
			return m, nil
		}
		return m.conductResume(fields[1])
	default:
		return m, m.startConductPlan(args)
	}
}

func conductUsage() string {
	return noticeStyle.Render(strings.Join([]string{
		"Usage:",
		"  /conduct <text>       Plan a goal as a ledger, confirm, then conduct it to completion",
		"  /conduct list         List conducted goals",
		"  /conduct resume <id>  Resume a stopped / blocked ledger",
	}, "\n"))
}

// startConductPlan seeds a ledger from the planner side-call, then opens the
// confirm modal. Result returns via conductPlannedMsg.
func (m *tuiModel) startConductPlan(goal string) tea.Cmd {
	m.turnRunning = true
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
			prog.Send(conductPlannedMsg{err: err})
			return
		}
		if len(res.Subtasks) == 0 {
			prog.Send(conductPlannedMsg{err: fmt.Errorf("planner returned no units — refine the goal and try again")})
			return
		}
		units := make([]conductor.Unit, 0, len(res.Subtasks))
		for i, ps := range res.Subtasks {
			units = append(units, conductor.Unit{
				ID:          i + 1,
				Description: ps.Description,
				BlockedBy:   ps.BlockedBy,
				Status:      conductor.UnitPending,
			})
		}
		store, err := conductor.NewStore()
		if err != nil {
			prog.Send(conductPlannedMsg{err: err})
			return
		}
		led, err := store.Create(goal, units)
		if err != nil {
			prog.Send(conductPlannedMsg{err: err})
			return
		}
		var b bytes.Buffer
		conductor.Report(&b, led)
		prog.Send(conductPlannedMsg{id: led.ID, report: strings.TrimRight(b.String(), "\n")})
	}()
	m.println(promptStyle.Render("> ") + "/conduct " + goal)
	m.println(noticeStyle.Render("Planning…"))
	return tickCmd()
}

func (m *tuiModel) onConductPlanned(msg conductPlannedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.turnRunning = false
		m.cancelTurn = nil
		m.println(errorStyle.Render("conduct: " + msg.err.Error()))
		return m, nil
	}
	m.println(msg.report)

	ch := make(chan UserResponse, 1)
	short := msg.id
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	m.openModal(askMsg{
		prompt: UserPrompt{
			Kind:     KindQuestion,
			Header:   "conduct",
			Question: fmt.Sprintf("Conduct this plan to completion (%s)?", short),
			Options:  []string{"Run it", "Cancel"},
		},
		resp: ch,
	})
	id := msg.id
	prog := m.sink.prog
	go func() {
		r := <-ch
		if r.Cancelled || len(r.Choices) == 0 || r.Choices[0] != "Run it" {
			prog.Send(conductCancelledMsg{id: id})
			return
		}
		prog.Send(conductRunMsg{id: id})
	}()
	return m, nil
}

// startConductRun drives the conductor loop in the background, streaming
// progress into the scrollback. Uses session defaults: sequential, Go
// verification gate, no re-planning (interactive runs favour visibility).
func (m *tuiModel) startConductRun(id string) tea.Cmd {
	m.turnRunning = true
	m.turnStart = time.Now()
	m.spinnerFrame = 0
	m.running = nil
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	prog := m.sink.prog
	go func() {
		defer cancel()
		store, err := conductor.NewStore()
		if err != nil {
			prog.Send(conductDoneMsg{id: id, err: err})
			return
		}
		w := &teaScrollbackWriter{prog: prog}
		c := conductor.New(store, &spawnerWorker{}, conductor.NewGoVerifier(), w, conductor.Config{})
		runErr := c.Run(ctx, id)
		prog.Send(conductDoneMsg{id: id, err: runErr})
	}()
	m.println(noticeStyle.Render("Conducting… (Ctrl-C to stop; resume later)"))
	return tickCmd()
}

func (m *tuiModel) onConductDone(msg conductDoneMsg) (tea.Model, tea.Cmd) {
	m.turnRunning = false
	m.cancelTurn = nil
	var b strings.Builder
	store, serr := conductor.NewStore()
	if serr == nil {
		if led, gerr := store.Get(msg.id); gerr == nil {
			var rb bytes.Buffer
			conductor.Report(&rb, led)
			b.WriteString(strings.TrimRight(rb.String(), "\n"))
		}
	}
	switch {
	case msg.err != nil && errors.Is(msg.err, context.Canceled):
		// Interrupted: show the report without an extra error notice.
	case msg.err != nil:
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(errorStyle.Render("conduct: " + msg.err.Error()))
	default:
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(noticeStyle.Render("Goal finished."))
	}
	if b.Len() > 0 {
		m.println(b.String())
	}
	return m, nil
}

// teaScrollbackWriter adapts an io.Writer (the conductor's progress stream)
// into TUI scrollback lines: each '\n'-terminated line becomes a noticeMsg; a
// trailing unterminated fragment is held until the next write.
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

func (m *tuiModel) conductResume(idArg string) (tea.Model, tea.Cmd) {
	store, err := conductor.NewStore()
	if err != nil {
		m.println(errorStyle.Render("conduct: " + err.Error()))
		return m, nil
	}
	id, err := store.ResolveID(idArg)
	if err != nil {
		m.println(errorStyle.Render("conduct: " + err.Error() + "  (try /conduct list)"))
		return m, nil
	}
	_, err = store.Update(id, func(l *conductor.Ledger) error {
		for i := range l.Units {
			if l.Units[i].Status == conductor.UnitBlocked {
				l.Units[i].Status = conductor.UnitPending
				l.Units[i].Attempts = 0
			}
		}
		return nil
	})
	if err != nil {
		m.println(errorStyle.Render("conduct: " + err.Error()))
		return m, nil
	}
	m.println(noticeStyle.Render("Resuming — re-running pending units…"))
	return m, m.startConductRun(id)
}
