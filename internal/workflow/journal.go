package workflow

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// scriptHash returns a hex SHA-256 of the user script. Used to validate that a
// resume_from journal was created from the same script as the current run.
func scriptHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

// NewRunID returns a unique run ID for a new workflow journal.
// Format: wf-YYYYMMDD-HHMMSS-xxxxxxxx.
func NewRunID() string {
	now := time.Now()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		b = [4]byte{
			byte(now.Nanosecond()),
			byte(now.Nanosecond() >> 8),
			byte(now.Nanosecond() >> 16),
			byte(now.Nanosecond() >> 24),
		}
	}
	return "wf-" + now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// journalsDir returns (and creates if needed) ~/.octo/workflow-journals.
func journalsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("workflow: home dir: %w", err)
	}
	dir := filepath.Join(home, ".octo", "workflow-journals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("workflow: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// JournalEntry records one completed agent() call.
type JournalEntry struct {
	Seq          int    `json:"seq"` // 0-based call index (tok-1)
	Prompt       string `json:"prompt"`
	Reply        string `json:"reply"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	ErrMsg       string `json:"error,omitempty"`
}

// Journal appends completed agent() results to a JSONL file so a workflow can
// be resumed after a partial run. Each entry is flushed to disk immediately so
// a crash mid-run leaves a consistent record of all completed calls.
// Journal is safe for concurrent Append calls from multiple goroutines.
type Journal struct {
	mu  sync.Mutex
	f   *os.File
	buf *bufio.Writer
	enc *json.Encoder
}

// journalFileMeta is the first record in a journal JSONL file.
type journalFileMeta struct {
	Type       string    `json:"type"` // "meta"
	RunID      string    `json:"run_id"`
	ScriptHash string    `json:"script_hash"`
	CreatedAt  time.Time `json:"created_at"`
}

// journalFileEntry is a data record in a journal JSONL file.
type journalFileEntry struct {
	Type         string `json:"type"` // "agent"
	Seq          int    `json:"seq"`
	Prompt       string `json:"prompt"`
	Reply        string `json:"reply"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	ErrMsg       string `json:"error,omitempty"`
}

// CreateJournal creates a new journal file at dir/runID.jsonl and writes the
// meta header. The caller must Close it when done.
func CreateJournal(dir, runID, hash string) (*Journal, error) {
	path := filepath.Join(dir, runID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("workflow: create journal %s: %w", path, err)
	}
	buf := bufio.NewWriter(f)
	enc := json.NewEncoder(buf)
	j := &Journal{f: f, buf: buf, enc: enc}
	meta := journalFileMeta{
		Type:       "meta",
		RunID:      runID,
		ScriptHash: hash,
		CreatedAt:  time.Now(),
	}
	if err := enc.Encode(meta); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workflow: journal meta: %w", err)
	}
	if err := buf.Flush(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workflow: journal flush: %w", err)
	}
	return j, nil
}

// Append writes one completed agent() call to the journal and flushes to disk.
// Safe for concurrent calls from multiple goroutines.
func (j *Journal) Append(e JournalEntry) error {
	rec := journalFileEntry{
		Type:         "agent",
		Seq:          e.Seq,
		Prompt:       e.Prompt,
		Reply:        e.Reply,
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
		ErrMsg:       e.ErrMsg,
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.enc.Encode(rec); err != nil {
		return err
	}
	return j.buf.Flush()
}

// Close flushes and closes the underlying file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.buf.Flush()
	return j.f.Close()
}

// LoadJournal reads dir/runID.jsonl and returns the recorded entries and the
// script hash from the meta header. Incomplete tail lines (from a crash
// mid-write) are silently dropped — the same approach as agent/session.go.
func LoadJournal(dir, runID string) (entries []JournalEntry, hash string, err error) {
	path := filepath.Join(dir, runID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("workflow: journal %q not found", runID)
		}
		return nil, "", fmt.Errorf("workflow: read journal %s: %w", path, err)
	}
	// Drop incomplete tail (a crash may leave a partial final record).
	if n := bytes.LastIndexByte(data, '\n'); n >= 0 {
		data = data[:n+1]
	} else {
		data = nil
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 4096), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// Use a generic map to discriminate on "type" before parsing the rest.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue // skip corrupt lines
		}
		var recType string
		if t, ok := raw["type"]; ok {
			_ = json.Unmarshal(t, &recType)
		}
		switch recType {
		case "meta":
			var m journalFileMeta
			if err := json.Unmarshal(line, &m); err == nil {
				hash = m.ScriptHash
			}
		case "agent":
			var e journalFileEntry
			if err := json.Unmarshal(line, &e); err == nil {
				entries = append(entries, JournalEntry{
					Seq:          e.Seq,
					Prompt:       e.Prompt,
					Reply:        e.Reply,
					InputTokens:  e.InputTokens,
					OutputTokens: e.OutputTokens,
					ErrMsg:       e.ErrMsg,
				})
			}
		}
	}
	return entries, hash, sc.Err()
}
