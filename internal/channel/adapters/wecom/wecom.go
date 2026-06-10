// Package wecom implements the WeCom (Enterprise WeChat) intelligent-robot
// adapter.
//
// Protocol: plain JSON frames over a WebSocket long connection to
// wss://openws.work.weixin.qq.com.
//
// Frame format: { cmd, headers: { req_id }, body }
// Commands:
//
//	aibot_subscribe      — auth (client → server)
//	ping                 — heartbeat (client → server)
//	aibot_msg_callback   — inbound message (server → client)
//	aibot_send_msg       — send a message (client → server)
//
// An auth rejection (non-zero errcode acking the subscribe frame) is fatal —
// the credentials are wrong and reconnecting cannot help.
package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/gorilla/websocket"
)

const (
	platformName = "wecom"
	defaultWSURL = "wss://openws.work.weixin.qq.com"

	heartbeatInterval = 30 * time.Second
	reconnectDelay    = 5 * time.Second
	// readTimeout catches silent connection drops (NAT timeout, firewall
	// idle-kill) that never deliver a FIN/RST. The server pings every ~30s,
	// so 75s gives two missed pings before we give up.
	readTimeout = 75 * time.Second
	// ackTimeout bounds send_frame_and_wait round-trips.
	ackTimeout = 30 * time.Second
)

func init() {
	channel.Register(platformName, New)
}

// Config keys.
const (
	cfgBotID        = "bot_id"
	cfgSecret       = "secret"
	cfgAllowedUsers = "allowed_users"
	// cfgWSURL is a test seam, not user-facing config.
	cfgWSURL = "ws_url"
)

// Adapter implements channel.Adapter for WeCom intelligent robots.
type Adapter struct {
	botID        string
	secret       string
	wsURL        string
	allowedUsers map[string]bool

	conn   *websocket.Conn
	connMu sync.Mutex // guards conn pointer and writes

	pendingAcks map[string]chan *wsFrame
	ackMu       sync.Mutex

	cancel context.CancelFunc
}

// New creates a WeCom adapter from platform config.
func New(cfg channel.PlatformConfig) (channel.Adapter, error) {
	botID, _ := cfg[cfgBotID].(string)
	secret, _ := cfg[cfgSecret].(string)
	if botID == "" || secret == "" {
		return nil, fmt.Errorf("wecom: bot_id and secret are required")
	}

	wsURL, _ := cfg[cfgWSURL].(string)
	if wsURL == "" {
		wsURL = defaultWSURL
	}

	allowed := make(map[string]bool)
	if raw, ok := cfg[cfgAllowedUsers].(string); ok {
		for _, u := range strings.Split(raw, ",") {
			if u = strings.TrimSpace(u); u != "" {
				allowed[u] = true
			}
		}
	}

	return &Adapter{
		botID:        botID,
		secret:       secret,
		wsURL:        wsURL,
		allowedUsers: allowed,
		pendingAcks:  make(map[string]chan *wsFrame),
	}, nil
}

// Platform returns "wecom".
func (a *Adapter) Platform() string { return platformName }

// Start connects to the WeCom WebSocket and blocks until ctx is cancelled.
// Transient connection errors reconnect; an auth rejection aborts.
func (a *Adapter) Start(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	ctx, a.cancel = context.WithCancel(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		err := a.connectAndListen(ctx, onMessage)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if _, ok := err.(*authError); ok {
				return fmt.Errorf("wecom: %w", err)
			}
			log.Printf("[wecom] connection lost: %v (reconnecting in %s)", err, reconnectDelay)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reconnectDelay):
		}
	}
}

// Stop gracefully shuts down.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}

// SendText sends a markdown message via aibot_send_msg.
func (a *Adapter) SendText(chatID, text, replyTo string) channel.SendResult {
	_, err := a.sendFrameAndWait("aibot_send_msg", map[string]any{
		"chatid":   chatID,
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": text},
	})
	if err != nil {
		return channel.SendResult{OK: false, Error: err.Error()}
	}
	return channel.SendResult{OK: true}
}

// SendFile is not yet implemented for WeCom (matches the Feishu and DingTalk
// adapters, which are text-only today).
func (a *Adapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	return channel.SendResult{OK: false, Error: "SendFile not yet implemented"}
}

// UpdateMessage — WeCom does not support editing sent messages.
func (a *Adapter) UpdateMessage(chatID, messageID, text string) bool { return false }

// SupportsMessageUpdates returns false.
func (a *Adapter) SupportsMessageUpdates() bool { return false }

// SendTyping — the aibot protocol has no typing indicator.
func (a *Adapter) SendTyping(chatID, contextToken string) error { return nil }

// ValidateConfig checks required fields.
func (a *Adapter) ValidateConfig(cfg channel.PlatformConfig) []string {
	var errs []string
	if v, _ := cfg[cfgBotID].(string); v == "" {
		errs = append(errs, "bot_id is required")
	}
	if v, _ := cfg[cfgSecret].(string); v == "" {
		errs = append(errs, "secret is required")
	}
	return errs
}

// ─── WebSocket protocol ─────────────────────────────────────────────────────

// authError marks a credential rejection — reconnecting cannot help.
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

type wsFrame struct {
	Cmd     string `json:"cmd,omitempty"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	Errcode *int            `json:"errcode,omitempty"`
	Errmsg  string          `json:"errmsg,omitempty"`
}

// frameErrcode digs errcode out of the frame top level or body.
func (f *wsFrame) frameErrcode() (int, string) {
	if f.Errcode != nil {
		return *f.Errcode, f.Errmsg
	}
	var body struct {
		Errcode *int   `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	if len(f.Body) > 0 && json.Unmarshal(f.Body, &body) == nil && body.Errcode != nil {
		return *body.Errcode, body.Errmsg
	}
	return 0, ""
}

func (a *Adapter) connectAndListen(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	log.Printf("[wecom] connecting to %s", a.wsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.wsURL, nil)
	if err != nil {
		return err
	}

	a.connMu.Lock()
	a.conn = conn
	a.connMu.Unlock()
	defer func() {
		a.connMu.Lock()
		a.conn = nil
		a.connMu.Unlock()
		conn.Close()
	}()

	// Close the socket when ctx is cancelled so the blocking read returns.
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	log.Printf("[wecom] connected, authenticating (bot_id=%s)", a.botID)
	subReqID := reqID("subscribe")
	if err := a.writeFrame("aibot_subscribe", subReqID, map[string]string{
		"bot_id": a.botID, "secret": a.secret,
	}); err != nil {
		return err
	}

	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)
	go a.heartbeatLoop(conn, heartbeatStop)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		var frame wsFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			log.Printf("[wecom] failed to parse frame: %v", err)
			continue
		}

		// Dispatch acks to waiting senders first.
		if id := frame.Headers.ReqID; id != "" {
			a.ackMu.Lock()
			ch, ok := a.pendingAcks[id]
			a.ackMu.Unlock()
			if ok {
				select {
				case ch <- &frame:
				default:
				}
				continue
			}
		}

		switch frame.Cmd {
		case "aibot_msg_callback":
			a.handleInbound(frame.Body, onMessage)
		case "aibot_event_callback":
			// Ignored.
		case "":
			// Ack / error frame with no waiting sender.
			code, msg := frame.frameErrcode()
			if code != 0 {
				log.Printf("[wecom] error response: errcode=%d errmsg=%s req_id=%s", code, msg, frame.Headers.ReqID)
				if strings.HasPrefix(frame.Headers.ReqID, "subscribe_") {
					return &authError{fmt.Sprintf("authentication failed (errcode=%d): %s", code, msg)}
				}
			}
		default:
			log.Printf("[wecom] unknown cmd=%s", frame.Cmd)
		}
	}
}

func (a *Adapter) heartbeatLoop(conn *websocket.Conn, stop chan struct{}) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if err := a.writeFrame("ping", reqID("ping"), nil); err != nil {
			log.Printf("[wecom] ping failed: %v, forcing reconnect", err)
			// Close the socket so the blocked read returns and the caller
			// reconnects.
			conn.Close()
			return
		}
	}
}

func (a *Adapter) writeFrame(cmd, id string, body any) error {
	frame := map[string]any{
		"cmd":     cmd,
		"headers": map[string]string{"req_id": id},
	}
	if body != nil {
		frame["body"] = body
	}
	b, _ := json.Marshal(frame)

	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("not connected")
	}
	return a.conn.WriteMessage(websocket.TextMessage, b)
}

// sendFrameAndWait sends a frame and blocks until an ack with the same req_id
// arrives, the timeout fires, or the connection drops.
func (a *Adapter) sendFrameAndWait(cmd string, body any) (*wsFrame, error) {
	id := reqID(strings.TrimPrefix(cmd, "aibot_"))
	ch := make(chan *wsFrame, 1)

	a.ackMu.Lock()
	a.pendingAcks[id] = ch
	a.ackMu.Unlock()
	defer func() {
		a.ackMu.Lock()
		delete(a.pendingAcks, id)
		a.ackMu.Unlock()
	}()

	if err := a.writeFrame(cmd, id, body); err != nil {
		return nil, err
	}

	select {
	case frame := <-ch:
		if code, msg := frame.frameErrcode(); code != 0 {
			if msg == "" {
				msg = "unknown"
			}
			return nil, fmt.Errorf("wecom API error %d: %s (cmd=%s)", code, msg, cmd)
		}
		return frame, nil
	case <-time.After(ackTimeout):
		return nil, fmt.Errorf("timeout waiting for ack (req_id=%s, cmd=%s)", id, cmd)
	}
}

func reqID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// ─── Inbound ────────────────────────────────────────────────────────────────

type wcMessage struct {
	MsgType  string `json:"msgtype"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
	MsgID    string `json:"msgid"`
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
}

func (a *Adapter) handleInbound(body json.RawMessage, onMessage func(channel.InboundEvent)) {
	var msg wcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("[wecom] unmarshal callback: %v", err)
		return
	}
	if msg.MsgType != "text" {
		return
	}

	chatID := msg.ChatID
	if chatID == "" {
		chatID = msg.From.UserID
	}
	if chatID == "" {
		return
	}

	if len(a.allowedUsers) > 0 && !a.allowedUsers[msg.From.UserID] {
		return
	}

	text := strings.TrimSpace(msg.Text.Content)
	if text == "" {
		return
	}

	chatType := "direct"
	if msg.ChatType == "group" {
		chatType = "group"
	}

	log.Printf("[wecom] msg from %s in %s (%s): %.80s", msg.From.UserID, chatID, chatType, text)
	onMessage(channel.InboundEvent{
		Type:      "message",
		Platform:  platformName,
		ChatID:    chatID,
		UserID:    msg.From.UserID,
		Text:      text,
		MessageID: msg.MsgID,
		ChatType:  chatType,
		Raw:       msg,
	})
}
