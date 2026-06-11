package tools

import (
	"context"
	"strings"
	"testing"
)

// recordingAsker captures the request and returns a canned response.
type recordingAsker struct {
	got  *AskRequest
	resp AskResponse
}

func (r *recordingAsker) Ask(_ context.Context, q AskRequest) (AskResponse, error) {
	r.got = &q
	return r.resp, nil
}

// TestAskUserQuestion_CtxAskerOverridesGlobal: a ctx-scoped asker (stamped by
// the IM turn) must win over the process-global one (the server's wsAsker) —
// otherwise IM questions broadcast to browser tabs that don't exist.
func TestAskUserQuestion_CtxAskerOverridesGlobal(t *testing.T) {
	global := &recordingAsker{resp: AskResponse{Choices: []string{"globalpick"}}}
	SetAsker(global)
	t.Cleanup(func() { SetAsker(nil) })

	ctxAsker := &recordingAsker{resp: AskResponse{Choices: []string{"ctxpick"}}}
	ctx := WithAsker(context.Background(), ctxAsker)

	res, err := AskUserQuestionTool{}.Execute(ctx, "ask_user_question", map[string]any{
		"question": "Which one?",
		"options":  []any{"A", "B"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ctxAsker.got == nil {
		t.Fatal("ctx asker was not used")
	}
	if global.got != nil {
		t.Error("global asker must not be consulted when a ctx asker is present")
	}
	if !strings.Contains(res.Text, "ctxpick") {
		t.Errorf("result %q should carry the ctx asker's answer", res.Text)
	}
}

// TestAskUserQuestion_GlobalFallback: without a ctx asker the global one
// still serves (CLI/web behavior unchanged).
func TestAskUserQuestion_GlobalFallback(t *testing.T) {
	global := &recordingAsker{resp: AskResponse{Choices: []string{"globalpick"}}}
	SetAsker(global)
	t.Cleanup(func() { SetAsker(nil) })

	res, err := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Which one?",
		"options":  []any{"A", "B"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "globalpick") {
		t.Errorf("result %q should carry the global asker's answer", res.Text)
	}
}
