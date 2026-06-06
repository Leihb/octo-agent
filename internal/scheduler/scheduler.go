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

	"github.com/robfig/cron/v3"
)

// Task represents a scheduled agent task.
type Task struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Cron      string    `json:"cron"`
	Prompt    string    `json:"prompt"`
	Model     string    `json:"model,omitempty"`
	Agent     string    `json:"agent,omitempty"` // "general" | "coding"
	Directory string    `json:"directory,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	LastRun   time.Time `json:"last_run,omitempty"`
	NextRun   time.Time `json:"next_run,omitempty"`
	SessionID string    `json:"session_id,omitempty"` // last session ID
}

// Runner is the interface the scheduler calls to execute a task.
type Runner interface {
	RunTask(ctx context.Context, task Task) (sessionID string, err error)
}

// Scheduler manages cron-based task execution.
type Scheduler struct {
	dir    string
	runner Runner
	cron   *cron.Cron

	mu    sync.Mutex
	tasks map[string]*Task
}

// New creates a Scheduler. dir is where task JSON files are stored
// (typically ~/.octo/tasks/). runner is called when a task is due.
func New(dir string, runner Runner) (*Scheduler, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}

	s := &Scheduler{
		dir:    dir,
		runner: runner,
		cron:   cron.New(cron.WithSeconds()),
		tasks:  make(map[string]*Task),
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

// Add creates a new task and schedules it.
func (s *Scheduler) Add(task Task) error {
	if task.ID == "" {
		task.ID = fmt.Sprintf("task_%d", time.Now().UnixMilli())
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	if task.Enabled {
		if _, err := s.cron.AddFunc(task.Cron, s.wrap(task)); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", task.Cron, err)
		}
	}
	if err := s.save(task); err != nil {
		return err
	}
	s.mu.Lock()
	s.tasks[task.ID] = &task
	s.mu.Unlock()
	return nil
}

// Update modifies an existing task.
func (s *Scheduler) Update(task Task) error {
	s.mu.Lock()
	existing, ok := s.tasks[task.ID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("task %q not found", task.ID)
	}

	// Remove old cron entry (we can't easily remove individual entries in robfig/cron,
	// so we reconstruct the whole schedule). For now, just update the stored entry.
	existing.Name = task.Name
	existing.Cron = task.Cron
	existing.Prompt = task.Prompt
	existing.Model = task.Model
	existing.Agent = task.Agent
	existing.Directory = task.Directory
	existing.Enabled = task.Enabled
	s.mu.Unlock()

	return s.save(*existing)
}

// Delete removes a task by ID.
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	delete(s.tasks, id)
	s.mu.Unlock()
	p := filepath.Join(s.dir, id+".json")
	return os.Remove(p)
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

// RunNow immediately executes a task by ID.
func (s *Scheduler) RunNow(ctx context.Context, id string) (string, error) {
	t, err := s.Get(id)
	if err != nil {
		return "", err
	}
	return s.runner.RunTask(ctx, *t)
}

// ─── Private methods ─────────────────────────────────────────────────────

func (s *Scheduler) wrap(task Task) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		sessionID, err := s.runner.RunTask(ctx, task)
		s.mu.Lock()
		if t, ok := s.tasks[task.ID]; ok {
			t.LastRun = time.Now()
			t.SessionID = sessionID
			if err != nil {
				log.Printf("[scheduler] task %q failed: %v", task.Name, err)
			}
		}
		s.mu.Unlock()
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
			if _, err := s.cron.AddFunc(task.Cron, s.wrap(task)); err != nil {
				log.Printf("[scheduler] cron add %q: %v", task.Name, err)
			}
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
