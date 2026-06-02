package main

import (
	"context"
	"errors"
	"testing"
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
	for _, cmd := range []string{"/help", "/skills", "/memory", "/mcp"} {
		m := newTestModel()
		_, cmdFn := m.dispatchSlash(cmd)
		if m.turnRunning {
			t.Errorf("%s should not start a turn", cmd)
		}
		if cmdFn != nil {
			// In inline mode, tea.Println is returned as Cmd — this is expected
		}
		// printlnBuf is staging; content goes to terminal via tea.Println in inline mode
	}
}

func TestTUI_SlashUnknownFallsThroughToTurn(t *testing.T) {
	// Unrecognised /-prefixed input is treated as ordinary user text (paths,
	// regexes, etc.) matching the plain REPL behaviour.
	m := newTestModel()
	_, cmd := m.dispatchSlash("/bogus")
	if !m.turnRunning {
		t.Error("unknown slash should start a turn like normal text")
	}
	if cmd == nil {
		t.Error("expected a start-turn Cmd")
	}
}

func TestTUI_SkillTriggerStartsTurn(t *testing.T) {
	m := newTestModel()
	m.cfg.skillReg = skillRegFor(t, map[string]string{"greet": "---\ndescription: d\n---\nSay hello."})
	if _, _ = m.dispatchSlash("/greet"); !m.turnRunning {
		t.Error("/<skill> should expand to a turn")
	}
}

// ── /conduct routing + state machine ──

func TestTUI_ConductHelpDoesNotStartTurn(t *testing.T) {
	for _, arg := range []string{"", "help"} {
		m := newTestModel()
		_, cmd := m.dispatchConduct(arg)
		if m.turnRunning {
			t.Errorf("/conduct %q should print usage, not start work", arg)
		}
		if cmd != nil {
			t.Errorf("/conduct %q should not return a Cmd (content goes to scrollback)", arg)
		}
	}
}

func TestTUI_ConductPlanStartsWork(t *testing.T) {
	m := newTestModel()
	if _, _ = m.dispatchConduct("migrate the auth layer"); !m.turnRunning {
		t.Error("/conduct <text> should occupy the session while planning")
	}
}

func TestTUI_ConductPlannedErrorReleasesSession(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	_, _ = m.onConductPlanned(conductPlannedMsg{err: errors.New("boom")})
	if m.turnRunning {
		t.Error("a planning error must release the session")
	}
	if m.modal != nil {
		t.Error("no confirm modal on error")
	}
}

func TestTUI_ConductPlannedSuccessOpensConfirmModal(t *testing.T) {
	m := newTestModel()
	m.turnRunning = true
	_, _ = m.onConductPlanned(conductPlannedMsg{
		id:     "20260529-120000-abcd1234",
		report: "Ledger abcd1234 [pending]\nGoal: do the thing",
	})
	if m.modal == nil {
		t.Fatal("a successful plan should open a confirm modal")
	}
	if !m.turnRunning {
		t.Error("session stays occupied until the plan is confirmed or cancelled")
	}
	// Unblock the bridge goroutine waiting on the modal answer.
	m.answerModal(UserResponse{Cancelled: true})
}

func TestTUI_ConductDoneReleasesSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	m := newTestModel()
	m.turnRunning = true
	_, _ = m.onConductDone(conductDoneMsg{id: "t-12345678"})
	if m.turnRunning {
		t.Error("conduct completion must release the session")
	}
}

func TestTUI_ConductDoneInterruptedReleasesSession(t *testing.T) {
	// A Ctrl-C'd run surfaces context.Canceled; it must still release the
	// session (and not render as a hard error — see onConductDone).
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	m := newTestModel()
	m.turnRunning = true
	_, _ = m.onConductDone(conductDoneMsg{err: context.Canceled})
	if m.turnRunning {
		t.Error("interrupted conduct must release the session")
	}
}

func TestTUI_ConductCancelledReleasesSession(t *testing.T) {
	// Declining the confirm modal routes conductCancelledMsg through Update,
	// which must release the session and append a resume hint to scrollback.
	m := newTestModel()
	m.turnRunning = true
	m.Update(conductCancelledMsg{id: "20260529-120000-abcd1234"})
	if m.turnRunning {
		t.Error("cancelling a planned conduct must release the session")
	}
}

func TestTUI_ConductResumeMissingID(t *testing.T) {
	m := newTestModel()
	_, cmd := m.dispatchConduct("resume")
	if m.turnRunning {
		t.Error("/conduct resume with no id should print usage, not start work")
	}
	_ = cmd
}

func TestTUI_ConductResumeUnknownID(t *testing.T) {
	// HOME → temp so the store reads an empty ~/.octo and ResolveID fails
	// cleanly rather than touching the real one.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	m := newTestModel()
	_, cmd := m.dispatchConduct("resume deadbeef")
	if m.turnRunning {
		t.Error("an unresolvable id must not start a run")
	}
	_ = cmd
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
