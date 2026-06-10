// Package scheduler provides a simple cron-based task scheduler for octo.
//
// Tasks are stored as JSON files in ~/.octo/tasks/. The scheduler runs in
// a goroutine, checking for due tasks every minute and spawning agent
// sessions to execute them.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/trash"
	"github.com/robfig/cron/v3"
)

// NotifyTarget routes a task run's outcome to an IM chat: the final reply on
// success, a short failure note on error.
type NotifyTarget struct {
	Platform string `json:"platform"` // e.g. "feishu"
	ChatID   string `json:"chat_id"`
}

// Task represents a scheduled agent task.
type Task struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Cron      string        `json:"cron"`
	Prompt    string        `json:"prompt"`
	Model     string        `json:"model,omitempty"`
	Agent     string        `json:"agent,omitempty"` // "general" | "coding"
	Directory string        `json:"directory,omitempty"`
	Notify    *NotifyTarget `json:"notify,omitempty"`
	Enabled   bool          `json:"enabled"`
	CreatedAt time.Time     `json:"created_at"`
	LastRun   time.Time     `json:"last_run,omitempty"`
	NextRun   time.Time     `json:"next_run,omitempty"`
	SessionID string        `json:"session_id,omitempty"` // last session ID
}

// Runner is the interface the scheduler calls to execute a task.
type Runner interface {
	RunTask(ctx context.Context, task Task) (sessionID string, err error)
}

// exprParser validates cron expressions with the same grammar the runtime
// cron uses (six fields, seconds first, plus @descriptors), so Add/Update can
// reject a bad expression before touching any state.
var exprParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Scheduler manages cron-based task execution.
type Scheduler struct {
	dir    string
	runner Runner
	cron   *cron.Cron

	mu      sync.Mutex
	tasks   map[string]*Task
	entries map[string]cron.EntryID // live cron entry per enabled task
}

// New creates a Scheduler. dir is where task JSON files are stored
// (typically ~/.octo/tasks/). runner is called when a task is due.
func New(dir string, runner Runner) (*Scheduler, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}

	s := &Scheduler{
		dir:     dir,
		runner:  runner,
		cron:    cron.New(cron.WithSeconds()),
		tasks:   make(map[string]*Task),
		entries: make(map[string]cron.EntryID),
	}

	if err := s.loadAll(); err != nil {
		return nil, fmt.Errorf("load tasks: %w", err)
	}

	return s, nil
}

// Start begins the cron scheduler loop. Non-blocking.
func (s *Scheduler) Start() {
	s.cron.Start()
}

// Stop gracefully stops the scheduler.
func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// Add creates a new task and schedules it. The generated ID and CreatedAt are
// written back into task so the caller can report them (e.g. in the create
// API response).
func (s *Scheduler) Add(task *Task) error {
	if _, err := exprParser.Parse(task.Cron); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", task.Cron, err)
	}
	if task.ID == "" {
		task.ID = fmt.Sprintf("task_%d", time.Now().UnixMilli())
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	if err := s.save(*task); err != nil {
		return err
	}
	cp := *task
	s.mu.Lock()
	s.tasks[cp.ID] = &cp
	s.mu.Unlock()
	if cp.Enabled {
		s.schedule(cp.ID, cp.Cron)
	}
	return nil
}

// Update modifies an existing task and reschedules its live cron entry so the
// change (new expression, enable/disable) takes effect immediately, not at the
// next process restart.
func (s *Scheduler) Update(task Task) error {
	if _, err := exprParser.Parse(task.Cron); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", task.Cron, err)
	}
	s.mu.Lock()
	existing, ok := s.tasks[task.ID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("task %q not found", task.ID)
	}
	existing.Name = task.Name
	existing.Cron = task.Cron
	existing.Prompt = task.Prompt
	existing.Model = task.Model
	existing.Agent = task.Agent
	existing.Directory = task.Directory
	existing.Notify = task.Notify
	existing.Enabled = task.Enabled
	cp := *existing
	s.mu.Unlock()

	s.unschedule(task.ID)
	if cp.Enabled {
		s.schedule(cp.ID, cp.Cron)
	}
	return s.save(cp)
}

// Delete removes a task by ID and unregisters its live cron entry.
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	delete(s.tasks, id)
	s.mu.Unlock()
	s.unschedule(id)
	p := filepath.Join(s.dir, id+".json")
	if _, err := os.Stat(p); err == nil {
		if err := trash.Move(p, s.dir); err != nil {
			return fmt.Errorf("trash task %s: %w", p, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List returns all tasks sorted by creation time.
func (s *Scheduler) List() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		result = append(result, *t)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// Get returns a single task by ID.
func (s *Scheduler) Get(id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("task %q not found", id)
	}
	cp := *t
	return &cp, nil
}

// RunNow starts an immediate background run of the task, mirroring a cron
// firing (own 30-minute context, LastRun/SessionID bookkeeping) — except that
// a manual run also fires when the task is disabled, since the user asked for
// it explicitly. It returns once the run is accepted, not when it finishes,
// so an HTTP handler calling it never blocks for the whole agent turn.
func (s *Scheduler) RunNow(id string) error {
	s.mu.Lock()
	_, ok := s.tasks[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("task %q not found", id)
	}
	go s.fire(id, true)
	return nil
}

// ─── Private methods ─────────────────────────────────────────────────────

// schedule registers a cron entry for the task and records its EntryID so a
// later Update/Delete can remove it. The expression is pre-validated by the
// callers, so a parse failure here is a programming error worth logging.
func (s *Scheduler) schedule(id, expr string) {
	eid, err := s.cron.AddFunc(expr, s.wrap(id))
	if err != nil {
		log.Printf("[scheduler] cron add %q: %v", id, err)
		return
	}
	s.mu.Lock()
	s.entries[id] = eid
	s.mu.Unlock()
}

// unschedule removes the task's live cron entry, if any.
func (s *Scheduler) unschedule(id string) {
	s.mu.Lock()
	eid, ok := s.entries[id]
	delete(s.entries, id)
	s.mu.Unlock()
	if ok {
		s.cron.Remove(eid)
	}
}

// wrap returns the function cron fires for a task. It looks the task up by ID
// at fire time — rather than capturing a copy at registration — so a deleted
// or disabled task stops running immediately and prompt/model edits apply to
// the very next run.
func (s *Scheduler) wrap(id string) func() {
	return func() { s.fire(id, false) }
}

// fire executes the task by ID and records LastRun/SessionID, persisting them
// so the bookkeeping survives a restart. A manual fire ignores the Enabled
// flag; a cron fire skips disabled (or since-deleted) tasks.
func (s *Scheduler) fire(id string, manual bool) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok || (!manual && !t.Enabled) {
		s.mu.Unlock()
		return
	}
	task := *t
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	sessionID, err := s.runner.RunTask(ctx, task)
	if err != nil {
		log.Printf("[scheduler] task %q failed: %v", task.Name, err)
	}

	s.mu.Lock()
	t, ok = s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	t.LastRun = time.Now()
	t.SessionID = sessionID
	cp := *t
	s.mu.Unlock()
	if err := s.save(cp); err != nil {
		log.Printf("[scheduler] save task %q: %v", task.Name, err)
	}
}

func (s *Scheduler) loadAll() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			log.Printf("[scheduler] read %s: %v", p, err)
			continue
		}
		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			log.Printf("[scheduler] unmarshal %s: %v", p, err)
			continue
		}
		s.tasks[task.ID] = &task
		if task.Enabled {
			s.schedule(task.ID, task.Cron)
		}
	}
	return nil
}

func (s *Scheduler) save(task Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(s.dir, task.ID+".json")
	return os.WriteFile(p, data, 0600)
}
