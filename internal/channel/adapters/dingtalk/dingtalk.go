// Package dingtalk implements the DingTalk (钉钉) Stream Mode adapter.
//
// DingTalk Stream Mode provides a persistent WebSocket connection for
// receiving bot messages and a webhook/OAPI for sending replies.
//
// Flow:
//  1. OAuth client_credentials → access_token
//  2. Open gateway connection → ticket
//  3. WebSocket to wss://wss.dingtalk.com/connect
//  4. Register for /v1.0/im/bot/messages/get topic
//  5. Receive frames → extract message → deliver to ChannelManager
//  6. Send replies via sessionWebhook (from inbound event) or OAPI
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/gorilla/websocket"
)

const (
	platformName = "dingtalk"
	apiBase      = "https://api.dingtalk.com"
	gatewayHost  = "wss://wss.dingtalk.com"
	msgTopic     = "/v1.0/im/bot/messages/get"
)

func init() {
	channel.Register(platformName, New)
}

// Config keys.
const (
	cfgClientID     = "client_id"
	cfgClientSecret = "client_secret"
	cfgAllowedUsers = "allowed_users"
)

// Adapter implements channel.Adapter for DingTalk Stream Mode.
type Adapter struct {
	clientID     string
	clientSecret string
	allowedUsers map[string]bool

	http        *http.Client
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.Mutex

	webhooks  map[string]webhookEntry
	webhookMu sync.Mutex

	running bool
	cancel  context.CancelFunc
}

type webhookEntry struct {
	URL         string
	ExpiresAtMs int64
}

// New creates a DingTalk adapter from platform config.
func New(cfg channel.PlatformConfig) (channel.Adapter, error) {
	cid, _ := cfg[cfgClientID].(string)
	secret, _ := cfg[cfgClientSecret].(string)
	if cid == "" || secret == "" {
		return nil, fmt.Errorf("dingtalk: client_id and client_secret are required")
	}

	allowed := make(map[string]bool)
	if raw, ok := cfg[cfgAllowedUsers].(string); ok {
		for _, u := range strings.Split(raw, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				allowed[u] = true
			}
		}
	}

	return &Adapter{
		clientID:     cid,
		clientSecret: secret,
		allowedUsers: allowed,
		http:         &http.Client{Timeout: 30 * time.Second},
		webhooks:     make(map[string]webhookEntry),
	}, nil
}

// Platform returns "dingtalk".
func (a *Adapter) Platform() string { return platformName }

// Start connects to DingTalk Stream Mode and blocks until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	a.running = true
	ctx, a.cancel = context.WithCancel(ctx)
	defer func() { a.running = false }()

	if err := a.refreshToken(); err != nil {
		return fmt.Errorf("dingtalk: token: %w", err)
	}

	ticket, err := a.openGateway()
	if err != nil {
		return fmt.Errorf("dingtalk: gateway: %w", err)
	}

	url := fmt.Sprintf("%s/connect?appKey=%s&ticket=%s", gatewayHost, a.clientID, ticket)
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dingtalk: connect: %w", err)
	}
	defer conn.Close()

	if err := a.register(conn); err != nil {
		return fmt.Errorf("dingtalk: register: %w", err)
	}

	log.Printf("[dingtalk] stream connected, listening")
	return a.readFrames(ctx, conn, onMessage)
}

// Stop gracefully shuts down.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}

// SendText sends a Markdown message via sessionWebhook.
func (a *Adapter) SendText(chatID, text, replyTo string) channel.SendResult {
	url := a.resolveWebhook(chatID)
	if url == "" {
		return channel.SendResult{OK: false, Error: "no valid webhook (expired or never received)"}
	}
	return a.sendWebhook(url, text)
}

// SendFile is not yet implemented for DingTalk.
func (a *Adapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	return channel.SendResult{OK: false, Error: "SendFile not yet implemented"}
}

// UpdateMessage — DingTalk does not support editing sent messages.
func (a *Adapter) UpdateMessage(chatID, messageID, text string) bool { return false }

// SupportsMessageUpdates returns false.
func (a *Adapter) SupportsMessageUpdates() bool { return false }

// SendTyping sends a typing indicator. DingTalk Stream Mode does not support this.
func (a *Adapter) SendTyping(chatID, contextToken string) error { return nil }

// ValidateConfig checks required fields.
func (a *Adapter) ValidateConfig(cfg channel.PlatformConfig) []string {
	var errs []string
	if cid, _ := cfg[cfgClientID].(string); cid == "" {
		errs = append(errs, "client_id is required")
	}
	if sec, _ := cfg[cfgClientSecret].(string); sec == "" {
		errs = append(errs, "client_secret is required")
	}
	return errs
}

// ─── OAuth ─────────────────────────────────────────────────────────────────

func (a *Adapter) refreshToken() error {
	body := map[string]string{"appKey": a.clientID, "appSecret": a.clientSecret}
	b, _ := json.Marshal(body)
	resp, err := a.http.Post(apiBase+"/v1.0/oauth2/accessToken", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.AccessToken == "" {
		return fmt.Errorf("empty access token in response")
	}

	a.tokenMu.Lock()
	a.accessToken = r.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(r.ExpireIn-60) * time.Second)
	a.tokenMu.Unlock()

	log.Printf("[dingtalk] token obtained, expires in %ds", r.ExpireIn)
	return nil
}

func (a *Adapter) getToken() string {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if time.Now().After(a.tokenExpiry) {
		// Best-effort refresh — don't block.
		go a.refreshToken()
	}
	return a.accessToken
}

// ─── Gateway ───────────────────────────────────────────────────────────────

func (a *Adapter) openGateway() (string, error) {
	body := map[string]string{
		"clientId":     a.clientID,
		"clientSecret": a.clientSecret,
	}
	b, _ := json.Marshal(body)
	resp, err := a.http.Post(apiBase+"/v1.0/gateway/connections/open", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var r struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.Ticket, nil
}

// ─── Stream frames ─────────────────────────────────────────────────────────

type streamFrame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Headers     map[string]string `json:"headers"`
	Data        json.RawMessage   `json:"data"`
}

func (a *Adapter) register(conn *websocket.Conn) error {
	frame := streamFrame{
		SpecVersion: "1.0",
		Type:        "register",
		Headers:     map[string]string{"clientId": a.clientID, "topic": msgTopic},
	}
	b, _ := json.Marshal(frame)
	return conn.WriteMessage(websocket.TextMessage, b)
}

func (a *Adapter) readFrames(ctx context.Context, conn *websocket.Conn, onMessage func(channel.InboundEvent)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		var frame streamFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		if frame.Type == "SYSTEM" {
			continue
		}
		if frame.Headers["topic"] != msgTopic {
			continue
		}
		a.handleInbound(frame.Data, onMessage)
	}
}

// ─── Inbound event handling ────────────────────────────────────────────────

type dtEvent struct {
	SenderStaffID             string `json:"senderStaffId"`
	SenderID                  string `json:"senderId"`
	ConversationID            string `json:"conversationId"`
	SessionWebhook            string `json:"sessionWebhook"`
	SessionWebhookExpiredTime int64  `json:"sessionWebhookExpiredTime"`
	ConversationType          string `json:"conversationType"`
	RobotCode                 string `json:"robotCode"`
	MsgID                     string `json:"msgId"`
	MsgType                   string `json:"msgtype"`
	ChatbotUserID             string `json:"chatbotUserId"`
	Text                      struct {
		Content string `json:"content"`
	} `json:"text"`
	Content struct {
		Content string `json:"content"`
	} `json:"content"`
	AtUsers []struct {
		DingtalkID string `json:"dingtalkId"`
	} `json:"atUsers"`
}

func (a *Adapter) handleInbound(raw json.RawMessage, onMessage func(channel.InboundEvent)) {
	var ev dtEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("[dingtalk] unmarshal event: %v", err)
		return
	}

	senderID := ev.SenderStaffID
	if senderID == "" {
		senderID = ev.SenderID
	}
	if senderID == "" {
		return
	}

	chatID := ev.ConversationID
	if chatID == "" {
		chatID = senderID
	}

	// Cache webhook.
	if ev.SessionWebhook != "" {
		a.webhookMu.Lock()
		a.webhooks[chatID] = webhookEntry{URL: ev.SessionWebhook, ExpiresAtMs: ev.SessionWebhookExpiredTime}
		a.webhookMu.Unlock()
	}

	// Group chat: only @-mention.
	if ev.ConversationType == "2" {
		mentioned := false
		for _, u := range ev.AtUsers {
			if u.DingtalkID == ev.ChatbotUserID {
				mentioned = true
				break
			}
		}
		content := ev.Text.Content
		if content == "" {
			content = ev.Content.Content
		}
		if !mentioned && !strings.Contains(content, "@") {
			return
		}
	}

	if len(a.allowedUsers) > 0 && !a.allowedUsers[senderID] {
		return
	}

	text := extractDTText(ev)
	if text == "" {
		return
	}

	ct := "direct"
	if ev.ConversationType == "2" {
		ct = "group"
	}

	onMessage(channel.InboundEvent{
		Type:      "message",
		Platform:  platformName,
		ChatID:    chatID,
		UserID:    senderID,
		Text:      text,
		MessageID: ev.MsgID,
		ChatType:  ct,
		Raw:       ev,
	})
}

func extractDTText(ev dtEvent) string {
	switch ev.MsgType {
	case "text":
		c := ev.Text.Content
		if idx := strings.Index(c, " "); idx > 0 && strings.HasPrefix(c, "@") {
			c = strings.TrimSpace(c[idx+1:])
		}
		return c
	case "richText":
		// RichText content is in Content.Content field.
		return ev.Content.Content
	default:
		return ""
	}
}

// ─── Send ──────────────────────────────────────────────────────────────────

func (a *Adapter) resolveWebhook(chatID string) string {
	a.webhookMu.Lock()
	defer a.webhookMu.Unlock()
	e, ok := a.webhooks[chatID]
	if !ok {
		return ""
	}
	if e.ExpiresAtMs > 0 && time.Now().UnixMilli()+5*60*1000 >= e.ExpiresAtMs {
		delete(a.webhooks, chatID)
		return ""
	}
	return e.URL
}

func (a *Adapter) sendWebhook(url, text string) channel.SendResult {
	body := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": "Octo",
			"text":  text,
		},
	}
	b, _ := json.Marshal(body)
	resp, err := a.http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return channel.SendResult{OK: false, Error: err.Error()}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return channel.SendResult{OK: true}
}
