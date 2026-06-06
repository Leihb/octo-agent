// Package feishu implements the Feishu (飞书) adapter.
//
// Feishu uses a WebSocket connection for receiving bot messages and a REST
// API for sending.
//
// Flow:
//  1. OAuth → tenant_access_token
//  2. POST /open-apis/ws/connection → WebSocket URL
//  3. Connect WebSocket, listen for im.message.receive_v1 events
//  4. Send replies via POST /open-apis/im/v1/messages
package feishu

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
	platformName  = "feishu"
	defaultDomain = "https://open.feishu.cn"
)

func init() {
	channel.Register(platformName, New)
}

// Config keys.
const (
	cfgAppID        = "app_id"
	cfgAppSecret    = "app_secret"
	cfgDomain       = "domain"
	cfgAllowedUsers = "allowed_users"
)

// Adapter implements channel.Adapter for Feishu.
type Adapter struct {
	appID        string
	appSecret    string
	domain       string
	allowedUsers map[string]bool

	http        *http.Client
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.Mutex

	botOpenID string
	running   bool
	cancel    context.CancelFunc
}

// New creates a Feishu adapter from platform config.
func New(cfg channel.PlatformConfig) (channel.Adapter, error) {
	appID, _ := cfg[cfgAppID].(string)
	secret, _ := cfg[cfgAppSecret].(string)
	if appID == "" || secret == "" {
		return nil, fmt.Errorf("feishu: app_id and app_secret are required")
	}

	domain, _ := cfg[cfgDomain].(string)
	if domain == "" {
		domain = defaultDomain
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
		appID:        appID,
		appSecret:    secret,
		domain:       domain,
		allowedUsers: allowed,
		http:         &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Platform returns "feishu".
func (a *Adapter) Platform() string { return platformName }

// Start connects to Feishu WebSocket and listens for messages.
func (a *Adapter) Start(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	a.running = true
	ctx, a.cancel = context.WithCancel(ctx)
	defer func() { a.running = false }()

	if err := a.refreshToken(); err != nil {
		return fmt.Errorf("feishu: token: %w", err)
	}

	wsURL, err := a.getWSEndpoint()
	if err != nil {
		return fmt.Errorf("feishu: ws endpoint: %w", err)
	}

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("feishu: connect: %w", err)
	}
	defer conn.Close()

	log.Printf("[feishu] connected to WebSocket")

	// Get bot's open_id once.
	a.botOpenID = a.getBotOpenID()

	return a.readEvents(ctx, conn, onMessage)
}

// Stop gracefully shuts down.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}

// SendText sends a text message.
func (a *Adapter) SendText(chatID, text, replyTo string) channel.SendResult {
	msgID, err := a.sendMessage(chatID, text)
	if err != nil {
		return channel.SendResult{OK: false, Error: err.Error()}
	}
	return channel.SendResult{OK: true, MessageID: msgID}
}

// SendFile is not yet implemented for Feishu.
func (a *Adapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	return channel.SendResult{OK: false, Error: "SendFile not yet implemented"}
}

// UpdateMessage edits an existing message.
func (a *Adapter) UpdateMessage(chatID, messageID, text string) bool {
	return a.updateMessage(messageID, text)
}

// SupportsMessageUpdates returns true.
func (a *Adapter) SupportsMessageUpdates() bool { return true }

// SendTyping — Feishu does not have a typing indicator API.
func (a *Adapter) SendTyping(chatID, contextToken string) error { return nil }

// ValidateConfig checks required fields.
func (a *Adapter) ValidateConfig(cfg channel.PlatformConfig) []string {
	var errs []string
	if id, _ := cfg[cfgAppID].(string); id == "" {
		errs = append(errs, "app_id is required")
	}
	if sec, _ := cfg[cfgAppSecret].(string); sec == "" {
		errs = append(errs, "app_secret is required")
	}
	return errs
}

// ─── OAuth ─────────────────────────────────────────────────────────────────

func (a *Adapter) refreshToken() error {
	body := map[string]string{"app_id": a.appID, "app_secret": a.appSecret}
	b, _ := json.Marshal(body)

	resp, err := a.http.Post(
		a.domain+"/open-apis/auth/v3/tenant_access_token/internal",
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Code != 0 {
		return fmt.Errorf("oauth error %d: %s", r.Code, r.Msg)
	}

	a.tokenMu.Lock()
	a.accessToken = r.TenantAccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(r.Expire-60) * time.Second)
	a.tokenMu.Unlock()

	log.Printf("[feishu] token obtained, expires in %ds", r.Expire)
	return nil
}

func (a *Adapter) getToken() string {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if time.Now().After(a.tokenExpiry) {
		go a.refreshToken()
	}
	return a.accessToken
}

// ─── WebSocket ─────────────────────────────────────────────────────────────

func (a *Adapter) getWSEndpoint() (string, error) {
	req, _ := http.NewRequest("POST", a.domain+"/open-apis/ws/connection", nil)
	req.Header.Set("Authorization", "Bearer "+a.getToken())
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var r struct {
		Code int `json:"code"`
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", fmt.Errorf("ws endpoint: code %d", r.Code)
	}
	return r.Data.URL, nil
}

func (a *Adapter) readEvents(ctx context.Context, conn *websocket.Conn, onMessage func(channel.InboundEvent)) error {
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

		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}

		switch envelope.Type {
		case "ping":
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			conn.WriteMessage(websocket.TextMessage, pong)
		case "message":
			a.handleEvent(envelope.Data, onMessage)
		}
	}
}

// ─── Bot info ──────────────────────────────────────────────────────────────

func (a *Adapter) getBotOpenID() string {
	req, _ := http.NewRequest("GET", a.domain+"/open-apis/bot/v3/info", nil)
	req.Header.Set("Authorization", "Bearer "+a.getToken())
	resp, err := a.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var r struct {
		Code int `json:"code"`
		Data struct {
			Bot struct {
				OpenID string `json:"open_id"`
			} `json:"bot"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return ""
	}
	return r.Data.Bot.OpenID
}

// ─── Message handling ──────────────────────────────────────────────────────

type fsEvent struct {
	Schema string `json:"schema"`
	Header struct {
		EventType string `json:"event_type"`
	} `json:"header"`
	Event struct {
		Message struct {
			MessageID   string `json:"message_id"`
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
	} `json:"event"`
}

type msgContent struct {
	Text string `json:"text"`
}

func (a *Adapter) handleEvent(raw json.RawMessage, onMessage func(channel.InboundEvent)) {
	var ev fsEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("[feishu] unmarshal event: %v", err)
		return
	}

	if ev.Header.EventType != "im.message.receive_v1" {
		return
	}

	msg := ev.Event.Message
	chatType := "direct"
	if msg.ChatType == "group" {
		chatType = "group"
	}

	senderID := ev.Event.Sender.SenderID.OpenID
	if senderID == "" {
		return
	}

	// Group chat: @-mention check.
	if chatType == "group" {
		if a.botOpenID == "" || !strings.Contains(msg.Content, a.botOpenID) {
			return
		}
	}

	if len(a.allowedUsers) > 0 && !a.allowedUsers[senderID] {
		return
	}

	var content msgContent
	json.Unmarshal([]byte(msg.Content), &content)

	text := strings.TrimSpace(content.Text)
	if text == "" {
		return
	}

	// Strip @-mentions.
	text = stripAtMentions(text)

	onMessage(channel.InboundEvent{
		Type:      "message",
		Platform:  platformName,
		ChatID:    msg.ChatID,
		UserID:    senderID,
		Text:      text,
		MessageID: msg.MessageID,
		ChatType:  chatType,
		Raw:       ev,
	})
}

func stripAtMentions(text string) string {
	// Remove Feishu @-mention format: @name or <at>...</at>
	for {
		start := strings.Index(text, "<at ")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "</at>")
		if end < 0 {
			break
		}
		text = text[:start] + text[start+end+5:]
	}
	text = strings.TrimSpace(text)
	// Remove leading @name patterns.
	for strings.HasPrefix(text, "@") {
		idx := strings.Index(text, " ")
		if idx < 0 {
			break
		}
		text = strings.TrimSpace(text[idx+1:])
	}
	return text
}

// ─── Send ──────────────────────────────────────────────────────────────────

func (a *Adapter) sendMessage(chatID, text string) (string, error) {
	body := map[string]any{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    map[string]string{"text": text},
	}
	b, _ := json.Marshal(body)

	url := a.domain + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+a.getToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	respBytes, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(respBytes, &r); err != nil {
		return "", fmt.Errorf("send: %s", string(respBytes))
	}
	if r.Code != 0 {
		return "", fmt.Errorf("send %d: %s", r.Code, r.Msg)
	}
	return r.Data.MessageID, nil
}

func (a *Adapter) updateMessage(messageID, text string) bool {
	body := map[string]any{
		"content": map[string]string{"text": text},
	}
	b, _ := json.Marshal(body)

	url := a.domain + "/open-apis/im/v1/messages/" + messageID
	req, _ := http.NewRequest("PATCH", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+a.getToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var r struct {
		Code int `json:"code"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Code == 0
}
