package tasks

import (
	"strings"
	"testing"
)

func TestStore_CreateAndGet(t *testing.T) {
	s := New()
	id, err := s.Create("Migrate auth middleware", "Replace legacy session check with new module", "Migrating auth middleware")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Errorf("first ID = %d, want 1", id)
	}
	got, ok := s.Get(id)
	if !ok {
		t.Fatal("created task not gettable")
	}
	if got.Subject != "Migrate auth middleware" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.Status != Pending {
		t.Errorf("new task should be Pending, got %q", got.Status)
	}
	if got.Created.IsZero() || got.Updated.IsZero() {
		t.Error("Created / Updated timestamps should be set")
	}
}

func TestStore_CreateRequiresSubject(t *testing.T) {
	s := New()
	if _, err := s.Create("", "", ""); err == nil {
		t.Error("empty subject should error")
	}
	if _, err := s.Create("   ", "", ""); err == nil {
		t.Error("whitespace-only subject should error")
	}
}

func TestStore_MonotonicIDs(t *testing.T) {
	s := New()
	for i := 1; i <= 5; i++ {
		id, _ := s.Create("t", "", "")
		if id != i {
			t.Errorf("Create #%d returned ID %d", i, id)
		}
	}
}

func TestStore_UpdateStatus(t *testing.T) {
	s := New()
	id, _ := s.Create("task", "", "")
	inProg := InProgress
	got, err := s.Update(id, UpdateField{Status: &inProg})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != InProgress {
		t.Errorf("Update Status = %q, want %q", got.Status, InProgress)
	}
}

func TestStore_UpdateRejectsInvalidStatus(t *testing.T) {
	s := New()
	id, _ := s.Create("task", "", "")
	bogus := Status("bogus")
	if _, err := s.Update(id, UpdateField{Status: &bogus}); err == nil {
		t.Error("invalid status should error")
	}
}

func TestStore_UpdateUnknownID(t *testing.T) {
	s := New()
	inProg := InProgress
	if _, err := s.Update(999, UpdateField{Status: &inProg}); err == nil {
		t.Error("unknown ID should error")
	}
}

func TestStore_UpdatePartialFieldsLeavesOthersAlone(t *testing.T) {
	s := New()
	id, _ := s.Create("orig subject", "orig desc", "orig active")

	newSubject := "new subject"
	got, _ := s.Update(id, UpdateField{Subject: &newSubject})
	if got.Subject != "new subject" {
		t.Errorf("Subject not updated: %q", got.Subject)
	}
	if got.Description != "orig desc" || got.ActiveForm != "orig active" {
		t.Errorf("untouched fields wiped: %+v", got)
	}
	if got.Status != Pending {
		t.Errorf("status should be unchanged: %q", got.Status)
	}
}

func TestStore_UpdateSubjectCannotBeEmpty(t *testing.T) {
	s := New()
	id, _ := s.Create("orig", "", "")
	empty := ""
	if _, err := s.Update(id, UpdateField{Subject: &empty}); err == nil {
		t.Error("clearing subject should error")
	}
}

func TestStore_ListSortsByStatusThenID(t *testing.T) {
	s := New()
	id1, _ := s.Create("a", "", "") // pending
	id2, _ := s.Create("b", "", "") // pending → completed
	id3, _ := s.Create("c", "", "") // pending → in_progress
	id4, _ := s.Create("d", "", "") // deleted (hidden)

	completed := Completed
	inProg := InProgress
	deleted := Deleted
	_, _ = s.Update(id2, UpdateField{Status: &completed})
	_, _ = s.Update(id3, UpdateField{Status: &inProg})
	_, _ = s.Update(id4, UpdateField{Status: &deleted})

	got := s.List()
	if len(got) != 3 {
		t.Fatalf("List should hide deleted; got %d entries", len(got))
	}
	// Expected order: in_progress (id3) → pending (id1) → completed (id2)
	if got[0].ID != id3 || got[1].ID != id1 || got[2].ID != id2 {
		t.Errorf("List order = %d,%d,%d, want %d,%d,%d",
			got[0].ID, got[1].ID, got[2].ID, id3, id1, id2)
	}
}

func TestStore_GetDeletedSurfacesIt(t *testing.T) {
	// Deleted tasks are kept (slot not reused) so a stale ID surfaces as
	// "deleted" rather than silently re-binding.
	s := New()
	id, _ := s.Create("t", "", "")
	del := Deleted
	_, _ = s.Update(id, UpdateField{Status: &del})

	got, ok := s.Get(id)
	if !ok {
		t.Fatal("deleted task should still be Get-able")
	}
	if got.Status != Deleted {
		t.Errorf("status = %q, want deleted", got.Status)
	}
}

func TestStore_Summary(t *testing.T) {
	s := New()
	if got := s.Summary(); got != "" {
		t.Errorf("empty store summary = %q, want empty", got)
	}

	id1, _ := s.Create("a", "", "")
	id2, _ := s.Create("b", "", "")
	_, _ = s.Create("c", "", "")
	inProg := InProgress
	done := Completed
	_, _ = s.Update(id1, UpdateField{Status: &inProg})
	_, _ = s.Update(id2, UpdateField{Status: &done})

	got := s.Summary()
	for _, want := range []string{"1 in progress", "1 pending", "1 done"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary missing %q: %q", want, got)
		}
	}
}

func TestStore_SummaryOmitsZeroBuckets(t *testing.T) {
	s := New()
	_, _ = s.Create("only", "", "")
	got := s.Summary()
	if !strings.Contains(got, "1 pending") {
		t.Errorf("Summary = %q", got)
	}
	if strings.Contains(got, "0 ") {
		t.Errorf("Summary should omit zero buckets: %q", got)
	}
}
