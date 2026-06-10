package server

import (
	"errors"
	"sync"
	"time"
)

// errDraining is returned by drainGate.begin once a restart drain has
// started. Handlers map it to their transport's "try again shortly" shape:
// HTTP 503, a WS error event, or a polite IM text reply.
var errDraining = errors.New("server is restarting; try again shortly")

// drainGate counts in-flight turns and, once draining, refuses new ones.
// Every turn execution path calls begin/end; Restart calls drain to wait for
// the in-flight set to empty before shutting down. The zero value is ready
// to use.
//
// This deliberately gates turn *intake* rather than stopping IM adapters
// up-front: an adapter must stay up until the drain completes so the turn
// that triggered the restart can deliver its final "restarting now" reply.
type drainGate struct {
	mu       sync.Mutex
	draining bool
	active   int
	idle     chan struct{} // non-nil while a drain waits on active turns
}

// begin registers a turn. It fails with errDraining once drain has started.
func (g *drainGate) begin() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return errDraining
	}
	g.active++
	return nil
}

// end deregisters a turn registered with begin.
func (g *drainGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active < 0 {
		// An unmatched end would otherwise silently make every drain hang
		// its full timeout (active never reaches 0 again). Fail loud.
		panic("drainGate: end without begin")
	}
	if g.draining && g.active == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// isDraining reports whether a drain has started. Turn loops use it to stop
// chaining queued follow-up turns once a restart is underway.
func (g *drainGate) isDraining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
}

// drain blocks new turns and waits up to timeout for active ones to finish.
// It reports whether the drain completed cleanly; on false the caller
// proceeds anyway (session persistence bounds the damage at one round).
func (g *drainGate) drain(timeout time.Duration) bool {
	g.mu.Lock()
	g.draining = true
	if g.active == 0 {
		g.mu.Unlock()
		return true
	}
	// Reuse an existing waiter channel so a second drain call doesn't strand
	// the first caller on a channel nobody will ever close.
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()

	select {
	case <-idle:
		return true
	case <-time.After(timeout):
		return false
	}
}
