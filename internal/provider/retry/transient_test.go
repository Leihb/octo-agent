package retry

import (
	"context"
	"errors"
	"io"
	"testing"
)

func hasTransientStream(err error) bool {
	var t interface{ TransientStream() bool }
	return errors.As(err, &t) && t.TransientStream()
}

func TestAsTransientStream(t *testing.T) {
	if AsTransientStream(nil) != nil {
		t.Error("nil should pass through as nil")
	}

	// Transport-level mid-stream failures are transient.
	if got := AsTransientStream(io.ErrUnexpectedEOF); !hasTransientStream(got) {
		t.Errorf("ErrUnexpectedEOF should be transient, got %v", got)
	}
	if got := AsTransientStream(errors.New("stream ID 3; INTERNAL_ERROR; received from peer")); !hasTransientStream(got) {
		t.Errorf("HTTP/2 reset should be transient, got %v", got)
	}

	// Caller cancellation / deadline must NOT be retried.
	if got := AsTransientStream(context.Canceled); hasTransientStream(got) {
		t.Error("context.Canceled must not be transient")
	}
	if got := AsTransientStream(context.DeadlineExceeded); hasTransientStream(got) {
		t.Error("context.DeadlineExceeded must not be transient")
	}
	// Wrapped cancellation is still recognised through the chain.
	if got := AsTransientStream(errors.New("read: " + context.Canceled.Error())); !hasTransientStream(got) {
		// A string-only mention isn't a real wrap; this should be transient.
		// (Guards against matching by message instead of errors.Is.)
		t.Error("a non-wrapping mention of cancellation should still be transient")
	}
	if got := AsTransientStream(wrapErr{context.Canceled}); hasTransientStream(got) {
		t.Error("a genuinely wrapped context.Canceled must not be transient")
	}

	// Unwrap exposes the original error.
	orig := io.ErrUnexpectedEOF
	if !errors.Is(AsTransientStream(orig), orig) {
		t.Error("AsTransientStream should preserve the wrapped error for errors.Is")
	}
}

type wrapErr struct{ e error }

func (w wrapErr) Error() string { return "wrapped: " + w.e.Error() }
func (w wrapErr) Unwrap() error { return w.e }
