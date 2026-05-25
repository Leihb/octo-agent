package agent

import (
	"context"
	"errors"
	"testing"
)

// fakeSender implements Sender for tests, recording its inputs and returning
// canned replies.
type fakeSender struct {
	gotModel    string
	gotSystem   string
	gotMessages []Message
	gotMaxToks  int

	reply Reply
	err   error
}

func (f *fakeSender) SendMessages(_ context.Context, model, system string, messages []Message, maxTokens int) (Reply, error) {
	f.gotModel = model
	f.gotSystem = system
	f.gotMessages = append([]Message(nil), messages...) // defensive copy
	f.gotMaxToks = maxTokens
	if f.err != nil {
		return Reply{}, f.err
	}
	return f.reply, nil
}

func TestAgent_Turn_HappyPath(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "hi from agent", Model: "m", StopReason: "end_turn"}}
	a := New(send, "claude-test")
	a.System = "you are octo"

	reply, err := a.Turn(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if reply.Content != "hi from agent" {
		t.Errorf("reply.Content = %q", reply.Content)
	}

	// Sender saw model + system + the single user message
	if send.gotModel != "claude-test" {
		t.Errorf("Sender saw model %q", send.gotModel)
	}
	if send.gotSystem != "you are octo" {
		t.Errorf("Sender saw system %q", send.gotSystem)
	}
	if len(send.gotMessages) != 1 || send.gotMessages[0].Role != RoleUser {
		t.Errorf("Sender saw messages %+v", send.gotMessages)
	}

	// History now has [user, assistant]
	snap := a.History.Snapshot()
	if len(snap) != 2 || snap[0].Role != RoleUser || snap[1].Role != RoleAssistant {
		t.Errorf("History after Turn = %+v", snap)
	}
}

func TestAgent_Turn_MultiTurnSendsFullHistory(t *testing.T) {
	send := &fakeSender{reply: Reply{Content: "ok"}}
	a := New(send, "m")

	for _, msg := range []string{"first", "second", "third"} {
		if _, err := a.Turn(context.Background(), msg); err != nil {
			t.Fatalf("Turn(%q): %v", msg, err)
		}
	}

	// On the third call the Sender must have seen [user, asst, user, asst, user].
	if got := len(send.gotMessages); got != 5 {
		t.Fatalf("len(msgs) on 3rd turn = %d, want 5", got)
	}
	wantRoles := []Role{RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser}
	for i, want := range wantRoles {
		if send.gotMessages[i].Role != want {
			t.Errorf("messages[%d].Role = %q, want %q", i, send.gotMessages[i].Role, want)
		}
	}
}

func TestAgent_Turn_SenderError_RestoresHistory(t *testing.T) {
	send := &fakeSender{err: errors.New("upstream 500")}
	a := New(send, "m")

	if _, err := a.Turn(context.Background(), "hello"); err == nil {
		t.Fatal("Turn: expected error, got nil")
	}

	// User message must be rolled back so the next attempt isn't a dup.
	if n := a.History.Len(); n != 0 {
		t.Errorf("History.Len after failed Turn = %d, want 0", n)
	}
}

func TestAgent_Turn_Validation(t *testing.T) {
	a := New(&fakeSender{}, "")
	if _, err := a.Turn(context.Background(), "hi"); err == nil {
		t.Error("empty model should error")
	}

	a = New(nil, "m")
	if _, err := a.Turn(context.Background(), "hi"); err == nil {
		t.Error("nil sender should error")
	}

	a = New(&fakeSender{}, "m")
	if _, err := a.Turn(context.Background(), ""); err == nil {
		t.Error("empty input should error")
	}
}
