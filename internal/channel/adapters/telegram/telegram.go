// Package telegram implements the Telegram Bot API adapter.
//
// Transport: HTTPS long-poll via getUpdates — no public domain required.
// Auth: a single bot token obtained from @BotFather.
// Group rule: the bot only reacts when @-mentioned or replied to (matches
// the Feishu and DingTalk adapters).
//
// base_url is configurable to allow self-hosted Bot API servers
// (https://github.com/tdlib/telegram-bot-api), which is the practical
// escape hatch on networks where api.telegram.org is blocked.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
)

const (
	platformName   = "telegram"
	defaultBaseURL = "https://api.telegram.org"

	// longPollTimeout is how long the server holds getUpdates open.
	longPollTimeout = 25 * time.Second
	// pollHTTPTimeout must comfortably exceed the long-poll window so we
	// don't tear down healthy connections mid-poll.
	pollHTTPTimeout = longPollTimeout + 10*time.Second

	// maxMessageChars leaves a margin under Telegram's 4096-char cap.
	maxMessageChars = 4000
)

func init() {
	channel.Register(platformName, New)
}

// Config keys.
const (
	cfgBotToken     = "bot_token"
	cfgBaseURL      = "base_url"
	cfgParseMode    = "parse_mode"
	cfgAllowedUsers = "allowed_users"
)

// Adapter implements channel.Adapter for the Telegram Bot API.
type Adapter struct {
	token        string
	baseURL      string
	parseMode    string
	allowedUsers map[string]bool

	http     *http.Client
	pollHTTP *http.Client

	// Cached bot identity, used for the group @-mention check.
	botID       int64
	botUsername string
	mentionRe   *regexp.Regexp

	running bool
	cancel  context.CancelFunc
}

// New creates a Telegram adapter from platform config.
func New(cfg channel.PlatformConfig) (channel.Adapter, error) {
	token, _ := cfg[cfgBotToken].(string)
	if token == "" {
		return nil, fmt.Errorf("telegram: bot_token is required")
	}

	baseURL, _ := cfg[cfgBaseURL].(string)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	// Default Markdown; an explicit empty parse_mode disables formatting.
	parseMode := "Markdown"
	if v, ok := cfg[cfgParseMode].(string); ok {
		parseMode = v
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
		token:        token,
		baseURL:      baseURL,
		parseMode:    parseMode,
		allowedUsers: allowed,
		http:         &http.Client{Timeout: 30 * time.Second},
		pollHTTP:     &http.Client{Timeout: pollHTTPTimeout},
	}, nil
}

// Platform returns "telegram".
func (a *Adapter) Platform() string { return platformName }

// Start long-polls getUpdates and blocks until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	a.running = true
	ctx, a.cancel = context.WithCancel(ctx)
	defer func() { a.running = false }()

	// Bot identity is required to gate group mentions; without it group
	// messages are dropped (fail closed) rather than answered indiscriminately.
	if err := a.fetchBotIdentity(ctx); err != nil {
		return fmt.Errorf("telegram: getMe: %w", err)
	}
	log.Printf("[telegram] bot identity: @%s (id=%d)", a.botUsername, a.botID)
	log.Printf("[telegram] starting long-poll (base_url=%s)", a.baseURL)

	var offset int64
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		updates, err := a.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			consecutiveErrors++
			delay := 5 * time.Second
			if consecutiveErrors > 3 {
				delay = 30 * time.Second
			}
			log.Printf("[telegram] poll error: %v (retrying in %s)", err, delay)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			continue
		}
		consecutiveErrors = 0

		for _, upd := range updates {
			offset = upd.UpdateID + 1
			a.processUpdate(upd, onMessage)
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

// SendText sends a message, splitting content over Telegram's length cap into
// consecutive messages. Returns the message ID of the last chunk.
func (a *Adapter) SendText(chatID, text, replyTo string) channel.SendResult {
	chunks := splitMessage(text, maxMessageChars)
	if len(chunks) == 0 {
		return channel.SendResult{OK: true}
	}

	var lastID string
	for i, chunk := range chunks {
		params := map[string]any{
			"chat_id":                  chatID,
			"text":                     chunk,
			"disable_web_page_preview": true,
		}
		if a.parseMode != "" {
			params["parse_mode"] = a.parseMode
		}
		if replyTo != "" && i == 0 {
			params["reply_to_message_id"] = replyTo
		}

		msg, err := a.callMessage("sendMessage", params)
		if err != nil {
			// Markdown parse failures fall back to plain text — the usual cause
			// is unescaped Markdown reserved chars in the agent's output.
			if a.parseMode != "" && isParseError(err) {
				log.Printf("[telegram] parse_mode failed, retrying as plain text: %v", err)
				delete(params, "parse_mode")
				msg, err = a.callMessage("sendMessage", params)
			}
			if err != nil {
				return channel.SendResult{OK: false, Error: err.Error()}
			}
		}
		lastID = fmt.Sprintf("%d", msg.MessageID)
	}
	return channel.SendResult{OK: true, MessageID: lastID}
}

// SendFile is not yet implemented for Telegram (matches the Feishu and
// DingTalk adapters, which are text-only today).
func (a *Adapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	return channel.SendResult{OK: false, Error: "SendFile not yet implemented"}
}

// UpdateMessage edits a previously sent message in place.
func (a *Adapter) UpdateMessage(chatID, messageID, text string) bool {
	params := map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if a.parseMode != "" {
		params["parse_mode"] = a.parseMode
	}
	_, err := a.callMessage("editMessageText", params)
	if err != nil && a.parseMode != "" && isParseError(err) {
		delete(params, "parse_mode")
		_, err = a.callMessage("editMessageText", params)
	}
	if err != nil {
		log.Printf("[telegram] update_message failed: %v", err)
		return false
	}
	return true
}

// SupportsMessageUpdates returns true.
func (a *Adapter) SupportsMessageUpdates() bool { return true }

// SendTyping sends a "typing" chat action (auto-expires after ~5s client-side).
func (a *Adapter) SendTyping(chatID, contextToken string) error {
	_, err := a.call(nil, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, a.http)
	return err
}

// ValidateConfig checks required fields.
func (a *Adapter) ValidateConfig(cfg channel.PlatformConfig) []string {
	var errs []string
	if tok, _ := cfg[cfgBotToken].(string); tok == "" {
		errs = append(errs, "bot_token is required")
	}
	return errs
}

// ─── Bot API ────────────────────────────────────────────────────────────────

type apiError struct {
	Code        int
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram API error %d: %s", e.Code, e.Description)
}

func isParseError(err error) bool {
	ae, ok := err.(*apiError)
	if !ok {
		return false
	}
	d := strings.ToLower(ae.Description)
	return strings.Contains(d, "can't parse entities") || strings.Contains(d, "markdown")
}

// call POSTs JSON to /bot<TOKEN>/<method> and returns the raw `result`.
// A nil ctx means context.Background(); the long-poll path passes the Start
// context so Stop cancels an in-flight getUpdates promptly.
func (a *Adapter) call(ctx context.Context, method string, params map[string]any, client *http.Client) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b, _ := json.Marshal(params)
	url := fmt.Sprintf("%s/bot%s/%s", a.baseURL, a.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %w", err)
	}
	if !r.OK {
		return nil, &apiError{Code: r.ErrorCode, Description: r.Description}
	}
	return r.Result, nil
}

type tgMessage struct {
	MessageID int64 `json:"message_id"`
	Date      int64 `json:"date"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Text           string `json:"text"`
	Caption        string `json:"caption"`
	ReplyToMessage *struct {
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
	} `json:"reply_to_message"`
}

func (a *Adapter) callMessage(method string, params map[string]any) (*tgMessage, error) {
	raw, err := a.call(nil, method, params, a.http)
	if err != nil {
		return nil, err
	}
	var msg tgMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("invalid message in response: %w", err)
	}
	return &msg, nil
}

func (a *Adapter) fetchBotIdentity(ctx context.Context) error {
	raw, err := a.call(ctx, "getMe", map[string]any{}, a.http)
	if err != nil {
		return err
	}
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return err
	}
	a.botID = me.ID
	a.botUsername = me.Username
	if me.Username != "" {
		a.mentionRe = regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(me.Username) + `\b`)
	}
	return nil
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

func (a *Adapter) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	params := map[string]any{
		"timeout":         int(longPollTimeout.Seconds()),
		"allowed_updates": []string{"message"},
	}
	if offset > 0 {
		params["offset"] = offset
	}
	raw, err := a.call(ctx, "getUpdates", params, a.pollHTTP)
	if err != nil {
		return nil, err
	}
	var updates []tgUpdate
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("invalid updates in response: %w", err)
	}
	return updates, nil
}

// ─── Inbound ────────────────────────────────────────────────────────────────

func (a *Adapter) processUpdate(upd tgUpdate, onMessage func(channel.InboundEvent)) {
	msg := upd.Message
	if msg == nil || msg.Chat.ID == 0 || msg.From.ID == 0 {
		return
	}

	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"
	text := msg.Text

	if isGroup {
		if !a.groupMention(msg, text) {
			return
		}
		text = a.stripBotMention(text)
	}

	userID := fmt.Sprintf("%d", msg.From.ID)
	if len(a.allowedUsers) > 0 && !a.allowedUsers[userID] {
		return
	}

	if text == "" {
		text = msg.Caption
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	chatType := "direct"
	if isGroup {
		chatType = "group"
	}

	log.Printf("[telegram] msg from %s in %d (%s): %.80s", userID, msg.Chat.ID, msg.Chat.Type, text)
	onMessage(channel.InboundEvent{
		Type:      "message",
		Platform:  platformName,
		ChatID:    fmt.Sprintf("%d", msg.Chat.ID),
		UserID:    userID,
		Text:      text,
		MessageID: fmt.Sprintf("%d", msg.MessageID),
		ChatType:  chatType,
		Raw:       msg,
	})
}

// groupMention reports whether the bot should react to a group message:
// either the text @-mentions the bot's username, or the message replies to
// one of the bot's own messages. Fails closed when bot identity is unknown.
func (a *Adapter) groupMention(msg *tgMessage, text string) bool {
	if a.botID == 0 {
		return false
	}
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From.ID == a.botID {
		return true
	}
	return a.mentionRe != nil && a.mentionRe.MatchString(text)
}

func (a *Adapter) stripBotMention(text string) string {
	if a.mentionRe == nil {
		return text
	}
	return strings.TrimSpace(a.mentionRe.ReplaceAllString(text, ""))
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// splitMessage splits text at the length cap, preferring paragraph / line /
// space boundaries and hard-cutting as a last resort.
func splitMessage(text string, limit int) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}

	var chunks []string
	for len(runes) > limit {
		window := string(runes[:limit])
		cut := strings.LastIndex(window, "\n\n")
		if cut <= 0 {
			cut = strings.LastIndex(window, "\n")
		}
		if cut <= 0 {
			cut = strings.LastIndex(window, " ")
		}
		if cut <= 0 {
			cut = limit
		} else {
			// LastIndex returned a byte offset into window; convert to runes.
			cut = len([]rune(window[:cut]))
		}
		chunks = append(chunks, strings.TrimRight(string(runes[:cut]), " \n"))
		runes = []rune(strings.TrimLeft(string(runes[cut:]), " \n"))
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
