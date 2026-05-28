package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/memory"
	"github.com/Leihb/octo-agent/internal/memoryd"
)

// buildMemorydWork assembles the daemon's per-tick work function: build
// a provider from the environment (refusing if neither key is set),
// stand up an agent + memory store, and return a closure that on each
// tick extracts from one idle session + runs consolidateIfDue.
//
// The function is built ONCE, before the daemon's signal handler is
// wired, so a missing-key error fails fast with a clear message
// rather than after the user has already seen "started".
func buildMemorydWork(stderr io.Writer, idleThreshold time.Duration) (memoryd.WorkFn, error) {
	provName := pickMemorydProvider()
	if provName == "" {
		return nil, errors.New("octo memoryd: set ANTHROPIC_API_KEY or OPENAI_API_KEY before starting (no chat-side provider config to inherit)")
	}
	prov, err := buildProvider(provName, stderr)
	if err != nil {
		return nil, err
	}
	modelName := defaultModels[provName]

	store, err := memory.NewStore()
	if err != nil {
		return nil, fmt.Errorf("octo memoryd: memory store: %w", err)
	}

	a := agent.New(providerSender{p: prov, cacheKey: newCacheKey()}, modelName)
	a.MaxTokens = 4096

	return func(ctx context.Context) error {
		st := store.LoadState()
		extractIdleSession(ctx, a, store, stderr, idleThreshold, &st)
		consolidateIfDue(ctx, a, store, &st)
		return store.SaveState(st)
	}, nil
}

// pickMemorydProvider chooses which provider the daemon should use,
// based on which credential is present in the environment. An explicit
// OCTO_MEMORYD_PROVIDER env var wins when set (lets the user pin to a
// specific backend even when both keys are present, e.g. running
// chat against Anthropic but memoryd against a cheaper OpenAI model).
// Returns "" when no usable provider is configured.
func pickMemorydProvider() string {
	if v := os.Getenv("OCTO_MEMORYD_PROVIDER"); v != "" {
		return v
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return providerAnthropic
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return providerOpenAI
	}
	return ""
}

// extractIdleSession finds the newest session that is (a) quiet for at
// least idleThreshold and (b) not yet extracted, and runs the extract
// pass against it. Only ONE session per tick by design — the next tick
// catches up on the rest, and keeping the per-tick work small means
// SIGTERM never finds the daemon mid-extract for too long.
//
// "Quiet" means the session file's mtime is older than now - threshold.
// The chat path mutates the session file on every turn (append-only
// JSONL), so a recent mtime is a strong signal that a human or another
// agent is still typing — the daemon defers extraction so a still-live
// session's transcript doesn't get extracted mid-flight.
func extractIdleSession(ctx context.Context, a *agent.Agent, store *memory.Store, stderr io.Writer, idleThreshold time.Duration, st *memory.State) {
	sessions, err := agent.ListSessions(memorydSessionsWindow)
	if err != nil {
		return
	}
	now := time.Now()
	for _, sess := range sessions {
		// Hit the boundary: this session and everything older are already
		// extracted (the cursor advances monotonically per session
		// timestamp, so anything before LastExtractedSession is done).
		if sess.ID == st.LastExtractedSession {
			return
		}
		if sess.TurnCount() == 0 {
			continue
		}
		mtime, err := agent.SessionMTime(sess.ID)
		if err != nil {
			continue
		}
		if now.Sub(mtime) < idleThreshold {
			continue // still active — defer
		}

		// Eligible — extract.
		res, err := a.ExtractMemory(ctx, sess.ToHistory().Snapshot())
		if err != nil {
			fmt.Fprintf(stderr, "octo memoryd: extract %s: %v\n", sess.ID, err)
			return
		}
		n := 0
		for _, f := range res.Facts {
			if serr := store.Save(memory.Entry{
				Description: f.Description,
				Type:        memory.Type(f.Type),
				Body:        f.Content,
			}); serr == nil {
				n++
			}
		}
		slug := res.Slug
		if slug == "" {
			slug = sess.ID
		}
		_ = store.SaveRolloutSummary(slug, res.Summary)
		st.LastExtractedSession = sess.ID

		if n > 0 || res.Summary != "" {
			fmt.Fprintf(stderr, "octo memoryd: extracted %d fact(s) from %s\n", n, sess.ID)
		}
		return // one session per tick
	}
}

// memorydSessionsWindow caps how far back the daemon looks for un-
// extracted sessions per tick. Larger than the chat-side window (which
// is 2) because the daemon is the catch-up path: if a user started
// many sessions while the daemon was off, the daemon needs a fatter
// window to walk back through them. 20 is generous without making
// the per-tick filesystem scan expensive.
const memorydSessionsWindow = 20
