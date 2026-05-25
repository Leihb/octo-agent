package agent

import "sync"

// History is the in-memory conversation log for one session. Concurrent-safe
// because the Web UI and the agent run loop touch it from different goroutines
// in later milestones.
//
// History does NOT include the system prompt — providers carry that
// out-of-band (Anthropic's top-level `system` field, OpenAI's first message
// with role "system"). Keep the system prompt on the Agent struct or as a
// constructor arg.
type History struct {
	mu       sync.RWMutex
	messages []Message
}

// NewHistory returns an empty History.
func NewHistory() *History {
	return &History{}
}

// Append adds a message to the end of the history.
func (h *History) Append(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, m)
}

// Snapshot returns a copy of the message slice safe to iterate without holding
// the lock. The returned slice's backing array is fresh; callers can mutate it.
func (h *History) Snapshot() []Message {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Message, len(h.messages))
	copy(out, h.messages)
	return out
}

// Len returns the number of messages currently in history.
func (h *History) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.messages)
}

// Reset drops all messages. Intended for "start a new session" UX.
func (h *History) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = nil
}
