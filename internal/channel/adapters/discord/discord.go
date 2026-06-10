// Package discord implements the Discord bot adapter.
//
// Receiving: Gateway WebSocket v10 (JSON encoding, no compression) with
// identify, heartbeat, and resume handling.
// Sending: REST API v10. Discord requires a User-Agent of the form
// "DiscordBot ($url, $versionNumber)" — requests with an invalid UA may be
// blocked at Cloudflare.
//
// Group rule: in guild channels the bot only reacts when @-mentioned
// (matches the other adapters); DMs always go through.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/gorilla/websocket"
)

const (
	platformName      = "discord"
	defaultAPIBase    = "https://discord.com/api/v10"
	defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

	// GUILDS | GUILD_MESSAGES | GUILD_MESSAGE_REACTIONS | DIRECT_MESSAGES | MESSAGE_CONTENT
	gatewayIntents = (1 << 0) | (1 << 9) | (1 << 10) | (1 << 12) | (1 << 15)

	reconnectDelay = 5 * time.Second
	// readTimeout bounds a silent connection: heartbeat ACKs arrive at least
	// every heartbeat interval (~41s), so 90s means two missed beats.
	readTimeout = 90 * time.Second
)

// fatalCloseCodes are gateway close codes that mean reconnecting cannot help
// (bad token, invalid intents, …). See Discord docs "Gateway close event codes".
var fatalCloseCodes = map[int]bool{4004: true, 4010: true, 4011: true, 4012: true, 4013: true, 4014: true}

func init() {
	channel.Register(platformName, New)
}

// Config keys.
const (
	cfgBotToken     = "bot_token"
	cfgAllowedUsers = "allowed_users"
	// cfgAPIBase / cfgGatewayURL are test seams, not user-facing config.
	cfgAPIBase    = "api_base"
	cfgGatewayURL = "gateway_url"
)

// Adapter implements channel.Adapter for Discord.
type Adapter struct {
	token        string
	allowedUsers map[string]bool
	apiBase      string
	gatewayURL   string

	http *http.Client

	// Bot identity from /users/@me, used to gate guild mentions and to
	// ignore our own messages.
	botUserID string
	mentionRe *regexp.Regexp

	// Resume state.
	sessionID        string
	resumeGatewayURL string
	lastSeq          int64
	seqMu            sync.Mutex

	cancel context.CancelFunc
}

// New creates a Discord adapter from platform config.
func New(cfg channel.PlatformConfig) (channel.Adapter, error) {
	token, _ := cfg[cfgBotToken].(string)
	if token == "" {
		return nil, fmt.Errorf("discord: bot_token is required")
	}

	allowed := make(map[string]bool)
	if raw, ok := cfg[cfgAllowedUsers].(string); ok {
		for _, u := range strings.Split(raw, ",") {
			if u = strings.TrimSpace(u); u != "" {
				allowed[u] = true
			}
		}
	}

	apiBase, _ := cfg[cfgAPIBase].(string)
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	gatewayURL, _ := cfg[cfgGatewayURL].(string)
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}

	return &Adapter{
		token:        token,
		allowedUsers: allowed,
		apiBase:      strings.TrimSuffix(apiBase, "/"),
		gatewayURL:   gatewayURL,
		http:         &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Platform returns "discord".
func (a *Adapter) Platform() string { return platformName }

// Start connects to the Gateway and blocks until ctx is cancelled. Transient
// connection errors reconnect (resuming the session when possible); fatal
// close codes (bad token, invalid intents) abort.
func (a *Adapter) Start(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	ctx, a.cancel = context.WithCancel(ctx)

	me, err := a.usersMe()
	if err != nil {
		return fmt.Errorf("discord: /users/@me: %w", err)
	}
	a.botUserID = me.ID
	a.mentionRe = regexp.MustCompile(`<@!?` + regexp.QuoteMeta(me.ID) + `>`)
	log.Printf("[discord] authenticated as %s (id=%s)", me.Username, me.ID)

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
			var fatal *fatalGatewayError
			if errors.As(err, &fatal) {
				return fmt.Errorf("discord: %w", err)
			}
			log.Printf("[discord-gateway] connection lost: %v (reconnecting in %s)", err, reconnectDelay)
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

// SendText sends a message via the REST API.
func (a *Adapter) SendText(chatID, text, replyTo string) channel.SendResult {
	payload := map[string]any{"content": text}
	if replyTo != "" {
		payload["message_reference"] = map[string]string{"message_id": replyTo}
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := a.rest(http.MethodPost, fmt.Sprintf("/channels/%s/messages", chatID), payload, &msg); err != nil {
		return channel.SendResult{OK: false, Error: err.Error()}
	}
	return channel.SendResult{OK: true, MessageID: msg.ID}
}

// SendFile is not yet implemented for Discord (matches the Feishu and
// DingTalk adapters, which are text-only today).
func (a *Adapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	return channel.SendResult{OK: false, Error: "SendFile not yet implemented"}
}

// UpdateMessage edits an existing message.
func (a *Adapter) UpdateMessage(chatID, messageID, text string) bool {
	err := a.rest(http.MethodPatch, fmt.Sprintf("/channels/%s/messages/%s", chatID, messageID),
		map[string]any{"content": text}, nil)
	if err != nil {
		log.Printf("[discord] update_message failed: %v", err)
		return false
	}
	return true
}

// SupportsMessageUpdates returns true.
func (a *Adapter) SupportsMessageUpdates() bool { return true }

// SendTyping triggers the "is typing…" indicator (expires after ~10s).
func (a *Adapter) SendTyping(chatID, contextToken string) error {
	return a.rest(http.MethodPost, fmt.Sprintf("/channels/%s/typing", chatID), nil, nil)
}

// ValidateConfig checks required fields.
func (a *Adapter) ValidateConfig(cfg channel.PlatformConfig) []string {
	var errs []string
	if tok, _ := cfg[cfgBotToken].(string); tok == "" {
		errs = append(errs, "bot_token is required")
	}
	return errs
}

// ─── REST ───────────────────────────────────────────────────────────────────

func userAgent() string {
	return "DiscordBot (https://github.com/Leihb/octo-agent, 1.0)"
}

func (a *Adapter) rest(method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.apiBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+a.token)
	req.Header.Set("User-Agent", userAgent())
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("discord API %d: %s", resp.StatusCode, e.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type dcUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

func (a *Adapter) usersMe() (*dcUser, error) {
	var me dcUser
	if err := a.rest(http.MethodGet, "/users/@me", nil, &me); err != nil {
		return nil, err
	}
	if me.ID == "" {
		return nil, fmt.Errorf("empty response from /users/@me")
	}
	return &me, nil
}

// ─── Gateway ────────────────────────────────────────────────────────────────

// fatalGatewayError marks close codes where reconnecting cannot help.
type fatalGatewayError struct {
	code   int
	reason string
}

func (e *fatalGatewayError) Error() string {
	return fmt.Sprintf("gateway rejected connection (code=%d): %s", e.code, e.reason)
}

type gatewayPayload struct {
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d"`
	Seq  int64           `json:"s"`
	Type string          `json:"t"`
}

// connectAndListen runs one gateway connection: hello → identify/resume →
// dispatch loop with heartbeats. Returns on any connection error; the caller
// reconnects unless the error is fatal.
func (a *Adapter) connectAndListen(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	url := a.gatewayURL
	if a.sessionID != "" && a.resumeGatewayURL != "" {
		url = a.resumeGatewayURL + "/?v=10&encoding=json"
	}
	log.Printf("[discord-gateway] connecting (resume=%v)", a.sessionID != "")

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Close the socket when ctx is cancelled so the blocking read returns.
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	var writeMu sync.Mutex
	send := func(payload any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, _ := json.Marshal(payload)
		return conn.WriteMessage(websocket.TextMessage, b)
	}

	var heartbeatAcked atomic.Bool
	heartbeatAcked.Store(true)
	var heartbeatStop chan struct{}
	defer func() {
		if heartbeatStop != nil {
			close(heartbeatStop)
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ce, ok := err.(*websocket.CloseError); ok && fatalCloseCodes[ce.Code] {
				return &fatalGatewayError{code: ce.Code, reason: ce.Text}
			}
			return err
		}

		var p gatewayPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.Seq != 0 {
			a.seqMu.Lock()
			a.lastSeq = p.Seq
			a.seqMu.Unlock()
		}

		switch p.Op {
		case 10: // Hello
			var hello struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			}
			if err := json.Unmarshal(p.Data, &hello); err != nil || hello.HeartbeatInterval <= 0 {
				return fmt.Errorf("invalid hello payload")
			}
			interval := time.Duration(hello.HeartbeatInterval) * time.Millisecond
			log.Printf("[discord-gateway] hello, heartbeat_interval=%s", interval)

			if heartbeatStop != nil {
				close(heartbeatStop)
			}
			heartbeatStop = make(chan struct{})
			go func(stop chan struct{}) {
				// First beat after interval*jitter, per the gateway docs.
				t := time.NewTimer(time.Duration(float64(interval) * rand.Float64()))
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
					}
					if !heartbeatAcked.Load() {
						log.Printf("[discord-gateway] missed heartbeat ack, forcing reconnect")
						conn.Close()
						return
					}
					heartbeatAcked.Store(false)
					a.seqMu.Lock()
					seq := a.lastSeq
					a.seqMu.Unlock()
					if err := send(map[string]any{"op": 1, "d": seq}); err != nil {
						conn.Close()
						return
					}
					t.Reset(interval)
				}
			}(heartbeatStop)

			if a.sessionID != "" && a.resumeGatewayURL != "" {
				a.seqMu.Lock()
				seq := a.lastSeq
				a.seqMu.Unlock()
				log.Printf("[discord-gateway] sending Resume (session=%s seq=%d)", a.sessionID, seq)
				if err := send(map[string]any{"op": 6, "d": map[string]any{
					"token": a.token, "session_id": a.sessionID, "seq": seq,
				}}); err != nil {
					return err
				}
			} else {
				log.Printf("[discord-gateway] sending Identify (intents=%d)", gatewayIntents)
				if err := send(map[string]any{"op": 2, "d": map[string]any{
					"token":   a.token,
					"intents": gatewayIntents,
					"properties": map[string]string{
						"os": "octo", "browser": "octo-agent", "device": "octo-agent",
					},
				}}); err != nil {
					return err
				}
			}

		case 0: // Dispatch
			a.handleDispatch(p.Type, p.Data, onMessage)

		case 1: // Heartbeat request
			a.seqMu.Lock()
			seq := a.lastSeq
			a.seqMu.Unlock()
			_ = send(map[string]any{"op": 1, "d": seq})

		case 7: // Reconnect
			log.Printf("[discord-gateway] server requested reconnect")
			return fmt.Errorf("server requested reconnect")

		case 9: // Invalid session
			var resumable bool
			_ = json.Unmarshal(p.Data, &resumable)
			log.Printf("[discord-gateway] invalid session (resumable=%v)", resumable)
			if !resumable {
				a.sessionID = ""
				a.resumeGatewayURL = ""
				a.seqMu.Lock()
				a.lastSeq = 0
				a.seqMu.Unlock()
			}
			return fmt.Errorf("invalid session")

		case 11: // Heartbeat ACK
			heartbeatAcked.Store(true)
		}
	}
}

// ─── Dispatch ───────────────────────────────────────────────────────────────

type dcMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Content   string `json:"content"`
	Author    dcUser `json:"author"`
	Mentions  []struct {
		ID string `json:"id"`
	} `json:"mentions"`
}

func (a *Adapter) handleDispatch(typ string, data json.RawMessage, onMessage func(channel.InboundEvent)) {
	switch typ {
	case "READY":
		var ready struct {
			SessionID        string `json:"session_id"`
			ResumeGatewayURL string `json:"resume_gateway_url"`
			User             dcUser `json:"user"`
		}
		if err := json.Unmarshal(data, &ready); err == nil {
			a.sessionID = ready.SessionID
			a.resumeGatewayURL = ready.ResumeGatewayURL
			log.Printf("[discord-gateway] READY as %s, session=%s", ready.User.Username, ready.SessionID)
		}
	case "RESUMED":
		log.Printf("[discord-gateway] RESUMED session=%s", a.sessionID)
	case "MESSAGE_CREATE":
		var msg dcMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		a.handleMessage(&msg, onMessage)
	}
}

func (a *Adapter) handleMessage(msg *dcMessage, onMessage func(channel.InboundEvent)) {
	if msg.Author.Bot || msg.Author.ID == a.botUserID || msg.ChannelID == "" {
		return
	}

	chatType := "direct"
	if msg.GuildID != "" {
		chatType = "group"
		mentioned := false
		for _, m := range msg.Mentions {
			if m.ID == a.botUserID {
				mentioned = true
				break
			}
		}
		if !mentioned {
			return
		}
	}

	if len(a.allowedUsers) > 0 && !a.allowedUsers[msg.Author.ID] {
		return
	}

	text := msg.Content
	if a.mentionRe != nil {
		text = a.mentionRe.ReplaceAllString(text, "")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	log.Printf("[discord] msg from %s in %s (%s): %.80s", msg.Author.ID, msg.ChannelID, chatType, text)
	onMessage(channel.InboundEvent{
		Type:      "message",
		Platform:  platformName,
		ChatID:    msg.ChannelID,
		UserID:    msg.Author.ID,
		Text:      text,
		MessageID: msg.ID,
		ChatType:  chatType,
		Raw:       msg,
	})
}
