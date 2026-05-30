package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubAsker records what the tool handed it and replays a canned response.
type stubAsker struct {
	resp    AskResponse
	err     error
	called  bool
	lastReq AskRequest
}

func (s *stubAsker) Ask(_ context.Context, q AskRequest) (AskResponse, error) {
	s.called = true
	s.lastReq = q
	return s.resp, s.err
}

func useAsker(t *testing.T, a Asker) {
	t.Helper()
	SetAsker(a)
	t.Cleanup(func() { SetAsker(nil) })
}

func TestAskUserQuestionTool_Schema(t *testing.T) {
	def := AskUserQuestionTool{}.Definition()
	if def.Name != "ask_user_question" {
		t.Errorf("Name = %q", def.Name)
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, want := range []string{"question", "options", "multi_select", "header"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema missing property %q", want)
		}
	}
	required, _ := def.Parameters["required"].([]string)
	if !sliceContains(required, "question") || !sliceContains(required, "options") {
		t.Errorf("question + options should be required, got %v", required)
	}
}

func TestAskUserQuestionTool_Execute_SingleChoice(t *testing.T) {
	stub := &stubAsker{resp: AskResponse{Choices: []string{"OAuth"}}}
	useAsker(t, stub)

	out, err := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Which auth method?",
		"options":  []any{"OAuth", "API key", "mTLS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "User chose: OAuth" {
		t.Errorf("Execute result = %q", out.Text)
	}
	if !stub.called {
		t.Error("asker was never invoked")
	}
	if stub.lastReq.Question != "Which auth method?" {
		t.Errorf("question not forwarded: %q", stub.lastReq.Question)
	}
	if len(stub.lastReq.Options) != 3 {
		t.Errorf("options not forwarded: %v", stub.lastReq.Options)
	}
}

func TestAskUserQuestionTool_Execute_MultiChoice(t *testing.T) {
	stub := &stubAsker{resp: AskResponse{Choices: []string{"OAuth", "API key"}}}
	useAsker(t, stub)

	out, err := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question":     "Which auth methods should we support?",
		"options":      []any{"OAuth", "API key", "mTLS"},
		"multi_select": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "User chose: OAuth, API key" {
		t.Errorf("Execute result = %q", out.Text)
	}
	if !stub.lastReq.MultiSelect {
		t.Error("multi_select flag not forwarded")
	}
}

func TestAskUserQuestionTool_Execute_OtherFreeText(t *testing.T) {
	stub := &stubAsker{resp: AskResponse{Custom: "Kerberos with constrained delegation"}}
	useAsker(t, stub)

	out, _ := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Which auth method?",
		"options":  []any{"OAuth", "API key"},
	})
	if !strings.HasPrefix(out.Text, "User chose: Other — ") {
		t.Errorf("free-text answer should be reported as Other — …, got %q", out.Text)
	}
	if !strings.Contains(out.Text, "Kerberos with constrained delegation") {
		t.Errorf("free-text payload lost: %q", out.Text)
	}
}

func TestAskUserQuestionTool_Execute_Cancelled(t *testing.T) {
	useAsker(t, &stubAsker{resp: AskResponse{Cancelled: true}})
	out, err := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Which?",
		"options":  []any{"a", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "(user cancelled)" {
		t.Errorf("cancellation = %q, want '(user cancelled)'", out.Text)
	}
}

func TestAskUserQuestionTool_Execute_HeaderForwarded(t *testing.T) {
	stub := &stubAsker{resp: AskResponse{Choices: []string{"a"}}}
	useAsker(t, stub)
	_, _ = AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Q?",
		"options":  []any{"a", "b"},
		"header":   "auth_method",
	})
	if stub.lastReq.Header != "auth_method" {
		t.Errorf("header not forwarded: %q", stub.lastReq.Header)
	}
}

func TestAskUserQuestionTool_Execute_AskerError(t *testing.T) {
	useAsker(t, &stubAsker{err: errors.New("stdin closed")})
	_, err := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Q?",
		"options":  []any{"a", "b"},
	})
	if err == nil || !strings.Contains(err.Error(), "stdin closed") {
		t.Errorf("asker error should propagate, got %v", err)
	}
}

func TestAskUserQuestionTool_Execute_NoAsker(t *testing.T) {
	SetAsker(nil)
	_, err := AskUserQuestionTool{}.Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Q?",
		"options":  []any{"a", "b"},
	})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("no asker should error 'not available', got %v", err)
	}
}

func TestAskUserQuestionTool_Execute_Validation(t *testing.T) {
	useAsker(t, &stubAsker{resp: AskResponse{Choices: []string{"a"}}})

	// Missing question.
	if _, err := (AskUserQuestionTool{}).Execute(context.Background(), "ask_user_question", map[string]any{
		"options": []any{"a", "b"},
	}); err == nil || !strings.Contains(err.Error(), "question is required") {
		t.Errorf("missing question should error, got %v", err)
	}

	// Too few options.
	if _, err := (AskUserQuestionTool{}).Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Q?",
		"options":  []any{"only"},
	}); err == nil || !strings.Contains(err.Error(), "2-4 entries") {
		t.Errorf("single option should error, got %v", err)
	}

	// Too many options.
	if _, err := (AskUserQuestionTool{}).Execute(context.Background(), "ask_user_question", map[string]any{
		"question": "Q?",
		"options":  []any{"a", "b", "c", "d", "e"},
	}); err == nil || !strings.Contains(err.Error(), "2-4 entries") {
		t.Errorf("5 options should error, got %v", err)
	}
}

func TestDefaultTools_AskGatedOnAsker(t *testing.T) {
	SetAsker(nil)
	t.Cleanup(func() { SetAsker(nil) })

	has := func() bool {
		for _, d := range DefaultTools() {
			if d.Name == "ask_user_question" {
				return true
			}
		}
		return false
	}

	if has() {
		t.Error("ask_user_question should be absent when no asker is configured")
	}
	useAsker(t, &stubAsker{})
	if !has() {
		t.Error("ask_user_question should appear once an asker is registered")
	}
}

func TestFormatAskResponse(t *testing.T) {
	cases := []struct {
		name string
		in   AskResponse
		want string
	}{
		{"cancelled", AskResponse{Cancelled: true}, "(user cancelled)"},
		{"single", AskResponse{Choices: []string{"OAuth"}}, "User chose: OAuth"},
		{"multi", AskResponse{Choices: []string{"a", "b"}}, "User chose: a, b"},
		{"other", AskResponse{Custom: "Kerberos"}, "User chose: Other — Kerberos"},
		{"empty (defensive)", AskResponse{}, "(user cancelled)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAskResponse(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func sliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
