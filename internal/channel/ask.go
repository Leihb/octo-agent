package channel

import "fmt"

// The interactive permission ask for IM channels: a tool call that resolves
// to an ask-class verdict sends a confirmation prompt into the chat, and the
// session's NEXT plain message is consumed as the answer. The inbound
// dispatcher routes that message via DeliverAskReply BEFORE spawning a turn —
// routing it through the normal turn path would deadlock behind runMu, which
// the asking turn still holds.

// BeginAsk claims the session's single ask slot and returns the channel on
// which the answer text will arrive, plus a release func the caller must run
// when the ask resolves (answer, timeout, or cancellation). A second BeginAsk
// while one is pending is refused — within a session turns are serialised, so
// this only guards against misuse.
func (s *Session) BeginAsk() (<-chan string, func(), error) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if s.pendingAsk != nil {
		return nil, nil, fmt.Errorf("an approval prompt is already pending in this chat")
	}
	ch := make(chan string, 1)
	s.pendingAsk = ch
	release := func() {
		s.askMu.Lock()
		if s.pendingAsk == ch {
			s.pendingAsk = nil
		}
		s.askMu.Unlock()
	}
	return ch, release, nil
}

// DeliverAskReply routes text to a pending ask and reports whether it was
// consumed. False means no ask is waiting — the caller should treat the
// message as normal chat input. Each ask consumes exactly one reply.
func (s *Session) DeliverAskReply(text string) bool {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if s.pendingAsk == nil {
		return false
	}
	s.pendingAsk <- text // buffered (cap 1); never blocks
	s.pendingAsk = nil
	return true
}
