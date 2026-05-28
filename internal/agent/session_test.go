package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setTempHome redirects both HOME (Unix) and USERPROFILE (Windows) to tmp so
// sessionsDir() stays inside the test sandbox on all platforms.
func setTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp) // Windows: os.UserHomeDir() reads USERPROFILE
	return tmp
}

func TestNewSession(t *testing.T) {
	s := NewSession("claude-haiku-4-5-20251001", "be helpful")
	if s.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if s.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q, want %q", s.Model, "claude-haiku-4-5-20251001")
	}
	if s.System != "be helpful" {
		t.Errorf("system = %q, want %q", s.System, "be helpful")
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt must be set")
	}
}

func TestSessionIDFormat(t *testing.T) {
	s := NewSession("m", "")
	// YYYYMMDD-HHMMSS-xxxxxxxx — 24 chars (15 timestamp + '-' + 8 hex suffix).
	if len(s.ID) != 24 {
		t.Errorf("ID len = %d, want 24 (got %q)", len(s.ID), s.ID)
	}
	if s.ID[8] != '-' || s.ID[15] != '-' {
		t.Errorf("ID separators wrong: %q", s.ID)
	}
}

func TestSessionID_NoCollisionSameSecond(t *testing.T) {
	// Two sessions created back-to-back (same second) must get distinct IDs,
	// so their save files don't clobber each other.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewSession("m", "").ID
		if seen[id] {
			t.Fatalf("duplicate session ID within the same second: %q", id)
		}
		seen[id] = true
	}
}

func TestTurnCount(t *testing.T) {
	s := NewSession("m", "")
	if s.TurnCount() != 0 {
		t.Errorf("TurnCount = %d, want 0", s.TurnCount())
	}
	s.Messages = []Message{
		NewUserMessage("hi"),
		NewAssistantMessage("hello"),
		NewUserMessage("bye"),
		NewAssistantMessage("goodbye"),
	}
	if s.TurnCount() != 2 {
		t.Errorf("TurnCount = %d, want 2", s.TurnCount())
	}
}

func TestSaveAndLoad(t *testing.T) {
	// Point sessions dir at a temp directory so we don't pollute ~/.octo.
	setTempHome(t)

	s := NewSession("model-x", "sys")
	s.Messages = []Message{
		NewUserMessage("ping"),
		NewAssistantMessage("pong"),
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadSession(s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	if got.ID != s.ID {
		t.Errorf("ID: got %q, want %q", got.ID, s.ID)
	}
	if got.Model != "model-x" {
		t.Errorf("Model: got %q, want %q", got.Model, "model-x")
	}
	if len(got.Messages) != 2 {
		t.Errorf("Messages len: got %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Content != "ping" {
		t.Errorf("Messages[0].Content: got %q, want %q", got.Messages[0].Content, "ping")
	}
}

func TestSave_AppendsIncrementally(t *testing.T) {
	setTempHome(t)

	s := NewSession("m", "sys")
	s.Messages = []Message{NewUserMessage("u1"), NewAssistantMessage("a1")}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Add a second turn and save again — should append, not rewrite.
	s.Messages = append(s.Messages, NewUserMessage("u2"), NewAssistantMessage("a2"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("loaded %d messages, want 4", len(got.Messages))
	}
	for i, want := range []string{"u1", "a1", "u2", "a2"} {
		if got.Messages[i].Content != want {
			t.Errorf("Messages[%d] = %q, want %q", i, got.Messages[i].Content, want)
		}
	}
}

func TestSave_RewritesWhenHistoryShrinks(t *testing.T) {
	// Simulates compaction: history is rewritten to fewer messages. Save must
	// rewrite the file from scratch, not append, so the load reflects the
	// shrunk history.
	setTempHome(t)

	s := NewSession("m", "")
	for i := 0; i < 6; i++ {
		s.Messages = append(s.Messages, NewUserMessage("x"))
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// Compaction replaces the 6 with a summary + 1 kept.
	s.Messages = []Message{NewUserMessage("[summary]"), NewAssistantMessage("kept")}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("loaded %d messages, want 2 (file should have been rewritten)", len(got.Messages))
	}
	if got.Messages[0].Content != "[summary]" {
		t.Errorf("Messages[0] = %q, want '[summary]'", got.Messages[0].Content)
	}
}

func TestSavePath_IsJSONL(t *testing.T) {
	setTempHome(t)
	s := NewSession("m", "")
	path, err := s.SavePath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".jsonl" {
		t.Errorf("SavePath ext = %q, want .jsonl", filepath.Ext(path))
	}
}

func TestLoadSession_NotFound(t *testing.T) {
	setTempHome(t)

	_, err := LoadSession("19991231-235959")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestLoadSession_StripDotJSON(t *testing.T) {
	setTempHome(t)

	s := NewSession("m", "")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadSession(s.ID + ".json")
	if err != nil {
		t.Fatalf("LoadSession with .json suffix: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("ID mismatch: got %q, want %q", got.ID, s.ID)
	}
}

func TestListSessions(t *testing.T) {
	setTempHome(t)

	// Save 3 sessions, then stamp distinct file mtimes so ordering (newest
	// first, by mtime) is deterministic.
	ids := make([]string, 3)
	base := time.Now()
	for i := 0; i < 3; i++ {
		s := &Session{ID: "20260101-00000" + string(rune('0'+i)), CreatedAt: base, Model: "m"}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		ids[i] = s.ID
		path, _ := s.SavePath()
		mod := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	sessions, err := ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}
	// Newest mtime first → the last-stamped (index 2) leads.
	if sessions[0].ID != ids[2] {
		t.Errorf("sessions[0].ID = %q, want %q (newest mtime)", sessions[0].ID, ids[2])
	}
}

func TestListSessions_Limit(t *testing.T) {
	setTempHome(t)

	for i := 0; i < 5; i++ {
		s := &Session{ID: "20260101-00000" + string(rune('0'+i)), CreatedAt: time.Now(), Model: "m"}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}

	sessions, err := ListSessions(3)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("got %d sessions with limit 3", len(sessions))
	}
}

func TestSyncFrom(t *testing.T) {
	s := NewSession("m", "")
	h := NewHistory()
	h.Append(NewUserMessage("hello"))
	h.Append(NewAssistantMessage("world"))

	s.SyncFrom(h)

	if len(s.Messages) != 2 {
		t.Errorf("Messages len = %d, want 2", len(s.Messages))
	}
}

func TestToHistory(t *testing.T) {
	s := NewSession("m", "")
	s.Messages = []Message{
		NewUserMessage("a"),
		NewAssistantMessage("b"),
	}
	h := s.ToHistory()
	if h.Len() != 2 {
		t.Errorf("History.Len() = %d, want 2", h.Len())
	}
	snap := h.Snapshot()
	if snap[0].Content != "a" {
		t.Errorf("snap[0].Content = %q, want %q", snap[0].Content, "a")
	}
}

func TestShortID(t *testing.T) {
	s := &Session{ID: "20260528-171234-a3b2c1d4"}
	if got := s.ShortID(); got != "a3b2c1d4" {
		t.Errorf("ShortID = %q, want %q", got, "a3b2c1d4")
	}
	// Degenerate IDs (shorter than 8 chars) round-trip whole.
	short := &Session{ID: "abc"}
	if got := short.ShortID(); got != "abc" {
		t.Errorf("ShortID = %q, want %q", got, "abc")
	}
}

// seedSession creates and saves a session with a synthetic ID we control,
// so resolver tests can target known prefixes/suffixes.
func seedSession(t *testing.T, id string) *Session {
	t.Helper()
	s := &Session{ID: id, CreatedAt: time.Now(), Model: "test-model"}
	s.Messages = []Message{{Role: "user", Content: "hi"}}
	if err := s.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	return s
}

func TestResolveSessionID_Last(t *testing.T) {
	setTempHome(t)
	if _, err := ResolveSessionID("last"); err == nil {
		t.Fatal("expected error when no sessions exist")
	}
	seedSession(t, "20260101-000000-aaaaaaaa")
	// Sleep so mtimes are distinguishable across the two files.
	time.Sleep(20 * time.Millisecond)
	newest := seedSession(t, "20260201-000000-bbbbbbbb")

	got, err := ResolveSessionID("last")
	if err != nil {
		t.Fatalf("ResolveSessionID(last): %v", err)
	}
	if got != newest.ID {
		t.Errorf("got %q, want %q (newest)", got, newest.ID)
	}
}

func TestResolveSessionID_SubstringUnique(t *testing.T) {
	setTempHome(t)
	a := seedSession(t, "20260101-000000-aaaaaaaa")
	seedSession(t, "20260201-000000-bbbbbbbb")

	got, err := ResolveSessionID("aaaaaaaa")
	if err != nil {
		t.Fatalf("substring resolve: %v", err)
	}
	if got != a.ID {
		t.Errorf("got %q, want %q", got, a.ID)
	}
}

func TestResolveSessionID_Ambiguous(t *testing.T) {
	setTempHome(t)
	seedSession(t, "20260528-100000-aaaaaaaa")
	seedSession(t, "20260528-110000-aaaaaabb")

	// "aaaaaa" is a substring of both hex suffixes.
	_, err := ResolveSessionID("aaaaaa")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	msg := err.Error()
	if !contains(msg, "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %s", msg)
	}
}

func TestResolveSessionID_ExactFullIDFastPath(t *testing.T) {
	setTempHome(t)
	s := seedSession(t, "20260528-171234-a3b2c1d4")
	got, err := ResolveSessionID(s.ID)
	if err != nil {
		t.Fatalf("exact resolve: %v", err)
	}
	if got != s.ID {
		t.Errorf("got %q, want %q", got, s.ID)
	}
}

func TestResolveSessionID_NoMatch(t *testing.T) {
	setTempHome(t)
	seedSession(t, "20260528-100000-aaaaaaaa")
	if _, err := ResolveSessionID("not-a-real-suffix"); err == nil {
		t.Fatal("expected no-match error")
	}
}

// contains is a tiny local helper so the resolver tests don't pull in
// strings.Contains via a new import block. Keeps the test additions minimal.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
