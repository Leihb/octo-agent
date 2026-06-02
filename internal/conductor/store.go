package conductor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the on-disk conductor repository. Each ledger gets its own
// directory under the base (default ~/.octo/conductor/<id>/) holding three
// files — the design's "磁盘三件套":
//
//	ledger.json     machine state, atomically written + fsync'd per transition
//	CONVENTIONS.md  the shared brain: naming/layout/type-mapping decisions
//	                workers read before starting and append to when done
//	JOURNAL.md      append-only audit log of every worker run + verdict
//
// ledger.json is the conductor's source of truth and only the conductor
// writes it. CONVENTIONS.md is written by workers (and seeded by the
// conductor); JOURNAL.md is append-only by the conductor. Keeping the two
// markdown docs in a per-ledger dir OUTSIDE any worktree means they survive
// and stay shared even when Phase 2 workers run in isolated worktrees.
type Store struct {
	mu   sync.Mutex
	base string
}

const (
	ledgerFile      = "ledger.json"
	conventionsFile = "CONVENTIONS.md"
	journalFile     = "JOURNAL.md"
)

// DefaultDir resolves ~/.octo/conductor.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("conductor: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, ".octo", "conductor"), nil
}

// NewStore returns a Store rooted at DefaultDir.
func NewStore() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(dir), nil
}

// NewStoreAt returns a Store rooted at base (used by tests).
func NewStoreAt(base string) *Store { return &Store{base: base} }

// Dir returns the per-ledger directory for id (where the three files live).
func (s *Store) Dir(id string) string { return filepath.Join(s.base, id) }

// ConventionsPath / JournalPath give the absolute paths workers are told to
// read/append. They live in the per-ledger dir, stable across worktrees.
func (s *Store) ConventionsPath(id string) string {
	return filepath.Join(s.Dir(id), conventionsFile)
}
func (s *Store) JournalPath(id string) string {
	return filepath.Join(s.Dir(id), journalFile)
}

// Create persists a new ledger from goal + seed units and seeds an empty
// CONVENTIONS.md (with the goal as a header so workers have orientation).
func (s *Store) Create(goal string, units []Unit) (*Ledger, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, errors.New("conductor: goal is required")
	}
	if err := validateUnits(units); err != nil {
		return nil, err
	}
	for i := range units {
		if units[i].Status == "" {
			units[i].Status = UnitPending
		}
	}
	now := time.Now().UTC()
	id, err := newID(now)
	if err != nil {
		return nil, err
	}
	l := &Ledger{
		ID:      id,
		Goal:    goal,
		Status:  LedgerPending,
		Created: now,
		Updated: now,
		Units:   units,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir(id), 0o755); err != nil {
		return nil, err
	}
	if err := s.persistLocked(l); err != nil {
		return nil, err
	}
	// Seed conventions doc if absent. Best-effort — a missing doc is not fatal.
	cp := s.ConventionsPath(id)
	if _, statErr := os.Stat(cp); os.IsNotExist(statErr) {
		seed := fmt.Sprintf("# Conventions & Decisions\n\nGoal: %s\n\n"+
			"Workers: read this file before starting, and append any naming, "+
			"package-layout, type-mapping, or interface decisions you make so "+
			"later units stay consistent with yours.\n", goal)
		_ = os.WriteFile(cp, []byte(seed), 0o644)
	}
	return l, nil
}

// Update applies mutate to the loaded ledger, stamps Updated, and persists
// atomically. One fsync per call — a crash strands at most one transition.
func (s *Store) Update(id string, mutate func(*Ledger) error) (*Ledger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.loadLocked(id)
	if err != nil {
		return nil, err
	}
	if err := mutate(l); err != nil {
		return nil, err
	}
	l.Updated = time.Now().UTC()
	if err := s.persistLocked(l); err != nil {
		return nil, err
	}
	return l, nil
}

// Get returns a copy of the ledger with the given id.
func (s *Store) Get(id string) (*Ledger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

// List enumerates all ledgers, newest first (ids are timestamp-prefixed).
func (s *Store) List() ([]*Ledger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.listIDsLocked()
	if err != nil {
		return nil, err
	}
	var out []*Ledger
	for _, id := range ids {
		l, err := s.loadLocked(id)
		if err != nil {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// AppendJournal appends a timestamped line to the ledger's JOURNAL.md.
// Best-effort: a journal write error never aborts the run.
func (s *Store) AppendJournal(id, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.JournalPath(id)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().UTC().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "[%s] %s\n", ts, strings.TrimRight(line, "\n"))
}

// ReadConventions returns the current conventions doc, or "" if absent.
func (s *Store) ReadConventions(id string) string {
	b, err := os.ReadFile(s.ConventionsPath(id))
	if err != nil {
		return ""
	}
	return string(b)
}

// ResolveID maps "last" / a full id / a unique substring to a stored id.
func (s *Store) ResolveID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("conductor: id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.listIDsLocked()
	if err != nil {
		return "", err
	}
	if input == "last" {
		if len(ids) == 0 {
			return "", errors.New("conductor: no ledgers to reference with 'last'")
		}
		return ids[0], nil
	}
	// Exact match first.
	for _, id := range ids {
		if id == input {
			return id, nil
		}
	}
	var matches []string
	for _, id := range ids {
		if strings.Contains(id, input) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("conductor: no ledger matches %q", input)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("conductor: ambiguous %q matches %d ledgers:\n  %s",
		input, len(matches), strings.Join(matches, "\n  "))
}

func (s *Store) ledgerPath(id string) string { return filepath.Join(s.Dir(id), ledgerFile) }

func (s *Store) listIDsLocked() ([]string, error) {
	ents, err := os.ReadDir(s.base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(s.base, e.Name(), ledgerFile)); statErr == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

func (s *Store) loadLocked(id string) (*Ledger, error) {
	if id == "" {
		return nil, errors.New("conductor: id is required")
	}
	b, err := os.ReadFile(s.ledgerPath(id))
	if err != nil {
		return nil, err
	}
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("conductor: parse %s: %w", id, err)
	}
	return &l, nil
}

// persistLocked writes ledger.json atomically with fsync (tmp → fsync →
// rename → dir fsync). Same durability contract as taskgraph.Store.
func (s *Store) persistLocked(l *Ledger) error {
	dir := s.Dir(l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.ledgerPath(l.ID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.ledgerPath(l.ID)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// newID generates `YYYYMMDD-HHMMSS-<8 hex>`, same shape as task/session ids.
func newID(now time.Time) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("conductor: generate id: %w", err)
	}
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(buf[:]), nil
}
