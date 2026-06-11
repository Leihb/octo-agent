package channel

import (
	"testing"
	"time"
)

func TestSession_BeginAskDeliverReply(t *testing.T) {
	sess := &Session{}

	replyCh, release, err := sess.BeginAsk()
	if err != nil {
		t.Fatalf("BeginAsk: %v", err)
	}
	defer release()

	if !sess.DeliverAskReply("yes") {
		t.Fatal("DeliverAskReply = false with a pending ask, want true")
	}
	select {
	case got := <-replyCh:
		if got != "yes" {
			t.Errorf("reply = %q, want %q", got, "yes")
		}
	case <-time.After(time.Second):
		t.Fatal("reply not delivered to the ask channel")
	}
}

func TestSession_DeliverWithoutPendingAsk(t *testing.T) {
	sess := &Session{}
	if sess.DeliverAskReply("hello") {
		t.Error("DeliverAskReply = true with no pending ask; the message must flow to a normal turn")
	}
}

func TestSession_SecondBeginAskRefused(t *testing.T) {
	sess := &Session{}
	_, release, err := sess.BeginAsk()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, _, err := sess.BeginAsk(); err == nil {
		t.Error("second BeginAsk should be refused while one is pending")
	}
}

func TestSession_ReleaseClearsPending(t *testing.T) {
	sess := &Session{}
	_, release, err := sess.BeginAsk()
	if err != nil {
		t.Fatal(err)
	}
	release()

	if sess.DeliverAskReply("late") {
		t.Error("a reply after release must not be consumed")
	}
	// The slot is reusable after release.
	if _, release2, err := sess.BeginAsk(); err != nil {
		t.Errorf("BeginAsk after release: %v", err)
	} else {
		release2()
	}
}

func TestSession_OneReplyOnly(t *testing.T) {
	sess := &Session{}
	_, release, err := sess.BeginAsk()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if !sess.DeliverAskReply("yes") {
		t.Fatal("first reply should be consumed")
	}
	if sess.DeliverAskReply("again") {
		t.Error("second reply must not be consumed — the ask is already answered")
	}
}
