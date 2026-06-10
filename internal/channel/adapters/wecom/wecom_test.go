package wecom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/gorilla/websocket"
)

// fakeWecom is a WebSocket server speaking the aibot frame protocol: it acks
// the subscribe (rejecting wrong secrets), pushes queued callbacks, and acks
// aibot_send_msg frames.
type fakeWecom struct {
	mu        sync.Mutex
	callbacks []map[string]any
	sent      []map[string]any
	srv       *httptest.Server
	wsURL     string
}

type rawFrame struct {
	Cmd     string `json:"cmd"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body map[string]any `json:"body"`
}

func newFakeWecom(t *testing.T) *fakeWecom {
	f := &fakeWecom{}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		f.runConn(t, conn)
	}))
	t.Cleanup(f.srv.Close)
	f.wsURL = "ws" + strings.TrimPrefix(f.srv.URL, "http")
	return f
}

func (f *fakeWecom) runConn(t *testing.T, conn *websocket.Conn) {
	for {
		var frame rawFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		switch frame.Cmd {
		case "aibot_subscribe":
			if frame.Body["secret"] != "good-secret" {
				conn.WriteJSON(map[string]any{
					"headers": map[string]string{"req_id": frame.Headers.ReqID},
					"errcode": 40001, "errmsg": "invalid secret",
				})
				continue
			}
			conn.WriteJSON(map[string]any{
				"headers": map[string]string{"req_id": frame.Headers.ReqID},
				"errcode": 0,
			})
			// Auth OK — push queued inbound callbacks.
			f.mu.Lock()
			cbs := f.callbacks
			f.mu.Unlock()
			for _, cb := range cbs {
				conn.WriteJSON(map[string]any{
					"cmd":     "aibot_msg_callback",
					"headers": map[string]string{"req_id": "cb_1"},
					"body":    cb,
				})
			}
		case "aibot_send_msg":
			f.mu.Lock()
			f.sent = append(f.sent, frame.Body)
			f.mu.Unlock()
			conn.WriteJSON(map[string]any{
				"headers": map[string]string{"req_id": frame.Headers.ReqID},
				"errcode": 0,
			})
		case "ping":
			conn.WriteJSON(map[string]any{
				"headers": map[string]string{"req_id": frame.Headers.ReqID},
				"errcode": 0,
			})
		}
	}
}

func textCallback(chatID, chatType, userID, content string) map[string]any {
	return map[string]any{
		"msgtype":  "text",
		"chatid":   chatID,
		"chattype": chatType,
		"msgid":    "msg-1",
		"from":     map[string]string{"userid": userID},
		"text":     map[string]string{"content": content},
	}
}

func newTestAdapter(t *testing.T, f *fakeWecom, extra map[string]any) *Adapter {
	cfg := channel.PlatformConfig{"bot_id": "aib123", "secret": "good-secret", "ws_url": f.wsURL}
	for k, v := range extra {
		cfg[k] = v
	}
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ad.(*Adapter)
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

func TestNew_RequiresCredentials(t *testing.T) {
	if _, err := New(channel.PlatformConfig{}); err == nil {
		t.Fatal("expected error for missing credentials")
	}
	if _, err := New(channel.PlatformConfig{"bot_id": "x"}); err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestStart_DeliversTextMessage(t *testing.T) {
	f := newFakeWecom(t)
	f.callbacks = []map[string]any{textCallback("chat-1", "single", "user-7", "hello octo")}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Platform != "wecom" || ev.ChatID != "chat-1" || ev.UserID != "user-7" ||
		ev.Text != "hello octo" || ev.ChatType != "direct" || ev.MessageID != "msg-1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestStart_GroupChatType(t *testing.T) {
	f := newFakeWecom(t)
	f.callbacks = []map[string]any{textCallback("chat-g", "group", "user-7", "hi")}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 || events[0].ChatType != "group" {
		t.Fatalf("expected group event, got %+v", events)
	}
}

func TestStart_AllowedUsersFilters(t *testing.T) {
	f := newFakeWecom(t)
	f.callbacks = []map[string]any{
		textCallback("c", "single", "stranger", "hi"),
		textCallback("c", "single", "friend", "yo"),
	}
	a := newTestAdapter(t, f, map[string]any{"allowed_users": "friend"})

	events := collectEvents(t, a, 1)
	if len(events) != 1 || events[0].UserID != "friend" {
		t.Fatalf("allowed_users not enforced: %+v", events)
	}
}

func TestStart_AuthFailureIsFatal(t *testing.T) {
	f := newFakeWecom(t)
	cfg := channel.PlatformConfig{"bot_id": "aib123", "secret": "wrong", "ws_url": f.wsURL}
	ad, _ := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := ad.Start(ctx, func(channel.InboundEvent) {})
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected auth error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("auth failure should abort without reconnect attempts")
	}
}

func TestSendText_WaitsForAck(t *testing.T) {
	f := newFakeWecom(t)
	a := newTestAdapter(t, f, nil)

	// Establish the connection first.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Start(ctx, func(channel.InboundEvent) {})

	deadline := time.Now().Add(2 * time.Second)
	for {
		res := a.SendText("chat-1", "**hello**", "")
		if res.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("send never succeeded: %+v", res)
		}
		time.Sleep(20 * time.Millisecond)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no aibot_send_msg frame received")
	}
	last := f.sent[len(f.sent)-1]
	if last["msgtype"] != "markdown" || last["chatid"] != "chat-1" {
		t.Fatalf("unexpected send body: %+v", last)
	}
}

func TestValidateConfig(t *testing.T) {
	a := &Adapter{}
	if errs := a.ValidateConfig(channel.PlatformConfig{}); len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %v", errs)
	}
	if errs := a.ValidateConfig(channel.PlatformConfig{"bot_id": "x", "secret": "y"}); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}
