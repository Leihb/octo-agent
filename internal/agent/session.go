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
	Messages  []Message `json:"messages"`

	// persisted is how many messages are already on disk. Save appends
	// Messages[persisted:]; it's not serialized.
	persisted int
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
	dir, err := sessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, s.ID+".jsonl"), nil
}

// sessionRecord is one JSONL line. Type discriminates the meta header from a
// message record.
type sessionRecord struct {
	Type      string    `json:"type"` // "meta" | "message"
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Model     string    `json:"model,omitempty"`
	System    string    `json:"system,omitempty"`
	Message   *Message  `json:"message,omitempty"`
}

func (s *Session) metaRecord() sessionRecord {
	return sessionRecord{Type: "meta", ID: s.ID, CreatedAt: s.CreatedAt, Model: s.Model, System: s.System}
}

func messageRecord(m Message) sessionRecord {
	mm := m
	return sessionRecord{Type: "message", Message: &mm}
}

// Save persists the session. The common case appends only the messages added
// since the last Save. When the in-memory history is shorter than what's on
// disk — which happens when compaction rewrites history into a summary — the
// file is rewritten from scratch instead (an infrequent O(n) event).
func (s *Session) Save() error {
	if s.persisted == 0 || len(s.Messages) < s.persisted {
		return s.rewriteAll()
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

// LoadSession reads ~/.octo/sessions/<id>.jsonl. id may be a bare session id,
// an id with a .jsonl/.json suffix, or an absolute path to a transcript file.
func LoadSession(id string) (*Session, error) {
	path, err := resolveSessionPath(id)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %q not found", id)
		}
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	s := &Session{}
	sc := bufio.NewScanner(f)
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
	s.persisted = len(s.Messages) // a resumed session continues appending
	return s, nil
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

// ToHistory converts the session's message slice into an agent History.
func (s *Session) ToHistory() *History {
	h := NewHistory()
	for _, m := range s.Messages {
		h.Append(m)
	}
	return h
}

// SyncFrom copies the current messages from h into the session.
// Call this before Save to flush the latest turns.
func (s *Session) SyncFrom(h *History) {
	s.Messages = h.Snapshot()
}
