package agent

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/trash"
)

// Session is a named conversation that persists to disk as a JSONL transcript
// (one record per line) under ~/.octo/sessions/<id>.jsonl. The first line is a
// meta record; each subsequent line is one message. This lets a turn be saved
// by APPENDING only its new messages rather than rewriting the whole file —
// the per-turn cost is O(new messages), not O(total history).
//
// "Last updated" is taken from the file's mtime rather than a stored field, so
// the append path never has to rewrite an earlier line.
type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Model     string    `json:"model"`
	System    string    `json:"system,omitempty"`
	Title     string    `json:"title,omitempty"`
	Source    string    `json:"source,omitempty"` // how the session was created: "" (manual) | "cron" | "channel" | "setup"
	// ModelConfig names the user-config model entry this session is bound to.
	// Empty means "the default entry at turn time" — also the value for every
	// session predating per-session model binding.
	ModelConfig string    `json:"model_config,omitempty"`
	Messages    []Message `json:"messages"`

	// Dir overrides the default ~/.octo/sessions location. Empty means use the
	// default. Not serialized — it's a runtime override.
	Dir string `json:"-"`

	// persisted is how many messages are already on disk. Save appends
	// Messages[persisted:]; it's not serialized.
	persisted int

	// forceRewrite is set when SyncFrom observes that the source History was
	// rewritten (compaction, repair, popLast): the on-disk prefix may no
	// longer match Messages, so the next Save must rewrite the whole file.
	// Cleared by a successful rewriteAll. Not serialized.
	forceRewrite bool
}

// NewSession creates a Session with an ID derived from the current time plus
// a random suffix: YYYYMMDD-HHMMSS-xxxxxxxx. The timestamp keeps IDs roughly
// sortable and human-readable; the 32-bit suffix removes same-second
// collisions (see B3).
func NewSession(model, system string) *Session {
	now := time.Now()
	return &Session{
		ID:        now.Format("20060102-150405") + "-" + randomSuffix(now),
		CreatedAt: now,
		Model:     model,
		System:    system,
	}
}

// randomSuffix returns 8 hex chars (32 bits) from crypto/rand, falling back to
// the nanosecond fraction if the system RNG is unavailable.
func randomSuffix(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", uint32(now.Nanosecond()))
	}
	return hex.EncodeToString(b[:])
}

// TurnCount returns the number of complete user+assistant turn pairs.
func (s *Session) TurnCount() int {
	return len(s.Messages) / 2
}

// UsedTools reports whether any assistant message in the session emitted
// at least one tool_use block. Used by the chat resume path to decide
// whether to auto-enable --tools — without that, a resumed session whose
// history contains tool_use blocks would be sent to the model with no
// tools array, and the model (seeing prior tool calls in the conversation
// but unable to make new structured ones) falls back to emitting tool
// calls as text. That looks like garbled XML to the user.
func (s *Session) UsedTools() bool {
	for _, m := range s.Messages {
		for _, b := range m.Blocks {
			if b.Type == "tool_use" {
				return true
			}
		}
	}
	return false
}

// EndsMidTurn reports whether the persisted transcript stops in the middle of
// a turn: the last message is a user message still awaiting a reply (the
// initiating input, a tool_result batch, or a mid-turn steer), or an
// assistant tool_use whose results never landed. A turn that finished — or
// was interrupted by the user — always ends on a plain assistant text message
// (finishInterrupted guarantees this), so a mid-turn tail means the process
// died with the turn in flight: crash, kill, power loss. Callers use it to
// warn the model that tool side effects from that turn may be unrecorded.
func (s *Session) EndsMidTurn() bool {
	if len(s.Messages) == 0 {
		return false
	}
	last := s.Messages[len(s.Messages)-1]
	switch last.Role {
	case RoleUser:
		return true
	case RoleAssistant:
		for _, b := range last.Blocks {
			if b.Type == "tool_use" {
				return true
			}
		}
	}
	return false
}

// sessionsDir returns (and creates if needed) ~/.octo/sessions.
func sessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("session: home dir: %w", err)
	}
	dir := filepath.Join(home, ".octo", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("session: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// SavePath returns the JSONL path where this session would be saved.
func (s *Session) SavePath() (string, error) {
	dir := s.Dir
	if dir == "" {
		var err error
		dir, err = sessionsDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, s.ID+".jsonl"), nil
}

// sessionRecord is one JSONL line. Type discriminates the meta header, a
// message record, and a title record. The title is appended on its own line
// (rather than rewriting the meta header) so the append-only path is preserved:
// it's generated after the first turn, by which point the meta line is already
// on disk. LoadSession takes the last title record as authoritative; rewriteAll
// also folds the title back into the meta header so a compacted file keeps it.
type sessionRecord struct {
	Type        string    `json:"type"` // "meta" | "message" | "title" | "model_config"
	ID          string    `json:"id,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	Model       string    `json:"model,omitempty"`
	System      string    `json:"system,omitempty"`
	Title       string    `json:"title,omitempty"`
	Source      string    `json:"source,omitempty"`
	ModelConfig string    `json:"model_config,omitempty"`
	Message     *Message  `json:"message,omitempty"`
}

func (s *Session) metaRecord() sessionRecord {
	return sessionRecord{Type: "meta", ID: s.ID, CreatedAt: s.CreatedAt, Model: s.Model, System: s.System, Title: s.Title, Source: s.Source, ModelConfig: s.ModelConfig}
}

func messageRecord(m Message) sessionRecord {
	mm := m
	return sessionRecord{Type: "message", Message: &mm}
}

// Save persists the session. The common case appends only the messages added
// since the last Save. The file is rewritten from scratch (an infrequent O(n)
// event) when the on-disk prefix can't be trusted: a history rewrite observed
// by SyncFrom (forceRewrite), or an in-memory list shorter than what's on disk
// — the length check is kept as a belt-and-braces fallback for callers that
// assign Messages directly instead of going through SyncFrom. With no new
// messages and a trusted prefix, Save is a no-op, so per-event callers (the
// server persists mid-turn progress on every agent event) don't touch the
// file at all between rounds.
func (s *Session) Save() error {
	if s.forceRewrite || s.persisted == 0 || len(s.Messages) < s.persisted {
		return s.rewriteAll()
	}
	if len(s.Messages) == s.persisted {
		return nil
	}
	return s.appendDelta()
}

// rewriteAll truncates the file and writes the meta record followed by every
// message. Used for the first save and after a history rewrite (compaction).
func (s *Session) rewriteAll() error {
	path, err := s.SavePath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	if err := enc.Encode(s.metaRecord()); err != nil {
		return fmt.Errorf("session: write meta: %w", err)
	}
	for _, m := range s.Messages {
		if err := enc.Encode(messageRecord(m)); err != nil {
			return fmt.Errorf("session: write message: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("session: flush %s: %w", path, err)
	}
	s.persisted = len(s.Messages)
	s.forceRewrite = false
	return nil
}

// appendDelta appends only the messages added since the last save.
func (s *Session) appendDelta() error {
	path, err := s.SavePath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, m := range s.Messages[s.persisted:] {
		if err := enc.Encode(messageRecord(m)); err != nil {
			return fmt.Errorf("session: append message: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("session: flush %s: %w", path, err)
	}
	s.persisted = len(s.Messages)
	return nil
}

// SetTitle records a short human-readable title for the session. It updates the
// in-memory field and, if the transcript already exists on disk, appends a title
// record so the title survives without rewriting the file. A fresh session whose
// file isn't written yet just carries the title until the first Save folds it
// into the meta header. Calling with an empty or unchanged title is a no-op.
func (s *Session) SetTitle(title string) error {
	title = strings.TrimSpace(title)
	if title == "" || title == s.Title {
		return nil
	}
	s.Title = title
	if s.persisted == 0 {
		// Nothing on disk yet; the next Save writes the title via metaRecord.
		return nil
	}
	if s.forceRewrite {
		// The file ends in an incomplete tail (crash mid-append) or its prefix
		// is stale (history rewrite): a raw append would corrupt it. Rewrite
		// instead — rewriteAll folds the title into the meta header.
		return s.rewriteAll()
	}
	path, err := s.SavePath()
	if err != nil {
		return err
	}
	// No O_CREATE: persisted > 0 means the transcript should already exist.
	// If it doesn't (session deleted meanwhile, or the path resolves
	// differently than when it was saved — e.g. an async title goroutine
	// outliving a test's HOME override), creating a file holding nothing but
	// a title record would leave an orphan "session" in list views. Fail
	// instead; a later turn retries title generation.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(sessionRecord{Type: "title", Title: title}); err != nil {
		return fmt.Errorf("session: append title: %w", err)
	}
	return nil
}

// SetModelConfig binds the session to a named config entry (empty name =
// the default entry) and records the entry's model string so the session
// displays and resumes on the right model. Like SetTitle, it appends a
// record when the transcript is already on disk so the binding survives
// without rewriting the file; a switch to the values already in place is a
// no-op.
func (s *Session) SetModelConfig(name, model string) error {
	if name == s.ModelConfig && (model == "" || model == s.Model) {
		return nil
	}
	s.ModelConfig = name
	if model != "" {
		s.Model = model
	}
	if s.persisted == 0 {
		// persisted == 0 means no MESSAGES on disk, not no file: a fresh
		// session saved before its first turn is a meta-only transcript, and
		// a load-modify-discard caller (the model-switch handler) would lose
		// the binding if we waited for "the next Save" on an object about to
		// be thrown away. Rewrite the existing file; a session with no file
		// yet just carries the binding until its first Save.
		if path, perr := s.SavePath(); perr == nil {
			if _, statErr := os.Stat(path); statErr == nil {
				return s.rewriteAll()
			}
		}
		return nil
	}
	if s.forceRewrite {
		// Stale or truncated on-disk prefix — fold into a full rewrite, same
		// reasoning as SetTitle.
		return s.rewriteAll()
	}
	path, err := s.SavePath()
	if err != nil {
		return err
	}
	// No O_CREATE — see SetTitle: never materialise an orphan transcript.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(sessionRecord{Type: "model_config", ModelConfig: name, Model: s.Model}); err != nil {
		return fmt.Errorf("session: append model_config: %w", err)
	}
	return nil
}

// DisplayTitle returns the label shown for the session in list views: the
// generated Title when present, otherwise a snippet of the first user message
// (so pre-title sessions and not-yet-titled ones are still recognisable), and
// finally "(untitled)" when there's nothing to show.
func (s *Session) DisplayTitle() string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return t
	}
	if snippet := firstUserSnippet(s.Messages); snippet != "" {
		return snippet
	}
	return "(untitled)"
}

// firstUserSnippet extracts a one-line preview from the first user message,
// skipping injected <system-reminder> blocks and tool-result turns, and
// truncating to a list-friendly width.
func firstUserSnippet(msgs []Message) string {
	const maxLen = 60
	for _, m := range msgs {
		if m.Role != RoleUser {
			continue
		}
		text := m.Content
		if text == "" {
			for _, b := range m.Blocks {
				if b.Type == "text" {
					text = b.Text
					break
				}
			}
		}
		text = StripSystemReminders(text)
		// Collapse to the first non-empty line.
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if r := []rune(line); len(r) > maxLen {
				return strings.TrimSpace(string(r[:maxLen-1])) + "…"
			}
			return line
		}
	}
	return ""
}

// StripSystemReminders removes <system-reminder>…</system-reminder> spans the
// harness injects into user turns (background-process completion notes,
// recalled memories, …). They are model-facing context, not user speech —
// strip them anywhere user text is rendered (session previews, the web
// transcript) so they don't leak into the UI.
func StripSystemReminders(s string) string {
	for {
		start := strings.Index(s, "<system-reminder>")
		if start < 0 {
			break
		}
		end := strings.Index(s, "</system-reminder>")
		if end < 0 || end < start {
			s = s[:start]
			break
		}
		s = s[:start] + s[end+len("</system-reminder>"):]
	}
	return s
}

// LoadSession reads ~/.octo/sessions/<id>.jsonl. id may be a bare session id,
// an id with a .jsonl/.json suffix, or an absolute path to a transcript file.
func LoadSession(id string) (*Session, error) {
	path, err := resolveSessionPath(id)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %q not found", id)
		}
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}

	// A complete record always ends in '\n' (json.Encoder writes it), so bytes
	// after the last newline are an incomplete tail — left by a crash mid-append
	// or by a writer the read is racing. Drop the tail instead of failing the
	// whole session: it is at most one message that was never fully committed.
	// The tail bytes are still in the file, though, so a plain append would
	// fuse them with the next record into one corrupt line — force the first
	// Save of this Session to rewrite the file instead.
	droppedTail := false
	if n := bytes.LastIndexByte(data, '\n'); n >= 0 {
		if n+1 < len(data) {
			droppedTail = true
		}
		data = data[:n+1]
	} else {
		droppedTail = len(data) > 0
		data = nil
	}

	s := &Session{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	// A single message (e.g. a large tool_result) can exceed the default 64 KB
	// line cap; allow up to 16 MB per line.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec sessionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("session: parse %s: %w", path, err)
		}
		switch rec.Type {
		case "meta":
			s.ID, s.CreatedAt, s.Model, s.System = rec.ID, rec.CreatedAt, rec.Model, rec.System
			s.Title = rec.Title // a compacted file carries the title in its meta header
			s.Source = rec.Source
			s.ModelConfig = rec.ModelConfig
		case "title":
			s.Title = rec.Title // last one wins
		case "model_config":
			s.ModelConfig = rec.ModelConfig // last one wins, like title
			if rec.Model != "" {
				s.Model = rec.Model
			}
		case "message":
			if rec.Message != nil {
				s.Messages = append(s.Messages, *rec.Message)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: read %s: %w", path, err)
	}

	if s.ID == "" {
		// Meta line was missing/corrupt — fall back to the filename stem.
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	rehydrateImageBlocks(s.Messages)
	s.persisted = len(s.Messages) // a resumed session continues appending
	s.forceRewrite = droppedTail
	return s, nil
}

// rehydrateImageBlocks reloads persisted image attachments (ImagePath) into
// memory so provider adapters can re-send them on resumed turns — image bytes
// are never serialised into the transcript itself. A block whose file is gone,
// or a legacy block saved without a path, degrades to a text placeholder: the
// anthropic adapter hard-errors on an image block with no data, which would
// otherwise make every later turn of the resumed session fail.
func rehydrateImageBlocks(msgs []Message) {
	for mi := range msgs {
		for bi := range msgs[mi].Blocks {
			b := &msgs[mi].Blocks[bi]
			if b.Type != "image" || b.Image != nil {
				continue
			}
			if b.ImagePath != "" {
				if data, err := os.ReadFile(b.ImagePath); err == nil {
					b.Image = &ImageData{MIMEType: imageMIMEFromPath(b.ImagePath), Data: data}
					continue
				}
			}
			name := "image"
			if b.ImagePath != "" {
				name = filepath.Base(b.ImagePath)
			}
			*b = NewTextBlock("[image attachment no longer available: " + name + "]")
		}
	}
}

// imageMIMEFromPath maps a file extension to its image MIME type, defaulting
// to JPEG (the web UI re-encodes every image upload as JPEG).
func imageMIMEFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/jpeg"
	}
}

// resolveSessionPath maps an id (bare, suffixed, or absolute path) to the
// transcript file path.
func resolveSessionPath(id string) (string, error) {
	if filepath.IsAbs(id) {
		return id, nil
	}
	id = strings.TrimSuffix(id, ".jsonl")
	id = strings.TrimSuffix(id, ".json")
	dir, err := sessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".jsonl"), nil
}

// SessionMTime returns the file mtime of the session whose id is given.
// Used by the C9 Phase 2 memory daemon to gate "is this session quiet
// long enough to safely run boundary extraction" — the chat path
// updates the session file on every turn, so a recent mtime means the
// user is still actively chatting and the daemon should defer.
func SessionMTime(id string) (time.Time, error) {
	p, err := resolveSessionPath(id)
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// ListSessions returns up to n most-recently-modified sessions from
// ~/.octo/sessions/, newest first (by file mtime).
func ListSessions(n int) ([]*Session, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("session: readdir %s: %w", dir, err)
	}

	type item struct {
		s   *Session
		mod time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		s, err := LoadSession(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // skip corrupt files
		}
		mod := s.CreatedAt
		if info, err := e.Info(); err == nil {
			mod = info.ModTime()
		}
		items = append(items, item{s: s, mod: mod})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].mod.After(items[j].mod)
	})

	out := make([]*Session, 0, len(items))
	for _, it := range items {
		out = append(out, it.s)
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// ShortID returns the 8-character abbreviation suitable for CLI display.
// It's the trailing hex suffix of the full ID — the random part — since the
// timestamp prefix repeats across same-second sessions and uniqueness comes
// from the suffix. Same idea as git's short SHA.
func (s *Session) ShortID() string { return shortSessionID(s.ID) }

func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// listSessionIDs returns every session ID on disk, newest first. Lexicographic
// reverse sort matches chronological order because IDs are timestamp-prefixed.
// Cheap — reads the directory listing, never opens the JSONL bodies.
func listSessionIDs() ([]string, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: readdir %s: %w", dir, err)
	}
	var ids []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(n, ".jsonl"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// ResolveSessionID maps a user-typed identifier to a full session ID.
// Accepted shapes:
//
//   - "last"                  → the most-recently-modified session
//   - the full session ID     → returned as-is (fast path; never walks the dir)
//   - any substring of an ID  → unique match required
//
// On zero matches returns a "no session matches" error; on multiple, an
// ambiguity error listing the candidates so the user can re-disambiguate.
// The returned ID is suitable for passing to LoadSession.
func ResolveSessionID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("session id is empty")
	}
	if input == "last" {
		sessions, err := ListSessions(1)
		if err != nil {
			return "", err
		}
		if len(sessions) == 0 {
			return "", fmt.Errorf("no saved sessions to resume")
		}
		return sessions[0].ID, nil
	}
	// Fast path: exact filename hit. Skips the directory walk when the
	// user pasted a full ID.
	if path, err := resolveSessionPath(input); err == nil {
		if _, err := os.Stat(path); err == nil {
			return strings.TrimSuffix(filepath.Base(path), ".jsonl"), nil
		}
	}
	ids, err := listSessionIDs()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, id := range ids {
		if strings.Contains(id, input) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session matches %q", input)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("ambiguous session %q matches %d sessions:\n  %s",
		input, len(matches), strings.Join(matches, "\n  "))
}

// DeleteSession removes the transcript file for the given session id. id may be
// a bare id or one with a .jsonl/.json suffix; absolute paths are rejected, and
// any id that would resolve outside the sessions directory (e.g. via "..") is
// refused so a caller-supplied id can't reach arbitrary files. A missing file
// is treated as success — deleting an already-gone session is not an error.
func DeleteSession(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("session id is empty")
	}
	if filepath.IsAbs(id) {
		return fmt.Errorf("session %q: absolute paths not allowed", id)
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(id, ".jsonl"), ".json")
	// Reject anything that isn't a plain filename stem (no separators, no "..").
	if stem != filepath.Base(stem) || strings.Contains(stem, "..") {
		return fmt.Errorf("invalid session id %q", id)
	}
	dir, err := sessionsDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, stem+".jsonl")
	if _, err := os.Stat(path); err == nil {
		if err := trash.Move(path, dir); err != nil {
			return fmt.Errorf("session: trash %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("session: stat %s: %w", path, err)
	}
	return nil
}

// ToHistory converts the session's message slice into an agent History.
func (s *Session) ToHistory() *History {
	h := NewHistory()
	for _, m := range s.Messages {
		h.Append(m)
	}
	return h
}

// SyncFrom copies the current messages from h into the session.
// Call this before Save to flush the latest turns. When the history was
// rewritten since the last sync (compaction, repair, popLast), the next Save
// rewrites the file instead of appending — see History.rewritten.
func (s *Session) SyncFrom(h *History) {
	s.Messages = h.Snapshot()
	if h.takeRewriteDirty() {
		s.forceRewrite = true
	}
}
