package telegram

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
)

// fakeBotAPI is a minimal Telegram Bot API server. It serves getMe, one batch
// of updates, and records sendMessage calls.
type fakeBotAPI struct {
	mu        sync.Mutex
	updates   []tgUpdate
	served    bool
	sent      []map[string]any
	sendFail  int // fail the first N sendMessage calls with a parse error
	srv       *httptest.Server
	botUserID int64
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	f := &fakeBotAPI{botUserID: 42}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params)

		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			writeResult(w, map[string]any{"id": f.botUserID, "username": "octo_bot"})
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.served {
				// Hold like a long poll, then return empty.
				time.Sleep(50 * time.Millisecond)
				writeResult(w, []any{})
				return
			}
			f.served = true
			writeResult(w, f.updates)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sendFail > 0 && params["parse_mode"] != nil {
				f.sendFail--
				writeError(w, 400, "Bad Request: can't parse entities")
				return
			}
			f.sent = append(f.sent, params)
			writeResult(w, map[string]any{"message_id": 100 + len(f.sent)})
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			writeResult(w, map[string]any{"message_id": 7})
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			writeResult(w, true)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeResult(w http.ResponseWriter, result any) {
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func writeError(w http.ResponseWriter, code int, desc string) {
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": code, "description": desc})
}

func newTestAdapter(t *testing.T, f *fakeBotAPI, extra map[string]any) *Adapter {
	cfg := channel.PlatformConfig{"bot_token": "tok", "base_url": f.srv.URL}
	for k, v := range extra {
		cfg[k] = v
	}
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ad.(*Adapter)
}

func update(chatType string, chatID, fromID int64, text string) tgUpdate {
	var msg tgMessage
	msg.MessageID = 1
	msg.Chat.ID = chatID
	msg.Chat.Type = chatType
	msg.From.ID = fromID
	msg.Text = text
	return tgUpdate{UpdateID: 1, Message: &msg}
}

// collectEvents runs Start until one event arrives or the timeout elapses.
func collectEvents(t *testing.T, a *Adapter, want int) []channel.InboundEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	f := newFakeBotAPI(t)
	f.updates = []tgUpdate{update("private", 1001, 2002, "hello octo")}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Platform != "telegram" || ev.ChatID != "1001" || ev.UserID != "2002" ||
		ev.Text != "hello octo" || ev.ChatType != "direct" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestStart_GroupRequiresMention(t *testing.T) {
	f := newFakeBotAPI(t)
	f.updates = []tgUpdate{
		update("supergroup", 1, 2, "ambient chatter"),
		update("supergroup", 1, 2, "@octo_bot do the thing"),
	}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Text != "do the thing" {
		t.Fatalf("mention not stripped: %q", events[0].Text)
	}
}

func TestStart_GroupReplyToBot(t *testing.T) {
	f := newFakeBotAPI(t)
	upd := update("group", 1, 2, "continue please")
	upd.Message.ReplyToMessage = &struct {
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
	}{}
	upd.Message.ReplyToMessage.From.ID = f.botUserID
	f.updates = []tgUpdate{upd}
	a := newTestAdapter(t, f, nil)

	events := collectEvents(t, a, 1)
	if len(events) != 1 || events[0].ChatType != "group" {
		t.Fatalf("expected reply-to-bot group event, got %+v", events)
	}
}

func TestStart_AllowedUsersFilters(t *testing.T) {
	f := newFakeBotAPI(t)
	f.updates = []tgUpdate{
		update("private", 1, 999, "stranger"),
		update("private", 1, 2002, "friend"),
	}
	a := newTestAdapter(t, f, map[string]any{"allowed_users": "2002"})

	events := collectEvents(t, a, 1)
	if len(events) != 1 || events[0].Text != "friend" {
		t.Fatalf("allowed_users not enforced: %+v", events)
	}
}

func TestSendText_ParseErrorFallsBackToPlain(t *testing.T) {
	f := newFakeBotAPI(t)
	f.sendFail = 1
	a := newTestAdapter(t, f, nil)

	res := a.SendText("123", "broken *markdown", "")
	if !res.OK {
		t.Fatalf("expected fallback success, got %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || f.sent[0]["parse_mode"] != nil {
		t.Fatalf("expected one plain-text send, got %+v", f.sent)
	}
}

func TestSendText_SplitsLongMessages(t *testing.T) {
	f := newFakeBotAPI(t)
	a := newTestAdapter(t, f, nil)

	long := strings.Repeat("word ", 1200) // ~6000 chars
	res := a.SendText("123", long, "")
	if !res.OK {
		t.Fatalf("send failed: %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) < 2 {
		t.Fatalf("expected split into >=2 messages, got %d", len(f.sent))
	}
	for _, p := range f.sent {
		if len([]rune(p["text"].(string))) > maxMessageChars {
			t.Fatalf("chunk exceeds cap: %d chars", len(p["text"].(string)))
		}
	}
}

func TestSplitMessage_Boundaries(t *testing.T) {
	if got := splitMessage("", 10); got != nil {
		t.Fatalf("empty input: %v", got)
	}
	if got := splitMessage("short", 10); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short input: %v", got)
	}
	chunks := splitMessage("para one\n\npara two\n\npara three", 12)
	if len(chunks) != 3 {
		t.Fatalf("expected paragraph split, got %v", chunks)
	}
	// Hard cut when no boundary exists.
	chunks = splitMessage(strings.Repeat("x", 25), 10)
	if len(chunks) != 3 {
		t.Fatalf("expected hard cut into 3, got %v", chunks)
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

func TestUpdateMessage(t *testing.T) {
	f := newFakeBotAPI(t)
	a := newTestAdapter(t, f, nil)
	if !a.UpdateMessage("1", "7", "edited") {
		t.Fatal("expected update to succeed")
	}
	if !a.SupportsMessageUpdates() {
		t.Fatal("telegram supports message updates")
	}
}

func TestSendTyping(t *testing.T) {
	f := newFakeBotAPI(t)
	a := newTestAdapter(t, f, nil)
	if err := a.SendTyping("1", ""); err != nil {
		t.Fatal(err)
	}
}
