package retry

import (
	"errors"
	"io"
	"time"
)

// ErrStreamIdle is returned by a reader from IdleTimeoutReader when no bytes
// arrive for longer than the configured idle window — the server accepted the
// streaming connection and then went silent without closing it. Providers
// surface this to fail the turn cleanly instead of blocking forever in a read.
//
// Unlike request-establishment failures (see Do), a mid-stream stall is not
// retried: the turn has already emitted tokens to the caller, so an auto-retry
// would duplicate them. Failing cleanly lets the agent loop roll its history
// back and the caller (or an unattended eval/cron run) decide whether to re-run.
var ErrStreamIdle = errors.New("provider: streaming response idle timeout (server stopped sending without closing)")

// IdleTimeoutReader wraps r so that any single read which makes no progress for
// longer than timeout aborts the stream: onIdle is invoked — typically a
// context cancel func that tears down the underlying HTTP request and unblocks
// the in-flight read — and the read returns ErrStreamIdle. The window is per
// read, so a healthy stream that keeps sending frames resets it on every chunk;
// only a genuine silent stall trips it.
//
// A non-positive timeout disables the guard and returns r unchanged, so callers
// can opt out (or leave it off where reads are trusted to be fast, e.g.
// httptest) without branching at the call site.
func IdleTimeoutReader(r io.Reader, timeout time.Duration, onIdle func()) io.Reader {
	if timeout <= 0 {
		return r
	}
	return &idleReader{r: r, timeout: timeout, onIdle: onIdle}
}

type idleReader struct {
	r        io.Reader
	timeout  time.Duration
	onIdle   func()
	timedOut bool
}

type readResult struct {
	n   int
	err error
}

// Read races a background read against a per-read timer. On timeout it fires
// onIdle (to cancel the request, which unblocks the abandoned read) and returns
// ErrStreamIdle; the timedOut latch makes every subsequent call return the same
// error without spawning another goroutine.
func (t *idleReader) Read(p []byte) (int, error) {
	if t.timedOut {
		return 0, ErrStreamIdle
	}
	// Buffered so the goroutine never leaks if we abandon it on timeout: its
	// eventual Read return lands in the channel and the goroutine exits.
	ch := make(chan readResult, 1)
	go func() {
		n, err := t.r.Read(p)
		ch <- readResult{n, err}
	}()
	timer := time.NewTimer(t.timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-timer.C:
		t.timedOut = true
		if t.onIdle != nil {
			t.onIdle() // cancel the request: unblocks the goroutine's blocked read
		}
		return 0, ErrStreamIdle
	}
}
