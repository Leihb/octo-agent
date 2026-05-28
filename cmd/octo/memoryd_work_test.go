package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/memoryd"
)

// ── pickMemorydProvider ─────────────────────────────────────────────────

func TestPickMemorydProvider_AnthropicOnly(t *testing.T) {
	t.Setenv("OCTO_MEMORYD_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("OPENAI_API_KEY", "")
	if got := pickMemorydProvider(); got != providerAnthropic {
		t.Errorf("Anthropic only → %q, want %q", got, providerAnthropic)
	}
}

func TestPickMemorydProvider_OpenAIOnly(t *testing.T) {
	t.Setenv("OCTO_MEMORYD_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if got := pickMemorydProvider(); got != providerOpenAI {
		t.Errorf("OpenAI only → %q, want %q", got, providerOpenAI)
	}
}

func TestPickMemorydProvider_AnthropicWinsWhenBothSet(t *testing.T) {
	t.Setenv("OCTO_MEMORYD_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-anth")
	t.Setenv("OPENAI_API_KEY", "sk-oai")
	if got := pickMemorydProvider(); got != providerAnthropic {
		t.Errorf("both set → Anthropic wins (alphabetical preference); got %q", got)
	}
}

func TestPickMemorydProvider_ExplicitOverride(t *testing.T) {
	t.Setenv("OCTO_MEMORYD_PROVIDER", providerOpenAI)
	t.Setenv("ANTHROPIC_API_KEY", "sk-anth") // would normally win
	t.Setenv("OPENAI_API_KEY", "sk-oai")
	if got := pickMemorydProvider(); got != providerOpenAI {
		t.Errorf("explicit override should beat both keys; got %q", got)
	}
}

func TestPickMemorydProvider_NeitherKeySet(t *testing.T) {
	t.Setenv("OCTO_MEMORYD_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	if got := pickMemorydProvider(); got != "" {
		t.Errorf("no keys → empty; got %q", got)
	}
}

// ── buildMemorydWork ────────────────────────────────────────────────────

func TestBuildMemorydWork_RefusesWithoutKeys(t *testing.T) {
	t.Setenv("OCTO_MEMORYD_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	fakeHomeForMemoryd(t)

	var errBuf bytes.Buffer
	work, err := buildMemorydWork(&errBuf, memoryd.DefaultIdleThreshold)
	if err == nil {
		t.Fatal("buildMemorydWork should refuse without API keys")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should name the missing env var: %v", err)
	}
	if work != nil {
		t.Error("WorkFn should be nil when buildMemorydWork errors")
	}
}

// ── Daemon.Work plumbing ────────────────────────────────────────────────

func TestDaemon_WorkCalledEachTick(t *testing.T) {
	var called int
	work := memoryd.WorkFn(func(_ context.Context) error {
		called++
		return nil
	})
	d := &memoryd.Daemon{Tick: 20 * time.Millisecond, Work: work}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if called < 2 {
		t.Errorf("expected ≥2 work calls in 70ms with 20ms tick, got %d", called)
	}
}

func TestDaemon_WorkErrorLoggedNotFatal(t *testing.T) {
	work := memoryd.WorkFn(func(_ context.Context) error {
		return errFake
	})
	var out bytes.Buffer
	d := &memoryd.Daemon{Tick: 20 * time.Millisecond, Work: work, Out: &out}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if !strings.Contains(out.String(), "tick error: fake error") {
		t.Errorf("tick error should be logged:\n%s", out.String())
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (e *fakeErr) Error() string { return "fake error" }

// ── extractIdleSession ──────────────────────────────────────────────────

// memorydStubSender returns a canned ExtractMemory reply (three-piece
// object) so extractIdleSession has something to persist.
type memorydStubSender struct{ reply string }

func (s *memorydStubSender) SendMessages(_ context.Context, _, _ string, _ []agent.Message, _ int) (agent.Reply, error) {
	return agent.Reply{Content: s.reply, InputTokens: 1, OutputTokens: 1}, nil
}

func TestExtractIdleSession_SkipsActiveSession(t *testing.T) {
	dir := fakeHomeForMemoryd(t)
	// Make a session whose mtime is now (active — within threshold).
	id := writeFakeSession(t, dir, time.Now())

	store := memory.NewStoreAt(filepath.Join(dir, ".octo", "memory"))
	a := agent.New(&memorydStubSender{reply: `{"facts":[]}`}, "test-model")
	st := memory.State{}
	var stderr bytes.Buffer
	extractIdleSession(context.Background(), a, store, &stderr, time.Hour, &st)

	if st.LastExtractedSession == id {
		t.Errorf("active session should NOT have been extracted, got cursor=%s", st.LastExtractedSession)
	}
}

func TestExtractIdleSession_ExtractsQuietSession(t *testing.T) {
	dir := fakeHomeForMemoryd(t)
	// Make a session whose mtime is 2 hours ago (well past threshold).
	id := writeFakeSession(t, dir, time.Now().Add(-2*time.Hour))

	store := memory.NewStoreAt(filepath.Join(dir, ".octo", "memory"))
	reply := `{"rollout_slug":"x","rollout_summary":"# done","facts":[{"type":"user","description":"d","content":"c"}]}`
	a := agent.New(&memorydStubSender{reply: reply}, "test-model")
	st := memory.State{}
	var stderr bytes.Buffer
	extractIdleSession(context.Background(), a, store, &stderr, time.Hour, &st)

	if st.LastExtractedSession != id {
		t.Errorf("quiet session should have been extracted; cursor=%s want %s", st.LastExtractedSession, id)
	}
	entries, _ := store.List()
	if len(entries) != 1 {
		t.Errorf("expected 1 entry persisted, got %d", len(entries))
	}
}

func TestExtractIdleSession_StopsAtCursor(t *testing.T) {
	dir := fakeHomeForMemoryd(t)
	id1 := writeFakeSession(t, dir, time.Now().Add(-2*time.Hour))

	store := memory.NewStoreAt(filepath.Join(dir, ".octo", "memory"))
	// Cursor already at id1 → extractIdleSession returns without doing
	// anything.
	st := memory.State{LastExtractedSession: id1}
	a := agent.New(&memorydStubSender{reply: `{"facts":[]}`}, "test-model")
	var stderr bytes.Buffer
	extractIdleSession(context.Background(), a, store, &stderr, time.Hour, &st)

	if st.LastExtractedSession != id1 {
		t.Errorf("cursor should be unchanged when nothing new; got %s", st.LastExtractedSession)
	}
	entries, _ := store.List()
	if len(entries) != 0 {
		t.Errorf("no entries should be persisted; got %d", len(entries))
	}
}

// writeFakeSession creates a session JSONL file under
// ~/.octo/sessions/ with the given mtime and a single user/assistant
// turn so TurnCount > 0. Returns the session id.
func writeFakeSession(t *testing.T, home string, mtime time.Time) string {
	t.Helper()
	sess := agent.NewSession("test-model", "")
	h := agent.NewHistory()
	h.Append(agent.NewUserMessage("hi"))
	h.Append(agent.NewAssistantMessage("hello"))
	sess.SyncFrom(h)
	if err := sess.Save(); err != nil {
		t.Fatal(err)
	}
	// Locate the session file and chtime it.
	sessDir := filepath.Join(home, ".octo", "sessions")
	matches, err := filepath.Glob(filepath.Join(sessDir, sess.ID+"*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("session file not found under %s", sessDir)
	}
	if err := os.Chtimes(matches[0], mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return sess.ID
}
