package agent

import (
	"os"
	"strings"
	"testing"
)

// TestHistoryRewriteDirty checks that every non-append mutation marks the
// history as rewritten and that plain appends do not — Session.SyncFrom
// relies on this to know when the persisted file's prefix went stale.
func TestHistoryRewriteDirty(t *testing.T) {
	mutations := []struct {
		name string
		do   func(h *History)
	}{
		{"ReplaceAll", func(h *History) { h.ReplaceAll([]Message{NewUserMessage("x")}) }},
		{"Reset", func(h *History) { h.Reset() }},
		{"TruncateTo", func(h *History) { h.TruncateTo(1) }},
		{"replaceLast", func(h *History) { h.replaceLast(NewUserMessage("y")) }},
		{"popLast", func(h *History) { h.popLast() }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			h := NewHistory()
			h.Append(NewUserMessage("a"))
			h.Append(NewAssistantMessage("b"))
			if h.RewriteDirty() {
				t.Fatal("Append alone must not mark history rewritten")
			}
			m.do(h)
			if !h.RewriteDirty() {
				t.Fatalf("%s did not mark history rewritten", m.name)
			}
			if !h.takeRewriteDirty() {
				t.Fatal("takeRewriteDirty returned false on a dirty history")
			}
			if h.RewriteDirty() || h.takeRewriteDirty() {
				t.Fatal("takeRewriteDirty did not clear the flag")
			}
		})
	}
}

// TestSessionSave_RewriteAfterHistoryRewrite is the corruption guard for
// incremental mid-turn saves: a history rewrite (compaction) that leaves the
// message count at or above the persisted count used to slip past Save's
// length check, appending on top of a stale prefix. SyncFrom must carry the
// rewrite over so Save rewrites the file.
func TestSessionSave_RewriteAfterHistoryRewrite(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := NewSession("m", "")
	h := NewHistory()
	for _, m := range []Message{
		NewUserMessage("one"), NewAssistantMessage("two"), NewUserMessage("three"),
	} {
		h.Append(m)
	}
	sess.SyncFrom(h)
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Rewrite history to the SAME length with a different prefix — the case
	// the pure length check cannot see.
	h.ReplaceAll([]Message{
		NewUserMessage("[summary]"), NewUserMessage("three"), NewAssistantMessage("four"),
	})
	sess.SyncFrom(h)
	if err := sess.Save(); err != nil {
		t.Fatalf("save after rewrite: %v", err)
	}

	got, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Messages) != 3 || got.Messages[0].Content != "[summary]" || got.Messages[2].Content != "four" {
		t.Fatalf("persisted messages do not match rewritten history: %+v", got.Messages)
	}
}

// TestSessionSave_NoopWhenUnchanged checks the fast path the server's
// per-event persister depends on: a Save with no new messages must leave the
// file byte-for-byte untouched.
func TestSessionSave_NoopWhenUnchanged(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := NewSession("m", "")
	sess.Messages = []Message{NewUserMessage("hello")}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	path, _ := sess.SavePath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := sess.Save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("no-op Save modified the file")
	}
}

// TestLoadSession_DropsPartialTail simulates a crash mid-append: the file
// ends in an incomplete JSON line. Loading must succeed with the complete
// records, and the next Save must rewrite the file so the dangling bytes
// can't fuse with a future append into one corrupt line.
func TestLoadSession_DropsPartialTail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := NewSession("m", "")
	sess.Messages = []Message{NewUserMessage("hello"), NewAssistantMessage("world")}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	path, _ := sess.SavePath()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"content":"PARTIAL-TAIL`); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	f.Close()

	got, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("load with partial tail: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (partial tail dropped)", len(got.Messages))
	}

	// Resume the session: append a message and save. The partial tail must
	// not survive into the rewritten file.
	got.Messages = append(got.Messages, NewUserMessage("again"))
	if err := got.Save(); err != nil {
		t.Fatalf("save after tail drop: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "PARTIAL-TAIL") {
		t.Fatal("partial tail bytes survived the post-recovery save")
	}
	re, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(re.Messages) != 3 || re.Messages[2].Content != "again" {
		t.Fatalf("reloaded messages = %+v, want 3 ending in \"again\"", re.Messages)
	}
}

// TestSetTitle_RewritesAfterPartialTail: SetTitle appends a raw title record;
// after a crash left a partial tail it must rewrite instead, or the title
// record fuses with the dangling bytes into one corrupt line.
func TestSetTitle_RewritesAfterPartialTail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := NewSession("m", "")
	sess.Messages = []Message{NewUserMessage("hello")}
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	path, _ := sess.SavePath()
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"type":"mess`)
	f.Close()

	got, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := got.SetTitle("My Title"); err != nil {
		t.Fatalf("set title: %v", err)
	}
	re, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("reload after SetTitle: %v", err)
	}
	if re.Title != "My Title" || len(re.Messages) != 1 {
		t.Fatalf("reloaded title=%q messages=%d, want \"My Title\"/1", re.Title, len(re.Messages))
	}
}
