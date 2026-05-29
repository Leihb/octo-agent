package retry

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingReader blocks every Read until release is closed, then returns EOF.
// It records whether a Read was ever entered so a test can confirm the reader
// was actually exercised.
type blockingReader struct {
	release chan struct{}
	mu      sync.Mutex
	entered bool
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.entered = true
	b.mu.Unlock()
	<-b.release
	return 0, io.EOF
}

func TestIdleTimeoutReader_PassthroughWhenDisabled(t *testing.T) {
	src := strings.NewReader("hello")
	got := IdleTimeoutReader(src, 0, func() { t.Fatal("onIdle should not fire") })
	if got != io.Reader(src) {
		t.Fatalf("non-positive timeout should return the reader unchanged, got %T", got)
	}
}

func TestIdleTimeoutReader_HealthyReadPassesThrough(t *testing.T) {
	r := IdleTimeoutReader(strings.NewReader("hello world"), 50*time.Millisecond, func() {
		t.Fatal("onIdle should not fire on a prompt read")
	})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != "hello world" {
		t.Errorf("got %q, want %q", out, "hello world")
	}
}

func TestIdleTimeoutReader_FiresOnStall(t *testing.T) {
	blk := &blockingReader{release: make(chan struct{})}
	var onIdleCalls int
	var mu sync.Mutex
	r := IdleTimeoutReader(blk, 20*time.Millisecond, func() {
		mu.Lock()
		onIdleCalls++
		mu.Unlock()
	})

	start := time.Now()
	n, err := r.Read(make([]byte, 8))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if elapsed < 20*time.Millisecond {
		t.Errorf("returned after %v, want >= idle window", elapsed)
	}
	mu.Lock()
	if onIdleCalls != 1 {
		t.Errorf("onIdle called %d times, want 1", onIdleCalls)
	}
	mu.Unlock()

	// A second read returns the same error without blocking or re-invoking
	// onIdle (the timedOut latch).
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, ErrStreamIdle) {
		t.Errorf("second read err = %v, want ErrStreamIdle", err)
	}
	mu.Lock()
	if onIdleCalls != 1 {
		t.Errorf("onIdle called %d times after second read, want 1", onIdleCalls)
	}
	mu.Unlock()

	// Release the abandoned goroutine so the test leaves nothing blocked.
	close(blk.release)
}
