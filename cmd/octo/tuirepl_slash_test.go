package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Leihb/octo-agent/internal/taskgraph"
)

// ── dispatchSlash routing ──

func TestTUI_SlashExitQuits(t *testing.T) {
	m := newTestModel()
	if _, _ = m.dispatchSlash("/exit"); !m.quit {
		t.Error("/exit should set quit")
	}
	m2 := newTestModel()
	if _, _ = m2.dispatchSlash("/quit"); !m2.quit {
		t.Error("/quit should set quit")
	}
}

func TestTUI_SlashInfoCommandsDontStartTurn(t *testing.T) {
	// Info commands render synchronously and must not occupy the session.
	for _, cmd := range []string{"/help", "/cost", "/skills", "/memory", "/mcp"} {
		m := newTestModel()
		_, cmdFn := m.dispatchSlash(cmd)
		if m.turnRunning {
			t.Errorf("%s should not start a turn", cmd)
		}
		if cmdFn == nil {
			t.Errorf("%s should return a render Cmd", cmd)
		}
	}
}

func TestTUI_SlashUnknownDoesNotStartTurn(t *testing.T) {
	m := newTestModel()
	_, cmd := m.dispatchSlash("/bogus")
	if m.turnRunning {
		t.Error("unknown command must not start a turn")
	}
	if cmd == nil {
		t.Error("unknown command should return a notice Cmd")
	}
}

func TestTUI_SkillTriggerStartsTurn(t *testing.T) {
	m := newTestModel()
	m.cfg.skillReg = skillRegFor(t, map[string]string{"greet": "---\ndescription: d\n---\nSay hello."})
	if _, _ = m.dispatchSlash("/greet"); !m.turnRunning {
		t.Error("/<skill> should expand to a turn")
	}
}

// ── /goal routing + state machine ──

func TestTUI_GoalHelpDoesNotStartTurn(t *testing.T) {
	for _, arg := range []string{"", "help"} {
		m := newTestModel()
		_, cmd := m.dispatchGoal(arg)
		if m.turnRunning {
			t.Errorf("/goal %q should print usage, not start work", arg)
		}
		if cmd == nil {
			t.Errorf("/goal %q should return a usage Cmd", arg)
		}
	}
}

func TestTUI_GoalPlanStartsWork(t *testing.T) {
	m := newTestModel()
	if _, _ = m.dispatchGoal("migrate the auth layer"); !m.turnRunning {
		t.Error("/goal <text> should occupy the session while planning")
	}
}

func TestTUI_GoalPlannedErrorReleasesSession(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	_, _ = m.onGoalPlanned(goalPlannedMsg{err: errors.New("boom")})
	if m.turnRunning {
		t.Error("a planning error must release the session")
	}
	if m.modal != nil {
		t.Error("no confirm modal on error")
	}
}

func TestTUI_GoalPlannedSuccessOpensConfirmModal(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	task := &taskgraph.Task{
		ID:       "20260529-120000-abcd1234",
		Goal:     "do the thing",
		Status:   taskgraph.TaskPending,
		Subtasks: []taskgraph.Subtask{{ID: 1, Description: "step one", Status: taskgraph.SubtaskPending}},
	}
	_, _ = m.onGoalPlanned(goalPlannedMsg{task: task})
	if m.modal == nil {
		t.Fatal("a successful plan should open a confirm modal")
	}
	if !m.turnRunning {
		t.Error("session stays occupied until the plan is confirmed or cancelled")
	}
	// Unblock the bridge goroutine waiting on the modal answer.
	m.answerModal(UserResponse{Cancelled: true})
}

func TestTUI_GoalDoneReleasesSession(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	task := &taskgraph.Task{ID: "t-12345678", Goal: "g", Status: taskgraph.TaskDone}
	_, _ = m.onGoalDone(goalDoneMsg{task: task})
	if m.turnRunning {
		t.Error("goal completion must release the session")
	}
}

func TestTUI_GoalDoneInterruptedReleasesSession(t *testing.T) {
	// A Ctrl-C'd run surfaces context.Canceled; it must still release the
	// session (and not render as a hard error — see onGoalDone).
	m := newTestModel()
	m.turnRunning = true
	_, _ = m.onGoalDone(goalDoneMsg{err: context.Canceled})
	if m.turnRunning {
		t.Error("interrupted goal must release the session")
	}
}

func TestTUI_GoalCancelledReleasesSession(t *testing.T) {
	// Declining the confirm modal routes goalCancelledMsg through Update,
	// which must release the session and print a resume hint.
	m := newTestModel()
	m.turnRunning = true
	task := &taskgraph.Task{ID: "20260529-120000-abcd1234", Goal: "g", Status: taskgraph.TaskPending}
	_, cmd := m.Update(goalCancelledMsg{task: task})
	if m.turnRunning {
		t.Error("cancelling a planned goal must release the session")
	}
	if cmd == nil {
		t.Error("expected a resume-hint notice Cmd")
	}
}

func TestTUI_GoalResumeMissingID(t *testing.T) {
	m := newTestModel()
	_, cmd := m.dispatchGoal("resume")
	if m.turnRunning {
		t.Error("/goal resume with no id should print usage, not start work")
	}
	if cmd == nil {
		t.Error("expected a usage Cmd")
	}
}

func TestTUI_GoalResumeUnknownID(t *testing.T) {
	// HOME → temp so the store reads an empty ~/.octo and ResolveID fails
	// cleanly rather than touching the real one.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	m := newTestModel()
	_, cmd := m.dispatchGoal("resume deadbeef")
	if m.turnRunning {
		t.Error("an unresolvable id must not start a run")
	}
	if cmd == nil {
		t.Error("expected an error notice Cmd")
	}
}

func TestTeaScrollbackWriter_SplitsLines(t *testing.T) {
	fp := &fakeProg{}
	w := &teaScrollbackWriter{prog: fp}
	if _, err := w.Write([]byte("line one\nline two\npartial")); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, msg := range fp.msgs {
		if n, ok := msg.(noticeMsg); ok {
			lines = append(lines, n.text)
		}
	}
	if len(lines) != 2 || lines[0] != "line one" || lines[1] != "line two" {
		t.Errorf("expected two complete lines, got %v", lines)
	}
	// The unterminated fragment is held, not emitted.
	if string(w.buf) != "partial" {
		t.Errorf("trailing fragment = %q, want %q", string(w.buf), "partial")
	}
}
