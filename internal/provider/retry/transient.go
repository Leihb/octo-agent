package retry

import (
	"context"
	"errors"
)

// transientStreamErr wraps a mid-stream transport failure — an HTTP/2 stream
// reset (INTERNAL_ERROR / GOAWAY), a connection drop, or an unexpected EOF
// while reading the SSE body — so the agent loop recognises it as recoverable
// (via the same TransientStream() interface streamIdleError uses) and re-issues
// the round from unchanged history. Like the idle case, the partial reply was
// never committed, so the retry is safe.
type transientStreamErr struct{ err error }

func (e transientStreamErr) Error() string       { return e.err.Error() }
func (e transientStreamErr) Unwrap() error       { return e.err }
func (transientStreamErr) TransientStream() bool { return true }

// AsTransientStream marks a mid-stream read failure as a recoverable transient
// stream error, EXCEPT a genuine context cancellation or deadline — that's the
// caller stopping the turn (a user interrupt or an overall timeout), which must
// not be auto-retried. err==nil passes through as nil. Providers wrap the error
// from a mid-stream body read with this so a flaky gateway/proxy that resets a
// long stream is retried instead of failing the turn.
func AsTransientStream(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return transientStreamErr{err}
}
