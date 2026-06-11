package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/Leihb/octo-agent/internal/tools"
)

func TestIsAffirmative(t *testing.T) {
	yes := []string{"yes", "y", "OK", " ok ", "Allow", "是", "可以", "同意", "允许", "YES"}
	for _, s := range yes {
		if !isAffirmative(s) {
			t.Errorf("isAffirmative(%q) = false, want true", s)
		}
	}
	no := []string{"no", "n", "deny", "不", "稍等", "yes please", "", "  ", "okay?"}
	for _, s := range no {
		if isAffirmative(s) {
			t.Errorf("isAffirmative(%q) = true, want false", s)
		}
	}
}

func askEnv(t *testing.T) (*Server, *channel.Session, *drainTestAdapter, channel.InboundEvent) {
	t.Helper()
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	sess := &channel.Session{}
	ad := &drainTestAdapter{}
	ev := channel.InboundEvent{ChatID: "c1", MessageID: "m1", Text: "original"}
	return srv, sess, ad, ev
}

func TestChannelPermissionAsk_AffirmativeAllows(t *testing.T) {
	srv, sess, ad, ev := askEnv(t)
	ask := srv.channelPermissionAsk(sess, ad, ev)

	done := make(chan struct{})
	var allow, remember bool
	var err error
	go func() {
		allow, remember, err = ask(context.Background(), "terminal", map[string]any{"command": "sudo ls"})
		close(done)
	}()

	// Wait until the prompt was sent and the ask slot is armed.
	waitFor(t, func() bool { return len(ad.texts()) == 1 })
	if !strings.Contains(ad.texts()[0], "terminal") {
		t.Errorf("prompt %q should name the tool", ad.texts()[0])
	}
	if !strings.Contains(ad.texts()[0], "sudo ls") {
		t.Errorf("prompt %q must show the input being approved", ad.texts()[0])
	}
	if !sess.DeliverAskReply("c1", "", "允许") {
		t.Fatal("ask slot not armed when the prompt was already sent")
	}

	<-done
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !allow {
		t.Error("affirmative reply should allow")
	}
	if remember {
		t.Error("IM approvals must be one-shot (remember=false)")
	}
}

func TestChannelPermissionAsk_NonAffirmativeDenies(t *testing.T) {
	srv, sess, ad, ev := askEnv(t)
	ask := srv.channelPermissionAsk(sess, ad, ev)

	done := make(chan struct{})
	var allow bool
	go func() {
		allow, _, _ = ask(context.Background(), "terminal", nil)
		close(done)
	}()
	waitFor(t, func() bool { return len(ad.texts()) == 1 })
	sess.DeliverAskReply("c1", "", "先等等，我看看")
	<-done

	if allow {
		t.Error("non-affirmative reply must deny")
	}
}

func TestChannelPermissionAsk_ContextCancelDenies(t *testing.T) {
	srv, sess, ad, ev := askEnv(t)
	ask := srv.channelPermissionAsk(sess, ad, ev)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var allow bool
	var err error
	go func() {
		allow, _, err = ask(ctx, "terminal", nil)
		close(done)
	}()
	waitFor(t, func() bool { return len(ad.texts()) == 1 })
	cancel() // the /stop path cancels the turn ctx

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not return on context cancellation")
	}
	if allow || err == nil {
		t.Errorf("cancelled ask: allow=%v err=%v, want deny with error", allow, err)
	}
	if sess.DeliverAskReply("c1", "", "yes") {
		t.Error("ask slot must be released after cancellation")
	}
}

func TestChannelPermissionAsk_TimeoutDenies(t *testing.T) {
	old := channelAskTimeout
	channelAskTimeout = 50 * time.Millisecond
	t.Cleanup(func() { channelAskTimeout = old })

	srv, sess, ad, ev := askEnv(t)
	ask := srv.channelPermissionAsk(sess, ad, ev)

	allow, _, err := ask(context.Background(), "terminal", nil)
	if err != nil {
		t.Fatalf("timeout should deny without error, got %v", err)
	}
	if allow {
		t.Error("timeout must deny")
	}
	if len(ad.texts()) == 0 {
		t.Error("prompt was never sent")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestParseAskReply(t *testing.T) {
	q := tools.AskRequest{Question: "q", Options: []string{"Alpha", "Beta", "Gamma"}}
	multi := tools.AskRequest{Question: "q", Options: []string{"Alpha", "Beta", "Gamma"}, MultiSelect: true}

	cases := []struct {
		req     tools.AskRequest
		text    string
		choices []string
		custom  string
		cancel  bool
	}{
		{q, "2", []string{"Beta"}, "", false},
		{q, " 1 ", []string{"Alpha"}, "", false},
		{q, "beta", []string{"Beta"}, "", false},        // label match, case-insensitive
		{q, "do it my way", nil, "do it my way", false}, // free text
		{q, "9", nil, "9", false},                       // out of range → custom text
		{q, "", nil, "", true},
		{multi, "1,3", []string{"Alpha", "Gamma"}, "", false},
		{multi, "1、3", []string{"Alpha", "Gamma"}, "", false},
	}
	for _, c := range cases {
		got := parseAskReply(c.text, c.req)
		if got.Cancelled != c.cancel {
			t.Errorf("parse(%q) cancelled = %v, want %v", c.text, got.Cancelled, c.cancel)
			continue
		}
		if strings.Join(got.Choices, "|") != strings.Join(c.choices, "|") || got.Custom != c.custom {
			t.Errorf("parse(%q) = choices %v custom %q, want %v %q", c.text, got.Choices, got.Custom, c.choices, c.custom)
		}
	}
}

func TestChannelAsker_NumberPicksOption(t *testing.T) {
	srv, sess, ad, ev := askEnv(t)
	asker := srv.channelAsker(sess, ad, ev)

	done := make(chan tools.AskResponse, 1)
	go func() {
		res, _ := asker.Ask(context.Background(), tools.AskRequest{
			Question: "Deploy where?", Options: []string{"staging", "production"},
		})
		done <- res
	}()
	waitFor(t, func() bool { return len(ad.texts()) == 1 })
	prompt := ad.texts()[0]
	if !strings.Contains(prompt, "Deploy where?") || !strings.Contains(prompt, "1. staging") {
		t.Errorf("prompt %q must show the question and numbered options", prompt)
	}
	if !sess.DeliverAskReply("c1", "", "2") {
		t.Fatal("ask slot not armed")
	}
	res := <-done
	if len(res.Choices) != 1 || res.Choices[0] != "production" {
		t.Errorf("choices = %v, want [production]", res.Choices)
	}
}

func TestChannelAsker_TimeoutCancels(t *testing.T) {
	old := channelAskTimeout
	channelAskTimeout = 50 * time.Millisecond
	t.Cleanup(func() { channelAskTimeout = old })

	srv, sess, ad, ev := askEnv(t)
	_ = sess
	_ = ad
	asker := srv.channelAsker(sess, ad, ev)
	res, err := asker.Ask(context.Background(), tools.AskRequest{Question: "q", Options: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("timeout should cancel without error, got %v", err)
	}
	if !res.Cancelled {
		t.Error("timeout must report Cancelled")
	}
}
