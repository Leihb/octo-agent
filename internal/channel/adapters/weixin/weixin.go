// Package weixin implements the Weixin (微信) iLink adapter.
//
// It uses the iLink protocol (https://ilinkai.weixin.qq.com) for QR login,
// long-poll message receiving, message sending, file download, and typing
// indicators. Features aligned with the Ruby archive/ruby implementation.
package weixin

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/Leihb/octo-agent/internal/channel/adapters/weixin/ilink"
)

const platformName = "weixin"

func init() {
	channel.Register(platformName, New)
}

const (
	cfgBaseURL    = "base_url"
	cfgCredPath   = "cred_path"
	cfgTimeoutSec = "timeout_sec"
	cfgToken      = "token"
	cfgAllowedUsr = "allowed_users"

	// Send queue tuning.
	flushCharThreshold = 400
	flushInterval      = 800 * time.Millisecond
	minSendInterval    = time.Second

	// Typing keepalive (matches @tencent-weixin/openclaw-weixin npm package).
	typingKeepaliveInterval = 5 * time.Second
	typingTicketTTL         = 24 * time.Hour

	// Error backoff.
	reconnectDelay        = 5 * time.Second
	maxBackoff            = 30 * time.Second
	sessionExpiredBackoff = 60 * time.Second

	// Max text chunk size (Unicode chars, per iLink recommendation).
	maxChunkChars = 2000
)

// sendQueue buffers outgoing text per chat, flushes on threshold/interval,
// throttles sends, and retries on rate-limit errors.
type sendQueue struct {
	client  *ilink.Client
	baseURL string
	token   string

	mu      sync.Mutex
	buffers map[string][]sendEntry
	lastAt  time.Time
	stopCh  chan struct{}
}

type sendEntry struct {
	text         string
	contextToken string
	at           time.Time
}

func newSendQueue(client *ilink.Client, baseURL, token string) *sendQueue {
	q := &sendQueue{
		client:  client,
		baseURL: baseURL,
		token:   token,
		buffers: make(map[string][]sendEntry),
		stopCh:  make(chan struct{}),
	}
	go q.loop()
	return q
}

func (q *sendQueue) enqueue(chatID, text, contextToken string) {
	q.mu.Lock()
	q.buffers[chatID] = append(q.buffers[chatID], sendEntry{text: text, contextToken: contextToken, at: time.Now()})
	q.mu.Unlock()
}

func (q *sendQueue) flush(chatID string) {
	q.mu.Lock()
	entries := q.buffers[chatID]
	delete(q.buffers, chatID)
	q.mu.Unlock()
	if len(entries) > 0 {
		q.sendBatch(chatID, entries)
	}
}

func (q *sendQueue) stop() {
	close(q.stopCh)
	// Drain remaining.
	q.mu.Lock()
	snapshot := q.buffers
	q.buffers = nil
	q.mu.Unlock()
	for chatID, entries := range snapshot {
		if len(entries) > 0 {
			q.sendBatch(chatID, entries)
		}
	}
}

func (q *sendQueue) loop() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
			q.drain()
		}
	}
}

func (q *sendQueue) drain() {
	now := time.Now()
	q.mu.Lock()
	ready := make(map[string][]sendEntry)
	for chatID, entries := range q.buffers {
		if len(entries) == 0 {
			continue
		}
		totalChars := 0
		for _, e := range entries {
			totalChars += len([]rune(e.text))
		}
		elapsed := now.Sub(entries[0].at)
		if totalChars >= flushCharThreshold || elapsed >= flushInterval {
			ready[chatID] = entries
			delete(q.buffers, chatID)
		}
	}
	q.mu.Unlock()

	for chatID, entries := range ready {
		q.sendBatch(chatID, entries)
	}
}

func (q *sendQueue) sendBatch(chatID string, entries []sendEntry) {
	if len(entries) == 0 {
		return
	}
	// Combine all texts.
	var parts []string
	for _, e := range entries {
		parts = append(parts, e.text)
	}
	combined := strings.Join(parts, "\n")
	// Use the last entry's context_token.
	ctoken := entries[len(entries)-1].contextToken

	chunks := smartChunkText(combined, maxChunkChars)
	for _, chunk := range chunks {
		q.throttle()
		q.sendWithRetry(chatID, chunk, ctoken)
	}
}

func (q *sendQueue) throttle() {
	q.mu.Lock()
	defer q.mu.Unlock()
	wait := minSendInterval - time.Since(q.lastAt)
	if wait > 0 {
		time.Sleep(wait)
	}
	q.lastAt = time.Now()
}

func (q *sendQueue) sendWithRetry(chatID, text, contextToken string) {
	retryDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, delay := range retryDelays {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := q.client.SendMessage(ctx, q.baseURL, q.token,
			ilink.BuildTextMessage(chatID, contextToken, text))
		cancel()
		if err == nil {
			return
		}
		apiErr, ok := err.(*ilink.APIError)
		if ok && apiErr.ErrCode == -2 && i < len(retryDelays)-1 {
			log.Printf("[weixin] rate-limited for %s, retry in %v (%d/%d)", chatID, delay, i+1, len(retryDelays))
			time.Sleep(delay)
			continue
		}
		log.Printf("[weixin] send failed for %s: %v", chatID, err)
		return
	}
}

// Adapter implements channel.Adapter for Weixin iLink.
type Adapter struct {
	baseURL  string
	credPath string
	client   *ilink.Client
	allowed  map[string]bool

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc

	bot           *ilinkBot
	sendQ         *sendQueue
	keepalives    map[string]context.CancelFunc
	keepalivesMu  sync.Mutex
	typingTickets map[string]typingTicket
	typingMu      sync.Mutex
}

type typingTicket struct {
	ticket   string
	cachedAt time.Time
}

type ilinkBot struct {
	client        *ilink.Client
	creds         *ilink.Credentials
	contextTokens sync.Map
	cursor        string
}

// New creates a Weixin adapter.
func New(cfg channel.PlatformConfig) (channel.Adapter, error) {
	baseURL, _ := cfg[cfgBaseURL].(string)
	if baseURL == "" {
		baseURL = ilink.DefaultBaseURL
	}
	credPath, _ := cfg[cfgCredPath].(string)
	if credPath == "" {
		credPath = ilink.DefaultCredPath()
	}
	timeoutSec, _ := cfg[cfgTimeoutSec].(int)
	if timeoutSec <= 0 {
		timeoutSec = 30
	}

	allowed := make(map[string]bool)
	if raw, ok := cfg[cfgAllowedUsr].(string); ok {
		for _, u := range strings.Split(raw, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				allowed[u] = true
			}
		}
	}

	client := ilink.NewClient()
	client.HTTP.Timeout = time.Duration(timeoutSec) * time.Second

	return &Adapter{
		baseURL:       strings.TrimSuffix(baseURL, "/"),
		credPath:      credPath,
		client:        client,
		allowed:       allowed,
		keepalives:    make(map[string]context.CancelFunc),
		typingTickets: make(map[string]typingTicket),
	}, nil
}

func (a *Adapter) Platform() string { return platformName }

// Start begins the long-poll receive loop.
func (a *Adapter) Start(ctx context.Context, onMessage func(channel.InboundEvent)) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("weixin: already running")
	}
	a.running = true
	ctx, a.cancel = context.WithCancel(ctx)
	a.mu.Unlock()

	defer func() { a.running = false }()

	creds, err := ilink.LoadCredentials(a.credPath)
	if err != nil || creds == nil {
		return fmt.Errorf("weixin: no credentials (run `octo channel login` first)")
	}

	a.bot = &ilinkBot{client: a.client, creds: creds}
	a.sendQ = newSendQueue(a.client, creds.BaseURL, creds.Token)

	consecutiveErrors := 0
	retryDelay := reconnectDelay

	log.Printf("[weixin] starting long-poll (base_url=%s)", creds.BaseURL)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updates, err := a.bot.client.GetUpdates(ctx, a.bot.creds.BaseURL, a.bot.creds.Token, a.bot.cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			apiErr, isAPI := err.(*ilink.APIError)
			if isAPI && apiErr.IsSessionExpired() {
				log.Printf("[weixin] session expired, backing off %v", sessionExpiredBackoff)
				time.Sleep(sessionExpiredBackoff)
				// Re-load credentials in case they were refreshed.
				if fresh, err := ilink.LoadCredentials(a.credPath); err == nil && fresh != nil {
					a.bot.creds = fresh
					a.sendQ.token = fresh.Token
					a.sendQ.baseURL = fresh.BaseURL
				}
				retryDelay = reconnectDelay
				consecutiveErrors = 0
				continue
			}

			consecutiveErrors++
			if consecutiveErrors > 3 {
				retryDelay = maxBackoff
			} else {
				retryDelay = time.Duration(consecutiveErrors) * reconnectDelay
			}
			log.Printf("[weixin] poll error: %v (retry in %v)", err, retryDelay)
			time.Sleep(retryDelay)
			continue
		}

		consecutiveErrors = 0
		retryDelay = reconnectDelay

		if updates.GetUpdatesBuf != "" {
			a.bot.cursor = updates.GetUpdatesBuf
		}

		for _, rawMsg := range updates.Msgs {
			var wire ilink.WireMessage
			if err := json.Unmarshal(rawMsg, &wire); err != nil {
				continue
			}
			a.handleInbound(&wire, onMessage)
		}
	}
}

// Stop shuts down the adapter.
func (a *Adapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return nil
	}
	a.running = false
	if a.cancel != nil {
		a.cancel()
	}
	if a.sendQ != nil {
		a.sendQ.stop()
	}
	a.stopAllKeepalives()
	return nil
}

// SendText enqueues text for buffered delivery.
func (a *Adapter) SendText(chatID, text, replyTo string) channel.SendResult {
	if a.bot == nil || a.bot.creds == nil {
		return a.sendStandalone(chatID, text)
	}

	var token string
	if ct, ok := a.bot.contextTokens.Load(chatID); ok {
		token = ct.(string)
	} else if a.credPath != "" {
		// Memory miss (e.g. the process restarted since the user last wrote):
		// fall back to the write-through store.
		token = loadContextTokens(contextStorePath(a.credPath))[chatID]
	}
	if token == "" {
		return channel.SendResult{OK: false, Error: "no context_token for user " + chatID}
	}

	plain := markdownToPlain(text)
	if plain == "" {
		return channel.SendResult{OK: true}
	}

	a.sendQ.enqueue(chatID, plain, token)
	return channel.SendResult{OK: true}
}

// sendStandalone delivers one message without Start(): credentials from disk
// and the chat's last context_token from the write-through store maintained
// by the receive loop. This is the path channel.SendOnce takes when the
// weixin channel isn't running in this process, so neither bot state nor the
// send queue exist. The send is synchronous; the caller owns retries.
func (a *Adapter) sendStandalone(chatID, text string) channel.SendResult {
	if a.credPath == "" {
		return channel.SendResult{OK: false, Error: "not logged in"}
	}
	creds, err := ilink.LoadCredentials(a.credPath)
	if err != nil || creds == nil {
		return channel.SendResult{OK: false, Error: "not logged in (run `octo channel login` first)"}
	}

	token := loadContextTokens(contextStorePath(a.credPath))[chatID]
	if token == "" {
		return channel.SendResult{OK: false, Error: "no stored context_token for " + chatID +
			" — the user must message the bot at least once while the weixin channel is running"}
	}

	plain := markdownToPlain(text)
	if plain == "" {
		return channel.SendResult{OK: true}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.client.SendMessage(ctx, creds.BaseURL, creds.Token,
		ilink.BuildTextMessage(chatID, token, plain)); err != nil {
		return channel.SendResult{OK: false, Error: err.Error()}
	}
	return channel.SendResult{OK: true}
}

// SendFile uploads and sends a file via CDN.
func (a *Adapter) SendFile(chatID, path, name, replyTo string) channel.SendResult {
	if a.bot == nil || a.bot.creds == nil {
		return channel.SendResult{OK: false, Error: "not logged in"}
	}

	ct, ok := a.bot.contextTokens.Load(chatID)
	if !ok {
		return channel.SendResult{OK: false, Error: "no context_token for user " + chatID}
	}

	// Flush pending text first (Ruby convention).
	a.sendQ.flush(chatID)

	data, err := os.ReadFile(path)
	if err != nil {
		return channel.SendResult{OK: false, Error: fmt.Sprintf("read file: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result, err := a.cdnUpload(ctx, chatID, data, name)
	if err != nil {
		log.Printf("[weixin] cdn upload %s failed: %v", name, err)
		return channel.SendResult{OK: false, Error: fmt.Sprintf("cdn upload: %v", err)}
	}

	items, err := a.buildMediaItems(result, name, data)
	if err != nil {
		return channel.SendResult{OK: false, Error: err.Error()}
	}

	msg := ilink.BuildMediaMessage(chatID, ct.(string), items)
	if err := a.client.SendMessage(ctx, a.bot.creds.BaseURL, a.bot.creds.Token, msg); err != nil {
		return channel.SendResult{OK: false, Error: fmt.Sprintf("send message: %v", err)}
	}
	return channel.SendResult{OK: true}
}

// cdnUpload uploads data to WeChat CDN and returns the media reference.
func (a *Adapter) cdnUpload(ctx context.Context, userID string, data []byte, fileName string) (*cdnUploadResult, error) {
	aesKey, err := ilink.GenerateAESKey()
	if err != nil {
		return nil, fmt.Errorf("generate aes key: %w", err)
	}
	ciphertext, err := ilink.EncryptAESECB(data, aesKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	var fileKeyBuf [16]byte
	if _, err := rand.Read(fileKeyBuf[:]); err != nil {
		return nil, fmt.Errorf("generate file key: %w", err)
	}
	fileKey := hex.EncodeToString(fileKeyBuf[:])

	rawMD5 := md5.Sum(data)
	rawMD5Hex := hex.EncodeToString(rawMD5[:])

	mediaType := mediaTypeForFile(fileName)

	uploadResp, err := a.client.GetUploadURL(ctx, a.bot.creds.BaseURL, a.bot.creds.Token, ilink.GetUploadURLRequest{
		FileKey:     fileKey,
		MediaType:   mediaType,
		ToUserID:    userID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5Hex,
		FileSize:    len(ciphertext),
		NoNeedThumb: true,
		AESKey:      ilink.EncodeAESKeyHex(aesKey),
	})
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", err)
	}
	if uploadResp.UploadParam == "" {
		return nil, fmt.Errorf("getuploadurl did not return upload_param")
	}

	cdnURL := ilink.BuildCDNUploadURL(ilink.CDNBaseURL, uploadResp.UploadParam, fileKey)
	encryptParam, err := a.client.UploadToCDN(ctx, cdnURL, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}

	return &cdnUploadResult{
		Media: ilink.CDNMedia{
			EncryptQueryParam: encryptParam,
			AESKey:            ilink.EncodeAESKeyBase64(aesKey),
			EncryptType:       1,
		},
		AESKey:            aesKey,
		EncryptedFileSize: len(ciphertext),
		MediaType:         mediaType,
	}, nil
}

// buildMediaItems constructs the item_list for a media message based on upload result.
func (a *Adapter) buildMediaItems(result *cdnUploadResult, fileName string, data []byte) ([]map[string]interface{}, error) {
	mediaMap := map[string]interface{}{
		"encrypt_query_param": result.Media.EncryptQueryParam,
		"aes_key":             result.Media.AESKey,
		"encrypt_type":        result.Media.EncryptType,
	}

	switch result.MediaType {
	case int(ilink.MediaImage):
		return []map[string]interface{}{
			{"type": ilink.ItemImage, "image_item": map[string]interface{}{
				"media":    mediaMap,
				"mid_size": result.EncryptedFileSize,
			}},
		}, nil
	case int(ilink.MediaVideo):
		return []map[string]interface{}{
			{"type": ilink.ItemVideo, "video_item": map[string]interface{}{
				"media":      mediaMap,
				"video_size": result.EncryptedFileSize,
			}},
		}, nil
	default:
		return []map[string]interface{}{
			{"type": ilink.ItemFile, "file_item": map[string]interface{}{
				"media":     mediaMap,
				"file_name": fileName,
				"len":       strconv.Itoa(len(data)),
			}},
		}, nil
	}
}

// cdnUploadResult holds the outcome of a CDN upload.
type cdnUploadResult struct {
	Media             ilink.CDNMedia
	AESKey            []byte
	EncryptedFileSize int
	MediaType         int
}

var (
	imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}
	videoExts = map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".mkv": true, ".avi": true}
)

func mediaTypeForFile(fileName string) int {
	ext := strings.ToLower(filepath.Ext(fileName))
	if imageExts[ext] {
		return int(ilink.MediaImage)
	}
	if videoExts[ext] {
		return int(ilink.MediaVideo)
	}
	return int(ilink.MediaFile)
}

// UpdateMessage is not supported.
func (a *Adapter) UpdateMessage(chatID, messageID, text string) bool { return false }

// SupportsMessageUpdates returns false.
func (a *Adapter) SupportsMessageUpdates() bool { return false }

// SendTyping triggers a typing indicator and starts keepalive.
func (a *Adapter) SendTyping(chatID, contextToken string) error {
	if a.bot == nil || a.bot.creds == nil {
		return fmt.Errorf("not logged in")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg, err := a.bot.client.GetConfig(ctx, a.bot.creds.BaseURL, a.bot.creds.Token, chatID, contextToken)
	if err != nil {
		return fmt.Errorf("getconfig: %w", err)
	}
	if cfg.TypingTicket == "" {
		return fmt.Errorf("no typing_ticket")
	}

	// Cache the ticket.
	a.typingMu.Lock()
	a.typingTickets[chatID] = typingTicket{ticket: cfg.TypingTicket, cachedAt: time.Now()}
	a.typingMu.Unlock()

	// Send initial typing.
	a.bot.client.SendTyping(ctx, a.bot.creds.BaseURL, a.bot.creds.Token, chatID, cfg.TypingTicket, 1)

	// Start keepalive goroutine.
	a.startTypingKeepalive(chatID, contextToken, cfg.TypingTicket)
	return nil
}

// ValidateConfig checks for token or credential file presence.
// Weixin is optional — returns nil when nothing is configured.
func (a *Adapter) ValidateConfig(cfg channel.PlatformConfig) []string {
	token, _ := cfg[cfgToken].(string)
	if token != "" {
		return nil
	}
	credPath, _ := cfg[cfgCredPath].(string)
	if credPath == "" {
		credPath = ilink.DefaultCredPath()
	}
	if _, err := os.Stat(credPath); err == nil {
		return nil
	}
	return nil // Weixin is optional; login is a separate setup step.
}

// ─── Inbound message handling ────────────────────────────────────────────

func (a *Adapter) handleInbound(wire *ilink.WireMessage, onMessage func(channel.InboundEvent)) {
	if wire.MessageType != ilink.MessageTypeUser {
		return
	}

	userID := wire.FromUserID
	if userID == "" || wire.ContextToken == "" {
		return
	}

	// allowed_users filter.
	if len(a.allowed) > 0 && !a.allowed[userID] {
		return
	}

	// Store context_token in memory and write it through to disk so other
	// processes (octo serve pushing task results) can send to this user too.
	if a.bot != nil {
		a.bot.contextTokens.Store(userID, wire.ContextToken)
	}
	if a.credPath != "" {
		saveContextToken(contextStorePath(a.credPath), userID, wire.ContextToken)
	}

	// Extract text and files.
	text := extractText(wire.ItemList)
	files := extractFiles(a.client, wire.ItemList)

	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return
	}

	log.Printf("[weixin] message from %s: %s", userID, truncate(text, 80))

	ev := channel.InboundEvent{
		Type:         "message",
		Platform:     platformName,
		ChatID:       userID,
		UserID:       userID,
		Text:         strings.TrimSpace(text),
		MessageID:    fmt.Sprintf("%d", wire.MessageID),
		ChatType:     "direct",
		ContextToken: wire.ContextToken,
		Raw:          wire,
	}

	// Attach files to event.
	for _, f := range files {
		ev.Files = append(ev.Files, channel.FileAttachment{
			Type:     f.ftype,
			Name:     f.name,
			Path:     f.path,
			MimeType: f.mime,
			DataURL:  f.dataURL,
		})
	}

	if onMessage != nil {
		onMessage(ev)
	}
}

// ─── Text extraction ─────────────────────────────────────────────────────

func extractText(items []ilink.MessageItem) string {
	var parts []string
	for _, item := range items {
		switch item.Type {
		case ilink.ItemText:
			if item.TextItem != nil {
				t := item.TextItem.Text
				// Prepend quoted message if present.
				if item.RefMsg != nil && item.RefMsg.Title != "" {
					refParts := []string{item.RefMsg.Title}
					if item.RefMsg.MessageItem != nil && item.RefMsg.MessageItem.TextItem != nil {
						refParts = append(refParts, item.RefMsg.MessageItem.TextItem.Text)
					}
					parts = append(parts, fmt.Sprintf("[引用: %s]", strings.Join(refParts, " | ")))
				}
				parts = append(parts, t)
			}
		case ilink.ItemVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				parts = append(parts, item.VoiceItem.Text)
			} else {
				parts = append(parts, "[语音]")
			}
		case ilink.ItemImage:
			parts = append(parts, "[图片]")
		case ilink.ItemFile:
			if item.FileItem != nil {
				parts = append(parts, fmt.Sprintf("[文件: %s]", item.FileItem.FileName))
			} else {
				parts = append(parts, "[文件]")
			}
		case ilink.ItemVideo:
			parts = append(parts, "[视频]")
		}
	}
	return strings.Join(parts, "\n")
}

// ─── File extraction and download ────────────────────────────────────────

type extractedFile struct {
	ftype   string
	name    string
	path    string
	mime    string
	dataURL string
}

func extractFiles(client *ilink.Client, items []ilink.MessageItem) []extractedFile {
	var files []extractedFile
	for _, item := range items {
		switch item.Type {
		case ilink.ItemImage:
			if item.ImageItem == nil || item.ImageItem.Media == nil {
				continue
			}
			img := item.ImageItem
			media := img.Media
			// Use top-level aeskey if present.
			if img.AESKey != "" && media.AESKey == "" {
				media = &ilink.CDNMedia{
					EncryptQueryParam: media.EncryptQueryParam,
					AESKey:            img.AESKey,
					EncryptType:       media.EncryptType,
					FullURL:           media.FullURL,
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			raw, err := client.DownloadMedia(ctx, media)
			cancel()
			if err != nil {
				log.Printf("[weixin] download image: %v", err)
				continue
			}
			mime := detectImageMIME(raw)
			dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
			files = append(files, extractedFile{ftype: "image", name: "image.jpg", mime: mime, dataURL: dataURL})

		case ilink.ItemFile:
			if item.FileItem == nil || item.FileItem.Media == nil {
				continue
			}
			fi := item.FileItem
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			raw, err := client.DownloadMedia(ctx, fi.Media)
			cancel()
			name := fi.FileName
			if name == "" {
				name = "attachment"
			}
			if err != nil {
				log.Printf("[weixin] download file %s: %v", name, err)
				files = append(files, extractedFile{ftype: "file", name: name})
				continue
			}
			// Save to disk.
			dir := filepath.Join(os.TempDir(), "octo-weixin")
			os.MkdirAll(dir, 0700)
			f, err := os.CreateTemp(dir, "wxfile-*"+filepath.Ext(name))
			if err != nil {
				files = append(files, extractedFile{ftype: "file", name: name})
				continue
			}
			f.Write(raw)
			f.Close()
			log.Printf("[weixin] file downloaded: %s (%d bytes)", f.Name(), len(raw))
			files = append(files, extractedFile{ftype: "file", name: name, path: f.Name()})

		case ilink.ItemVoice:
			if item.VoiceItem != nil {
				files = append(files, extractedFile{ftype: "voice", name: "voice.amr"})
			}
		case ilink.ItemVideo:
			if item.VideoItem != nil {
				files = append(files, extractedFile{ftype: "video", name: "video.mp4"})
			}
		}
	}
	return files
}

func detectImageMIME(raw []byte) string {
	if len(raw) < 4 {
		return "image/jpeg"
	}
	switch {
	case raw[0] == 0xFF && raw[1] == 0xD8:
		return "image/jpeg"
	case raw[0] == 0x89 && raw[1] == 0x50 && raw[2] == 0x4E && raw[3] == 0x47:
		return "image/png"
	case raw[0] == 0x47 && raw[1] == 0x49 && raw[2] == 0x46:
		return "image/gif"
	case raw[0] == 0x52 && raw[1] == 0x49 && raw[2] == 0x46 && raw[3] == 0x46:
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// ─── Typing keepalive ────────────────────────────────────────────────────

func (a *Adapter) startTypingKeepalive(userID, contextToken, ticket string) {
	a.stopTypingKeepalive(userID)

	ctx, cancel := context.WithCancel(context.Background())
	a.keepalivesMu.Lock()
	a.keepalives[userID] = cancel
	a.keepalivesMu.Unlock()

	go func() {
		ticker := time.NewTicker(typingKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Send status=2 (stop typing) on exit.
				sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
				a.bot.client.SendTyping(sctx, a.bot.creds.BaseURL, a.bot.creds.Token, userID, ticket, 2)
				scancel()
				return
			case <-ticker.C:
				sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
				a.bot.client.SendTyping(sctx, a.bot.creds.BaseURL, a.bot.creds.Token, userID, ticket, 1)
				scancel()
			}
		}
	}()
}

func (a *Adapter) stopTypingKeepalive(userID string) {
	a.keepalivesMu.Lock()
	cancel, ok := a.keepalives[userID]
	if ok {
		delete(a.keepalives, userID)
	}
	a.keepalivesMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (a *Adapter) stopAllKeepalives() {
	a.keepalivesMu.Lock()
	defer a.keepalivesMu.Unlock()
	for userID, cancel := range a.keepalives {
		cancel()
		delete(a.keepalives, userID)
	}
}

// ─── Text utilities ──────────────────────────────────────────────────────

// markdownToPlain strips Markdown syntax for WeChat (no rendering support).
func markdownToPlain(text string) string {
	// Strip code blocks: keep content, drop fences.
	re := regexp.MustCompile("(?s)```[^\n]*\n?(.*?)```")
	text = re.ReplaceAllString(text, "$1")

	// Images.
	text = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`).ReplaceAllString(text, "")

	// Links: keep text, drop URL.
	text = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`).ReplaceAllString(text, "$1")

	// Bold/italic markers.
	text = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`\*([^*]+)\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`__([^_]+)__`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`_([^_]+)_`).ReplaceAllString(text, "$1")

	// Headers.
	text = regexp.MustCompile(`(?m)^#+\s+`).ReplaceAllString(text, "")

	// Horizontal rules.
	text = regexp.MustCompile(`(?m)^[-*_]{3,}\s*$`).ReplaceAllString(text, "")

	return strings.TrimSpace(text)
}

// smartChunkText splits text into ≤N Unicode character chunks, preferring
// paragraph (double-newline), then line, then space boundaries.
func smartChunkText(text string, maxChars int) []string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxChars {
			chunks = append(chunks, string(runes))
			break
		}
		window := string(runes[:maxChars])
		cut := maxChars
		// Prefer \n\n, then \n, then space.
		for _, sep := range []string{"\n\n", "\n", " "} {
			if idx := strings.LastIndex(window, sep); idx > 0 {
				cut = idx + len(sep)
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
		// Trim leading whitespace of remainder.
		for len(runes) > 0 && (runes[0] == ' ' || runes[0] == '\n') {
			runes = runes[1:]
		}
	}
	return chunks
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
