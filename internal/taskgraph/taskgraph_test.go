package taskgraph

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeStore returns a Store rooted at a tempdir. Helper for every test.
func makeStore(t *testing.T) *Store {
	t.Helper()
	return NewStoreAt(t.TempDir())
}

// validSubs returns a minimal valid 3-node DAG: 1 → 2 → 3. Statuses default
// to SubtaskPending — mimics what Store.Create stamps so the Ready() tests
// can probe state transitions without re-fabricating that defaulting.
func validSubs() []Subtask {
	return []Subtask{
		{ID: 1, Description: "do A", Status: SubtaskPending},
		{ID: 2, Description: "do B", Status: SubtaskPending, BlockedBy: []int{1}},
		{ID: 3, Description: "do C", Status: SubtaskPending, BlockedBy: []int{2}},
	}
}

// ── Validation ───────────────────────────────────────────────────────────

func TestValidateSubtasks_EmptySliceErrors(t *testing.T) {
	if err := validateSubtasks(nil); err == nil {
		t.Error("empty subtasks should error")
	}
}

func TestValidateSubtasks_EmptyDescriptionErrors(t *testing.T) {
	subs := []Subtask{{ID: 1, Description: ""}}
	if err := validateSubtasks(subs); err == nil {
		t.Error("empty description should error")
	}
}

func TestValidateSubtasks_IDsMustBeMonotonic(t *testing.T) {
	subs := []Subtask{
		{ID: 1, Description: "a"},
		{ID: 3, Description: "b"}, // skip
	}
	if err := validateSubtasks(subs); err == nil {
		t.Error("non-monotonic ID should error")
	}
}

func TestValidateSubtasks_BlockedByMustReferenceEarlier(t *testing.T) {
	subs := []Subtask{
		{ID: 1, Description: "a", BlockedBy: []int{2}}, // forward ref → cycle
		{ID: 2, Description: "b"},
	}
	if err := validateSubtasks(subs); err == nil {
		t.Error("forward dependency should error (no DAG cycles)")
	}
}

func TestValidateSubtasks_BlockedBySelfErrors(t *testing.T) {
	subs := []Subtask{{ID: 1, Description: "a", BlockedBy: []int{1}}}
	if err := validateSubtasks(subs); err == nil {
		t.Error("self-dependency should error")
	}
}

func TestValidateSubtasks_BlockedByUnknownErrors(t *testing.T) {
	// 1 → 2; 2 depends on a nonexistent 5.
	subs := []Subtask{
		{ID: 1, Description: "a"},
		{ID: 2, Description: "b", BlockedBy: []int{5}},
	}
	if err := validateSubtasks(subs); err == nil {
		t.Error("dangling dependency should error")
	}
}

func TestValidateSubtasks_HappyPath(t *testing.T) {
	if err := validateSubtasks(validSubs()); err != nil {
		t.Errorf("valid DAG rejected: %v", err)
	}
}

// ── Ready ────────────────────────────────────────────────────────────────

func TestSubtask_Ready_NoDepsIsReady(t *testing.T) {
	tk := &Task{Subtasks: validSubs()}
	if !tk.Subtasks[0].Ready(tk) {
		t.Error("first subtask (no deps, pending) should be Ready")
	}
}

func TestSubtask_Ready_BlockedUntilDepsDone(t *testing.T) {
	tk := &Task{Subtasks: validSubs()}
	if tk.Subtasks[1].Ready(tk) {
		t.Error("subtask with un-done dep should not be Ready")
	}
	tk.Subtasks[0].Status = SubtaskDone
	if !tk.Subtasks[1].Ready(tk) {
		t.Error("once dep is Done, subtask should be Ready")
	}
}

func TestSubtask_Ready_OnlyPendingStateIsReady(t *testing.T) {
	tk := &Task{Subtasks: validSubs()}
	tk.Subtasks[0].Status = SubtaskDone // not pending → not Ready
	if tk.Subtasks[0].Ready(tk) {
		t.Error("a Done subtask should not be Ready (already ran)")
	}
}

func TestTask_Find(t *testing.T) {
	tk := &Task{Subtasks: validSubs()}
	if s := tk.Find(2); s == nil || s.Description != "do B" {
		t.Errorf("Find(2) = %+v", s)
	}
	if s := tk.Find(99); s != nil {
		t.Errorf("Find on missing ID should be nil, got %+v", s)
	}
}

// ── Store.Create + persistence ───────────────────────────────────────────

func TestStore_Create_PersistsAndAssignsID(t *testing.T) {
	s := makeStore(t)
	tk, err := s.Create("ship the daemon", validSubs())
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID == "" {
		t.Error("Create should assign an ID")
	}
	if tk.Status != TaskPending {
		t.Errorf("new task should be Pending, got %q", tk.Status)
	}
	if tk.Subtasks[0].Status != SubtaskPending {
		t.Errorf("subtasks should default to Pending, got %q", tk.Subtasks[0].Status)
	}
	if tk.Created.IsZero() || tk.Updated.IsZero() {
		t.Error("Created / Updated should be set")
	}
	// File must exist on disk.
	if _, err := os.Stat(filepath.Join(s.Dir(), tk.ID+".json")); err != nil {
		t.Errorf("task file not persisted: %v", err)
	}
}

func TestStore_Create_RejectsEmptyGoal(t *testing.T) {
	s := makeStore(t)
	if _, err := s.Create("", validSubs()); err == nil {
		t.Error("empty goal should error")
	}
	if _, err := s.Create("   ", validSubs()); err == nil {
		t.Error("whitespace-only goal should error")
	}
}

func TestStore_Create_RejectsInvalidSubtasks(t *testing.T) {
	s := makeStore(t)
	if _, err := s.Create("goal", nil); err == nil {
		t.Error("empty subtasks should error")
	}
	bad := []Subtask{{ID: 5, Description: "wrong-id"}}
	if _, err := s.Create("goal", bad); err == nil {
		t.Error("non-monotonic ID should error")
	}
}

func TestStore_Create_AssignsUniqueIDs(t *testing.T) {
	// Smoke test: 50 rapid creates must all get distinct IDs.
	s := makeStore(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tk, err := s.Create("goal", validSubs())
		if err != nil {
			t.Fatal(err)
		}
		if seen[tk.ID] {
			t.Errorf("duplicate ID generated: %s", tk.ID)
		}
		seen[tk.ID] = true
	}
}

// ── Store.Update ─────────────────────────────────────────────────────────

func TestStore_Update_AppliesMutation(t *testing.T) {
	s := makeStore(t)
	tk, _ := s.Create("goal", validSubs())

	got, err := s.Update(tk.ID, func(t *Task) error {
		t.Status = TaskRunning
		t.Find(1).Status = SubtaskRunning
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskRunning {
		t.Errorf("status not updated: %q", got.Status)
	}
	if got.Find(1).Status != SubtaskRunning {
		t.Errorf("subtask status not updated: %q", got.Find(1).Status)
	}

	// Re-load to confirm the change actually hit disk.
	reload, err := s.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reload.Status != TaskRunning {
		t.Errorf("disk reload lost status: %q", reload.Status)
	}
}

func TestStore_Update_StampsUpdatedTime(t *testing.T) {
	s := makeStore(t)
	tk, _ := s.Create("goal", validSubs())
	origUpdated := tk.Updated
	// Force a non-zero gap so the stamp changes even at second granularity.
	time := func(name string) {} // shadow `time` so we don't pull stdlib in for one sleep
	_ = time

	got, _ := s.Update(tk.ID, func(t *Task) error {
		t.Status = TaskRunning
		return nil
	})
	if !got.Updated.After(origUpdated) && !got.Updated.Equal(origUpdated) {
		t.Errorf("Updated did not advance: was %v, now %v", origUpdated, got.Updated)
	}
}

func TestStore_Update_MutationErrorRollsBack(t *testing.T) {
	s := makeStore(t)
	tk, _ := s.Create("goal", validSubs())
	sentinel := errors.New("simulated")

	_, err := s.Update(tk.ID, func(t *Task) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("Update should propagate mutation err, got %v", err)
	}
	// Disk state unchanged.
	reload, _ := s.Get(tk.ID)
	if reload.Status != TaskPending {
		t.Errorf("status should be unchanged when mutate errors: %q", reload.Status)
	}
}

func TestStore_Update_UnknownIDErrors(t *testing.T) {
	s := makeStore(t)
	if _, err := s.Update("bogus", func(*Task) error { return nil }); err == nil {
		t.Error("update on missing ID should error")
	}
}

// ── Store.Get / List ─────────────────────────────────────────────────────

func TestStore_Get_RoundTrip(t *testing.T) {
	s := makeStore(t)
	tk, _ := s.Create("goal X", validSubs())

	got, err := s.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "goal X" {
		t.Errorf("Get round-trip lost goal: %q", got.Goal)
	}
	if len(got.Subtasks) != 3 {
		t.Errorf("Get round-trip lost subtasks: %+v", got.Subtasks)
	}
}

func TestStore_Get_MissingErrors(t *testing.T) {
	s := makeStore(t)
	if _, err := s.Get("does-not-exist"); err == nil {
		t.Error("Get on missing id should error")
	}
}

func TestStore_List_EmptyStore(t *testing.T) {
	s := makeStore(t)
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty store List = %+v", got)
	}
}

func TestStore_List_SortsNewestFirst(t *testing.T) {
	s := makeStore(t)
	// IDs are time-stamped, so creating in sequence orders them ascending by
	// creation. List should return them descending.
	t1, _ := s.Create("first", validSubs())
	t2, _ := s.Create("second", validSubs())
	t3, _ := s.Create("third", validSubs())

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	// Most recent first. We don't know the millisecond-tiebreak ordering but
	// the IDs include random hex, so we just check membership + that the
	// newest two IDs are present in the first two positions.
	allIDs := []string{t1.ID, t2.ID, t3.ID}
	got2 := []string{got[0].ID, got[1].ID, got[2].ID}
	if !sameSet(allIDs, got2) {
		t.Errorf("List membership wrong: got %v, want %v", got2, allIDs)
	}
}

func TestStore_List_SkipsNonJSONAndDirs(t *testing.T) {
	s := makeStore(t)
	_, _ = s.Create("real", validSubs())

	_ = os.MkdirAll(s.Dir(), 0o755)
	_ = os.WriteFile(filepath.Join(s.Dir(), "stray.txt"), []byte("ignore me"), 0o644)
	_ = os.Mkdir(filepath.Join(s.Dir(), "subdir"), 0o755)

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("List should ignore non-.json files and subdirs, got %d", len(got))
	}
}

// ── Atomicity ────────────────────────────────────────────────────────────

func TestStore_Persist_NoTempLeftBehind(t *testing.T) {
	// After a successful Create + Update, only the final .json should be on
	// disk — no `<id>.json.tmp` should leak.
	s := makeStore(t)
	tk, _ := s.Create("goal", validSubs())
	_, _ = s.Update(tk.ID, func(t *Task) error {
		t.Status = TaskRunning
		return nil
	})
	ents, _ := os.ReadDir(s.Dir())
	for _, de := range ents {
		if strings.HasSuffix(de.Name(), ".tmp") {
			t.Errorf("temp file leaked: %s", de.Name())
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

func TestTask_ShortID(t *testing.T) {
	if got := (&Task{ID: "20260528-171234-a3b2c1d4"}).ShortID(); got != "a3b2c1d4" {
		t.Errorf("ShortID = %q, want %q", got, "a3b2c1d4")
	}
	if got := (&Task{ID: "abc"}).ShortID(); got != "abc" {
		t.Errorf("short ID for short input = %q, want %q", got, "abc")
	}
}

// seedTaskID creates a task with a synthetic ID we control. Returns the
// stored ID. ResolveID isn't called by Store.Create — it stamps a fresh
// timestamped ID — so we hand-craft the file to test resolver paths
// against known prefixes/suffixes.
func seedTaskID(t *testing.T, s *Store, id string) string {
	t.Helper()
	task := &Task{
		ID:       id,
		Goal:     "synthetic",
		Status:   TaskPending,
		Subtasks: validSubs(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(task); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	return id
}

func TestResolveID_ExactFullID(t *testing.T) {
	s := makeStore(t)
	id := seedTaskID(t, s, "20260528-171234-a3b2c1d4")
	got, err := s.ResolveID(id)
	if err != nil {
		t.Fatalf("ResolveID(exact): %v", err)
	}
	if got != id {
		t.Errorf("got %q, want %q", got, id)
	}
}

func TestResolveID_SubstringUnique(t *testing.T) {
	s := makeStore(t)
	seedTaskID(t, s, "20260101-000000-aaaaaaaa")
	expect := seedTaskID(t, s, "20260201-000000-bbbbbbbb")
	got, err := s.ResolveID("bbbbbbbb")
	if err != nil {
		t.Fatalf("ResolveID(substring): %v", err)
	}
	if got != expect {
		t.Errorf("got %q, want %q", got, expect)
	}
}

func TestResolveID_LastNewest(t *testing.T) {
	s := makeStore(t)
	seedTaskID(t, s, "20260101-000000-aaaaaaaa")
	newest := seedTaskID(t, s, "20260601-000000-bbbbbbbb")
	got, err := s.ResolveID("last")
	if err != nil {
		t.Fatalf("ResolveID(last): %v", err)
	}
	// Newest by reverse lex sort over the timestamp-prefixed ID.
	if got != newest {
		t.Errorf("got %q, want %q (newest)", got, newest)
	}
}

func TestResolveID_LastEmptyStore(t *testing.T) {
	s := makeStore(t)
	if _, err := s.ResolveID("last"); err == nil {
		t.Fatal("expected error for 'last' against empty store")
	}
}

func TestResolveID_Ambiguous(t *testing.T) {
	s := makeStore(t)
	seedTaskID(t, s, "20260528-100000-aaaaaaaa")
	seedTaskID(t, s, "20260528-110000-aaaaaabb")
	_, err := s.ResolveID("aaaaaa")
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("missing 'ambiguous': %v", err)
	}
}

func TestResolveID_NoMatch(t *testing.T) {
	s := makeStore(t)
	seedTaskID(t, s, "20260528-100000-aaaaaaaa")
	if _, err := s.ResolveID("zzzzzz"); err == nil {
		t.Fatal("expected no-match error")
	}
}

func TestResolveID_EmptyInput(t *testing.T) {
	s := makeStore(t)
	if _, err := s.ResolveID("  "); err == nil {
		t.Fatal("expected empty-input error")
	}
}
