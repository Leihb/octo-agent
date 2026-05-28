// Package tasks implements a session-scoped task tracker. The agent's
// task_manager tool group (task_create / task_update / task_list) backs onto
// this Store so the model can break work down into discrete steps and surface
// progress to the user.
//
// Phase 1 is intentionally in-memory only — tasks vanish when the REPL exits.
// Cross-session persistence is M11 territory (`octo task` CLI + JSON files
// under ~/.octo/tasks/) and is deliberately out of scope here.
package tasks

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the lifecycle marker of a task. The model drives transitions:
// new tasks land as Pending; the model marks them InProgress when it starts
// and Completed when finished. Deleted is the soft-delete sentinel — calls
// to List skip it, but the task isn't actually removed (so a stale ID won't
// silently re-bind to a different task later).
type Status string

const (
	Pending    Status = "pending"
	InProgress Status = "in_progress"
	Completed  Status = "completed"
	Deleted    Status = "deleted"
)

// validStatus reports whether s is a known status value. Unknown strings from
// the LLM are rejected so the store doesn't accumulate junk values that
// won't survive a future schema check.
func validStatus(s Status) bool {
	switch s {
	case Pending, InProgress, Completed, Deleted:
		return true
	}
	return false
}

// Task is one tracked work item. ID is assigned by the store on Create and
// never reused — even after Delete, the slot stays so a stale ID surfaces as
// "deleted" rather than silently re-binding.
type Task struct {
	ID          int       // 1-based, monotonic per store
	Subject     string    // short imperative title ("Migrate auth middleware")
	Description string    // longer detail / acceptance criteria (optional)
	Status      Status    // see consts above
	ActiveForm  string    // present continuous form ("Migrating auth middleware") for spinner UI
	Created     time.Time // first Create
	Updated     time.Time // last Update or Create
}

// Store is the session-scoped task list. Methods are safe for concurrent use
// — the tools and the REPL's /tasks command may dispatch through it at the
// same time (parallel sub-agent calls share the parent's store; the REPL
// loop runs synchronously w.r.t. tool dispatch, but defensively guarding is
// cheap and prevents future surprises).
type Store struct {
	mu    sync.Mutex
	next  int
	tasks []Task // dense slice; index irrelevant for users (lookups by ID)
}

// New returns an empty Store with the next-ID counter at 1.
func New() *Store {
	return &Store{next: 1}
}

// Create adds a new task. Subject is required; description and activeForm
// are optional. Returns the assigned ID.
func (s *Store) Create(subject, description, activeForm string) (int, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return 0, fmt.Errorf("task: subject is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	t := Task{
		ID:          s.next,
		Subject:     subject,
		Description: strings.TrimSpace(description),
		Status:      Pending,
		ActiveForm:  strings.TrimSpace(activeForm),
		Created:     now,
		Updated:     now,
	}
	s.next++
	s.tasks = append(s.tasks, t)
	return t.ID, nil
}

// Update mutates an existing task. Only fields whose UpdateField pointer is
// non-nil are touched, so callers can change just the status without
// supplying the subject etc. Status must be a known value when provided.
type UpdateField struct {
	Status      *Status
	Subject     *string
	Description *string
	ActiveForm  *string
}

// Update applies u to the task with the given ID. Returns the updated task
// (copy, not pointer — Store owns the canonical record) or an error if the
// task doesn't exist or u carries an invalid status.
func (s *Store) Update(id int, u UpdateField) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(id)
	if idx < 0 {
		return Task{}, fmt.Errorf("task: no task with id %d", id)
	}
	if u.Status != nil && !validStatus(*u.Status) {
		return Task{}, fmt.Errorf("task: invalid status %q (want pending|in_progress|completed|deleted)", *u.Status)
	}
	t := &s.tasks[idx]
	if u.Status != nil {
		t.Status = *u.Status
	}
	if u.Subject != nil {
		subj := strings.TrimSpace(*u.Subject)
		if subj == "" {
			return Task{}, fmt.Errorf("task: subject cannot be cleared to empty")
		}
		t.Subject = subj
	}
	if u.Description != nil {
		t.Description = strings.TrimSpace(*u.Description)
	}
	if u.ActiveForm != nil {
		t.ActiveForm = strings.TrimSpace(*u.ActiveForm)
	}
	t.Updated = time.Now().UTC()
	return *t, nil
}

// Get returns the task with the given ID (including deleted ones, so a
// stale ID can be diagnosed). The second return is false if the ID was
// never assigned.
func (s *Store) Get(id int) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(id)
	if idx < 0 {
		return Task{}, false
	}
	return s.tasks[idx], true
}

// List returns every non-deleted task in display order: in-progress first
// (most pressing), then pending (next up), then completed (done, kept
// visible for context). Within each bucket, ordered by ID ascending.
func (s *Store) List() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if t.Status == Deleted {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := statusOrder(out[i].Status), statusOrder(out[j].Status)
		if oi != oj {
			return oi < oj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// indexLocked finds the slice index of the task with the given ID. -1 means
// not found. Caller MUST hold s.mu.
func (s *Store) indexLocked(id int) int {
	for i, t := range s.tasks {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// statusOrder ranks statuses for the display sort. Smaller = shown earlier.
func statusOrder(s Status) int {
	switch s {
	case InProgress:
		return 0
	case Pending:
		return 1
	case Completed:
		return 2
	}
	return 3
}

// Summary returns a one-line count of tasks by status — used for the REPL
// status line printed after each task tool call:
//
//	"3 in progress, 2 pending, 5 done"
//
// Buckets with zero are omitted. Returns "" when there are no tasks.
func (s *Store) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var inProg, pending, done int
	for _, t := range s.tasks {
		switch t.Status {
		case InProgress:
			inProg++
		case Pending:
			pending++
		case Completed:
			done++
		}
	}
	if inProg+pending+done == 0 {
		return ""
	}
	var parts []string
	if inProg > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", inProg))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", done))
	}
	return strings.Join(parts, ", ")
}
