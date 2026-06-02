package conductor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateUnits(t *testing.T) {
	tests := []struct {
		name    string
		units   []Unit
		wantErr bool
	}{
		{"empty", nil, true},
		{"ok linear", []Unit{{ID: 1, Description: "a"}, {ID: 2, Description: "b", BlockedBy: []int{1}}}, false},
		{"ok forward edge", []Unit{{ID: 1, Description: "a", BlockedBy: []int{2}}, {ID: 2, Description: "b"}}, false},
		{"non-contiguous ids ok", []Unit{{ID: 5, Description: "a"}, {ID: 9, Description: "b", BlockedBy: []int{5}}}, false},
		{"empty desc", []Unit{{ID: 1, Description: "  "}}, true},
		{"dup id", []Unit{{ID: 1, Description: "a"}, {ID: 1, Description: "b"}}, true},
		{"dangling dep", []Unit{{ID: 1, Description: "a", BlockedBy: []int{99}}}, true},
		{"self dep", []Unit{{ID: 1, Description: "a", BlockedBy: []int{1}}}, true},
		{"cycle", []Unit{{ID: 1, Description: "a", BlockedBy: []int{2}}, {ID: 2, Description: "b", BlockedBy: []int{1}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUnits(tt.units)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateUnits err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestReadyAndUpstream(t *testing.T) {
	l := &Ledger{Units: []Unit{
		{ID: 1, Description: "a", Status: UnitDone, ResultSummary: "made A"},
		{ID: 2, Description: "b", Status: UnitPending, BlockedBy: []int{1}},
		{ID: 3, Description: "c", Status: UnitPending, BlockedBy: []int{2}},
	}}
	ready := l.ReadyUnits()
	if len(ready) != 1 || ready[0] != 2 {
		t.Fatalf("ReadyUnits = %v, want [2]", ready)
	}
	ups := l.UpstreamResults(2)
	if len(ups) != 1 || ups[0].ID != 1 || ups[0].Summary != "made A" {
		t.Fatalf("UpstreamResults(2) = %+v, want [{1 ... made A}]", ups)
	}
	if l.AllDone() {
		t.Fatal("AllDone = true, want false")
	}
}

func TestNextUnitID(t *testing.T) {
	l := &Ledger{Units: []Unit{{ID: 1}, {ID: 4}, {ID: 2}}}
	if got := l.nextUnitID(); got != 5 {
		t.Fatalf("nextUnitID = %d, want 5", got)
	}
}

func TestStoreCreateGetUpdate(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	l, err := s.Create("port it", []Unit{{ID: 1, Description: "step one"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if l.Status != LedgerPending {
		t.Fatalf("status = %s, want pending", l.Status)
	}
	// Conventions doc seeded.
	if _, err := os.Stat(s.ConventionsPath(l.ID)); err != nil {
		t.Fatalf("conventions doc not seeded: %v", err)
	}
	// Update persists.
	_, err = s.Update(l.ID, func(l *Ledger) error {
		l.Units[0].Status = UnitDone
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(l.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Units[0].Status != UnitDone {
		t.Fatalf("persisted status = %s, want done", got.Units[0].Status)
	}
}

func TestStoreListAndResolveID(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	a, _ := s.Create("alpha", []Unit{{ID: 1, Description: "x"}})
	b, _ := s.Create("beta", []Unit{{ID: 1, Description: "y"}})

	leds, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(leds) != 2 {
		t.Fatalf("List len = %d, want 2", len(leds))
	}

	// "last" resolves to the newest (ids are timestamp-prefixed; both created
	// the same second may tie — accept either of the two known ids).
	last, err := s.ResolveID("last")
	if err != nil {
		t.Fatalf("ResolveID(last): %v", err)
	}
	if last != a.ID && last != b.ID {
		t.Fatalf("ResolveID(last) = %q, want one of the created ids", last)
	}
	// Exact + substring.
	if got, err := s.ResolveID(a.ID); err != nil || got != a.ID {
		t.Fatalf("ResolveID(full) = %q, %v", got, err)
	}
	short := a.ID[len(a.ID)-8:]
	if got, err := s.ResolveID(short); err != nil || got != a.ID {
		t.Fatalf("ResolveID(short) = %q, %v", got, err)
	}
}

func TestStoreJournalAndConventions(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	l, _ := s.Create("g", []Unit{{ID: 1, Description: "x"}})
	s.AppendJournal(l.ID, "hello")
	s.AppendJournal(l.ID, "world")
	b, err := os.ReadFile(s.JournalPath(l.ID))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if !strings.Contains(string(b), "hello") || !strings.Contains(string(b), "world") {
		t.Fatalf("journal missing entries: %s", b)
	}
	// Worker-style append to conventions, then ReadConventions sees it.
	cp := s.ConventionsPath(l.ID)
	f, _ := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\nDECISION: use package foo\n")
	f.Close()
	if !strings.Contains(s.ReadConventions(l.ID), "use package foo") {
		t.Fatalf("ReadConventions missing appended decision")
	}
}

func TestStorePersistAtomicLayout(t *testing.T) {
	base := t.TempDir()
	s := NewStoreAt(base)
	l, _ := s.Create("g", []Unit{{ID: 1, Description: "x"}})
	// ledger.json must live under <base>/<id>/.
	if _, err := os.Stat(filepath.Join(base, l.ID, "ledger.json")); err != nil {
		t.Fatalf("ledger.json not at expected path: %v", err)
	}
	// No leftover tmp file.
	if _, err := os.Stat(filepath.Join(base, l.ID, "ledger.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("leftover tmp file present")
	}
}

func TestCmdVerifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell builtins (true/false); shell differs on Windows")
	}
	ctx := context.Background()

	// Empty gate → green (self-verify mode).
	if v, err := (&CmdVerifier{}).Verify(ctx, VerifyTarget{}); err != nil || !v.Green {
		t.Fatalf("empty gate: verdict=%+v err=%v, want green", v, err)
	}

	// All pass.
	green := &CmdVerifier{Commands: []string{"true", "true"}}
	if v, err := green.Verify(ctx, VerifyTarget{}); err != nil || !v.Green {
		t.Fatalf("true gate: verdict=%+v err=%v, want green", v, err)
	}

	// First failure short-circuits and names the command.
	red := &CmdVerifier{Commands: []string{"echo boom && false", "true"}}
	v, err := red.Verify(ctx, VerifyTarget{})
	if err != nil {
		t.Fatalf("red gate err = %v", err)
	}
	if v.Green {
		t.Fatal("red gate verdict green, want red")
	}
	if !strings.Contains(v.Summary, "boom") {
		t.Errorf("summary missing failing output: %q", v.Summary)
	}
}
