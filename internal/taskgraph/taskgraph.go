// Package taskgraph implements octo's autonomous task orchestration data
// layer (M11). The package is named taskgraph (not "task") to avoid colliding
// with internal/tasks — that one backs the in-session
// task_create/task_update/task_list tool group (a todo list); this one
// backs `octo task` (a planned DAG of autonomous work).
//
// A Task is a top-level goal the user submitted (`octo task start "…"`). It
// owns a DAG of Subtasks the planner decomposed it into. The scheduler
// (separate package, separate PR) walks the DAG: each ready Subtask becomes
// one M10 sub-agent call running in its own isolated context. The harness
// only ever drives the task-graph state — the long-running conversation
// history that breaks `/goal`-style evaluators never gets sent anywhere.
//
// Persistence is one JSON file per Task under `~/.octo/tasks/<id>.json`.
// Every state transition is fsync'd before returning, so a `kill -9` mid-run
// resumes cleanly from `octo task resume <id>` (PR4) without any subtask
// re-running by accident.
//
// This package is pure data + state machine + persistence. It has no LLM
// dependency, no scheduler logic, and no I/O beyond the filesystem.
package taskgraph

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

// TaskStatus is the top-level lifecycle marker.
//
//	pending   the task is on disk but the scheduler hasn't picked it up
//	running   at least one subtask is in flight (or the scheduler is iterating)
//	done      every required subtask finished successfully
//	failed    a subtask errored under stop-on-fail and remaining were skipped
//	cancelled the user called `octo task cancel <id>`
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskDone      TaskStatus = "done"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

// SubtaskStatus is the per-node lifecycle. `skipped` is the marker the
// stop-on-fail scheduler stamps on still-pending subtasks once any peer has
// failed, so the resume path can distinguish "didn't try" from "tried and
// failed".
type SubtaskStatus string

const (
	SubtaskPending SubtaskStatus = "pending"
	SubtaskRunning SubtaskStatus = "running"
	SubtaskDone    SubtaskStatus = "done"
	SubtaskFailed  SubtaskStatus = "failed"
	SubtaskSkipped SubtaskStatus = "skipped"
)

// Task is one autonomous goal plus its planned DAG of subtasks.
type Task struct {
	ID       string     `json:"id"`
	Goal     string     `json:"goal"`
	Status   TaskStatus `json:"status"`
	Created  time.Time  `json:"created"`
	Updated  time.Time  `json:"updated"`
	Subtasks []Subtask  `json:"subtasks"`
}

// Subtask is one node in the DAG.
//
// BlockedBy lists IDs (1-based, within the same Task) of subtasks that must
// reach SubtaskDone before this one is eligible to run. An empty BlockedBy
// means "ready from the start".
//
// Result holds the sub-agent's final reply text on Done. Error carries the
// failure message on Failed. Both are empty otherwise.
type Subtask struct {
	ID          int           `json:"id"`
	Description string        `json:"description"`
	Status      SubtaskStatus `json:"status"`
	BlockedBy   []int         `json:"blocked_by,omitempty"`
	Result      string        `json:"result,omitempty"`
	Error       string        `json:"error,omitempty"`
	Started     *time.Time    `json:"started,omitempty"`
	Finished    *time.Time    `json:"finished,omitempty"`
}

// validateSubtasks runs the integrity checks the planner output has to
// satisfy before we persist it. Catches loops, dangling references, and
// non-monotonic IDs early.
func validateSubtasks(subs []Subtask) error {
	if len(subs) == 0 {
		return errors.New("task: at least one subtask is required")
	}
	seen := make(map[int]bool, len(subs))
	for i, s := range subs {
		if strings.TrimSpace(s.Description) == "" {
			return fmt.Errorf("task: subtask %d has empty description", i+1)
		}
		// IDs must be 1, 2, 3, … in order. Anything else is a planner bug.
		want := i + 1
		if s.ID != want {
			return fmt.Errorf("task: subtask at index %d has ID %d, want %d", i, s.ID, want)
		}
		if seen[s.ID] {
			return fmt.Errorf("task: duplicate subtask ID %d", s.ID)
		}
		seen[s.ID] = true
	}
	// BlockedBy references must point at earlier subtasks (DAG, no cycles).
	// Requiring "earlier" rather than "any but self" is stricter than needed
	// for a DAG but trivially rules out cycles and matches how the planner
	// emits the list.
	for _, s := range subs {
		for _, dep := range s.BlockedBy {
			if dep <= 0 || dep >= s.ID {
				return fmt.Errorf("task: subtask %d depends on %d (must reference an earlier subtask)", s.ID, dep)
			}
			if !seen[dep] {
				return fmt.Errorf("task: subtask %d depends on unknown subtask %d", s.ID, dep)
			}
		}
	}
	return nil
}

// Ready reports whether all of s's dependencies are Done in the given task.
// A subtask must be Pending AND have all its BlockedBy dependencies Done to
// be ready. The scheduler calls this on every loop iteration.
func (s Subtask) Ready(in *Task) bool {
	if s.Status != SubtaskPending {
		return false
	}
	for _, depID := range s.BlockedBy {
		dep := in.Find(depID)
		if dep == nil || dep.Status != SubtaskDone {
			return false
		}
	}
	return true
}

// Find returns a pointer to the subtask with the given ID, or nil. The
// returned pointer is into Task.Subtasks — mutations are visible to the
// Task. Callers updating state should go through the Store so the change
// is fsync'd.
func (t *Task) Find(id int) *Subtask {
	for i := range t.Subtasks {
		if t.Subtasks[i].ID == id {
			return &t.Subtasks[i]
		}
	}
	return nil
}

// Store is the on-disk task repository, rooted at a base dir (default
// ~/.octo/tasks/). Every mutating operation holds the store mutex, persists
// the changed task to a temp file, fsyncs, and atomically renames so a
// kill -9 either sees the old state or the new — never a half-written file.
type Store struct {
	mu  sync.Mutex
	dir string
}

// DefaultDir resolves ~/.octo/tasks.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("task: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, ".octo", "tasks"), nil
}

// NewStore returns a Store rooted at DefaultDir.
func NewStore() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(dir), nil
}

// NewStoreAt returns a Store rooted at dir (used by tests).
func NewStoreAt(dir string) *Store {
	return &Store{dir: dir}
}

// Dir returns the on-disk root of this store. Useful for tests + CLI
// "show me where my tasks live" UX.
func (s *Store) Dir() string { return s.dir }

// Create persists a new Task assembled from goal + planned subtasks. The
// ID is generated here (`YYYYMMDD-HHMMSS-<8 hex>`); the caller never picks.
// Returns the persisted Task (with ID / Created / Updated populated).
func (s *Store) Create(goal string, subs []Subtask) (*Task, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, errors.New("task: goal is required")
	}
	if err := validateSubtasks(subs); err != nil {
		return nil, err
	}
	for i := range subs {
		if subs[i].Status == "" {
			subs[i].Status = SubtaskPending
		}
	}
	now := time.Now().UTC()
	id, err := newID(now)
	if err != nil {
		return nil, err
	}
	t := &Task{
		ID:       id,
		Goal:     goal,
		Status:   TaskPending,
		Created:  now,
		Updated:  now,
		Subtasks: subs,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	if err := s.persistLocked(t); err != nil {
		return nil, err
	}
	return t, nil
}

// Update applies mutate to a copy of the task, validates, and persists. The
// mutate callback may mutate the passed Task pointer freely — the store
// stamps Updated and writes atomically. Returns the persisted Task.
//
// Use this for every state transition: marking a subtask Running, then Done,
// then advancing the parent task to Done/Failed. Each call is one fsync, so
// a crash between calls strands at most one transition.
func (s *Store) Update(id string, mutate func(*Task) error) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.loadLocked(id)
	if err != nil {
		return nil, err
	}
	if err := mutate(t); err != nil {
		return nil, err
	}
	t.Updated = time.Now().UTC()
	if err := s.persistLocked(t); err != nil {
		return nil, err
	}
	return t, nil
}

// Get returns a copy of the task with the given ID (or an error if it
// doesn't exist / can't be read). The copy is safe to inspect concurrently
// — any subsequent mutation must go through Update.
func (s *Store) Get(id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

// List enumerates all task IDs the store knows about, sorted descending by
// filename (which is timestamp-prefixed, so newest first).
func (s *Store) List() ([]*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Task
	for _, de := range ents {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		t, err := s.loadLocked(id)
		if err != nil {
			continue // skip unreadable; don't poison the list
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// ShortID returns the 8-character abbreviation of t.ID, suitable for display
// in CLI lists. Same rule as agent.Session.ShortID — the trailing hex suffix.
func (t *Task) ShortID() string { return shortTaskID(t.ID) }

func shortTaskID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// listIDsLocked enumerates every task ID under s.dir, newest first.
// Caller must hold s.mu.
func (s *Store) listIDsLocked() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(n, ".json"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// ResolveID maps a user-typed identifier to a stored task ID.
// Accepts: "last" (most recent), the exact full ID, or any substring of
// an ID (unique match required). Ambiguity surfaces as an error listing
// the candidates so the user can re-disambiguate.
func (s *Store) ResolveID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("task id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Fast path: exact filename hit. Avoids the directory walk when the
	// caller pasted a full ID.
	if input != "last" {
		if _, err := os.Stat(s.path(input)); err == nil {
			return input, nil
		}
	}
	ids, err := s.listIDsLocked()
	if err != nil {
		return "", err
	}
	if input == "last" {
		if len(ids) == 0 {
			return "", errors.New("no tasks to reference with 'last'")
		}
		return ids[0], nil
	}
	var matches []string
	for _, id := range ids {
		if strings.Contains(id, input) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no task matches %q", input)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("ambiguous task %q matches %d tasks:\n  %s",
		input, len(matches), strings.Join(matches, "\n  "))
}

// path returns the on-disk JSON file for a given task id.
func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *Store) ensureDir() error { return os.MkdirAll(s.dir, 0o755) }

// loadLocked reads + decodes the task file. Caller must hold s.mu.
func (s *Store) loadLocked(id string) (*Task, error) {
	if id == "" {
		return nil, errors.New("task: id is required")
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("task: parse %s: %w", id, err)
	}
	return &t, nil
}

// persistLocked writes the task atomically with fsync. Caller holds s.mu.
//
// The flow is:
//
//  1. Marshal to bytes.
//  2. Write to "<id>.json.tmp".
//  3. fsync the tmp file (so contents hit disk).
//  4. Rename to "<id>.json" (atomic on POSIX; on Windows os.Rename is
//     atomic too as long as the target's parent directory matches).
//  5. fsync the parent directory (so the rename hits disk).
//
// Cost is one syscall round-trip per state change. Worth it: a kill -9
// between subtask completions otherwise loses the just-completed result.
func (s *Store) persistLocked(t *Task) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(t.ID) + ".tmp"
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
	if err := os.Rename(tmp, s.path(t.ID)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// fsync the parent dir so the rename is durable. On Windows os.Open of a
	// directory and the subsequent Sync are no-ops, so this is harmless there.
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// newID generates a task identifier of the form `YYYYMMDD-HHMMSS-<8 hex>`.
// The timestamp prefix gives chronological filename sort; the 32-bit random
// suffix makes same-second collisions effectively impossible without making
// the ID hard to type. Same shape as session IDs (see the fix in #64).
func newID(now time.Time) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("task: generate id: %w", err)
	}
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(buf[:]), nil
}
