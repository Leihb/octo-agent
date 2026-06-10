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

	// rewritten is set by every mutation that isn't a plain Append (compaction,
	// overflow recovery, tool-pairing repair, popLast). Session.SyncFrom
	// consumes it to learn that the persisted file's prefix may no longer match
	// memory, so the next Save must rewrite the file instead of appending. A
	// pure length comparison can't detect this: a rewrite can leave history at
	// or above the persisted length while changing earlier messages.
	rewritten bool
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
	h.rewritten = true
}

// TruncateTo keeps only the first n messages.
// Used by overflow recovery to pop messages from tail.
func (h *History) TruncateTo(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n < len(h.messages) {
		h.messages = h.messages[:n]
		h.rewritten = true
	}
}

// ReplaceAll atomically replaces the entire message list.
// Used by compaction to rebuild history from summary + recent messages.
func (h *History) ReplaceAll(msgs []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = make([]Message, len(msgs))
	copy(h.messages, msgs)
	h.rewritten = true
}

// Tail returns the last n messages (or all if fewer).
func (h *History) Tail(n int) []Message {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if n >= len(h.messages) {
		out := make([]Message, len(h.messages))
		copy(out, h.messages)
		return out
	}
	out := make([]Message, n)
	copy(out, h.messages[len(h.messages)-n:])
	return out
}

// replaceLast replaces the last message in history with m. If history is empty,
// this is a no-op. Used by ensureToolPairing to merge synthetic tool_results
// into an existing user message (e.g., from inbox drain) to preserve the
// tool_use/tool_result pairing requirement.
func (h *History) replaceLast(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) == 0 {
		return
	}
	h.messages[len(h.messages)-1] = m
	h.rewritten = true
}

// RewriteDirty reports whether history has been rewritten (any non-append
// mutation) since the flag was last consumed by takeRewriteDirty. Callers use
// it as a cheap "do I need to re-sync" check; it does not clear the flag.
func (h *History) RewriteDirty() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rewritten
}

// takeRewriteDirty returns the rewritten flag and clears it. Consumed by
// Session.SyncFrom, which translates it into a full-file rewrite on Save.
func (h *History) takeRewriteDirty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.rewritten
	h.rewritten = false
	return d
}

// FindSystemMsg returns the first system message index, or -1.
func (h *History) FindSystemMsg() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i, m := range h.messages {
		if m.Role == RoleSystem {
			return i
		}
	}
	return -1
}
