package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/gorilla/websocket"
)

// fakeDiscord serves /users/@me + message REST endpoints and a gateway
// WebSocket that runs hello → identify → READY → one MESSAGE_CREATE per
// queued message.
type fakeDiscord struct {
	mu       sync.Mutex
	sent     []map[string]any
	edited   []map[string]any
	messages []dcMessage
	srv      *httptest.Server
	wsURL    string
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{}
	upgrader := websocket.Upgrader{}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gateway":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			f.runGateway(t, conn)
		case r.URL.Path == "/users/@me":
			if r.Header.Get("Authorization") != "Bot tok" {
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]any{"message": "401: Unauthorized"})
				return
			}
			if !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
				t.Errorf("missing DiscordBot User-Agent: %q", r.Header.Get("User-Agent"))
			}
			json.NewEncoder(w).Encode(dcUser{ID: "bot-1", Username: "octo"})
		case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost:
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.sent = append(f.sent, p)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"id": "m-1"})
		case r.Method == http.MethodPatch:
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.edited = append(f.edited, p)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"id": "m-1"})
		case strings.HasSuffix(r.URL.Path, "/typing"):
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.wsURL = "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/gateway"
	return f
}

func (f *fakeDiscord) runGateway(t *testing.T, conn *websocket.Conn) {
	send := func(op int, typ string, d any) {
		b, _ := json.Marshal(d)
		payload := map[string]any{"op": op, "d": json.RawMessage(b)}
		if typ != "" {
			payload["t"] = typ
			payload["s"] = 1
		}
		conn.WriteJSON(payload)
	}

	send(10, "", map[string]any{"heartbeat_interval": 45000})

	// Expect Identify.
	var identify gatewayPayload
	if err := conn.ReadJSON(&identify); err != nil || identify.Op != 2 {
		t.Errorf("expected identify (op 2), got %+v err=%v", identify, err)
		return
	}

	send(0, "READY", map[string]any{
		"session_id":         "sess-1",
		"resume_gateway_url": "",
		"user":               dcUser{ID: "bot-1", Username: "octo"},
	})

	f.mu.Lock()
	msgs := f.messages
	f.mu.Unlock()
	for _, m := range msgs {
		send(0, "MESSAGE_CREATE", m)
	}

	// Hold the connection open; respond to heartbeats until the client leaves.
	for {
		var p gatewayPayload
		if err := conn.ReadJSON(&p); err != nil {
			return
		}
		if p.Op == 1 {
			send(11, "", nil)
		}
	}
}

func newTestAdapter(t *testing.T, f *fakeDiscord, extra map[string]any) *Adapter {
	cfg := channel.PlatformConfig{
		"bot_token":   "tok",
		"api_base":    f.srv.URL,
		"gateway_url": f.wsURL,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ad.(*Adapter)
}

func dm(id, fromID, content string) dcMessage {
	return dcMessage{ID: id, ChannelID: "chan-1", Content: content, Author: dcUser{ID: fromID}}
}

func collectEvents(t *testing.T, a *Adapter, want int) []channel.InboundEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var mu sync.Mutex
	var events []channel.InboundEvent
	done := make(chan struct{})
	go func() {
		_ = a.Start(ctx, func(ev channel.InboundEvent) {
			mu.Lock()
			events = append(events, ev)
			if len(events) >= want {
				cancel()
			}
			mu.Unlock()
		})
		close(done)
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return events
}

func TestNew_RequiresToken(t *testing.T) {
	if _, err := New(channel.PlatformConfig{}); err == nil {
		t.Fatal("expected error for missing bot_token")
	}
}

func TestStart_DeliversDirectMessage(t *testing.T) {
	f := newFakeDiscord(t)
	f.messages = []dcMessage{dm("m-9", "user-7", "hello octo")}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Platform != "discord" || ev.ChatID != "chan-1" || ev.UserID != "user-7" ||
		ev.Text != "hello octo" || ev.ChatType != "direct" || ev.MessageID != "m-9" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestStart_GuildRequiresMention(t *testing.T) {
	f := newFakeDiscord(t)
	plain := dm("m-1", "user-7", "ambient chatter")
	plain.GuildID = "g-1"
	mentioned := dm("m-2", "user-7", "<@bot-1> do the thing")
	mentioned.GuildID = "g-1"
	mentioned.Mentions = []struct {
		ID string `json:"id"`
	}{{ID: "bot-1"}}
	f.messages = []dcMessage{plain, mentioned}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Text != "do the thing" || events[0].ChatType != "group" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

func TestStart_IgnoresBotsAndSelf(t *testing.T) {
	f := newFakeDiscord(t)
	fromBot := dm("m-1", "other-bot", "beep")
	fromBot.Author.Bot = true
	fromSelf := dm("m-2", "bot-1", "echo")
	human := dm("m-3", "user-7", "real message")
	f.messages = []dcMessage{fromBot, fromSelf, human}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 || events[0].Text != "real message" {
		t.Fatalf("bot/self messages not filtered: %+v", events)
	}
}

func TestStart_AllowedUsersFilters(t *testing.T) {
	f := newFakeDiscord(t)
	f.messages = []dcMessage{dm("m-1", "stranger", "hi"), dm("m-2", "friend", "yo")}
	a := newTestAdapter(t, f, map[string]any{"allowed_users": "friend"})

	events := collectEvents(t, a, 1)
	if len(events) != 1 || events[0].UserID != "friend" {
		t.Fatalf("allowed_users not enforced: %+v", events)
	}
}

func TestStart_BadTokenFailsFast(t *testing.T) {
	f := newFakeDiscord(t)
	cfg := channel.PlatformConfig{"bot_token": "wrong", "api_base": f.srv.URL, "gateway_url": f.wsURL}
	ad, _ := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := ad.Start(ctx, func(channel.InboundEvent) {})
	if err == nil || !strings.Contains(err.Error(), "users/@me") {
		t.Fatalf("expected /users/@me auth error, got %v", err)
	}
}

func TestSendText(t *testing.T) {
	f := newFakeDiscord(t)
	a := newTestAdapter(t, f, nil)

	res := a.SendText("chan-1", "hello", "m-0")
	if !res.OK || res.MessageID != "m-1" {
		t.Fatalf("send failed: %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent[0]["content"] != "hello" {
		t.Fatalf("unexpected payload: %+v", f.sent[0])
	}
	if f.sent[0]["message_reference"] == nil {
		t.Fatal("expected message_reference for reply")
	}
}

func TestUpdateMessageAndTyping(t *testing.T) {
	f := newFakeDiscord(t)
	a := newTestAdapter(t, f, nil)

	if !a.UpdateMessage("chan-1", "m-1", "edited") {
		t.Fatal("expected edit to succeed")
	}
	if !a.SupportsMessageUpdates() {
		t.Fatal("discord supports message updates")
	}
	if err := a.SendTyping("chan-1", ""); err != nil {
		t.Fatal(err)
	}
}

func TestValidateConfig(t *testing.T) {
	a := &Adapter{}
	if errs := a.ValidateConfig(channel.PlatformConfig{}); len(errs) != 1 {
		t.Fatalf("expected 1 error, got %v", errs)
	}
	if errs := a.ValidateConfig(channel.PlatformConfig{"bot_token": "x"}); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}
