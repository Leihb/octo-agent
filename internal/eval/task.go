// Package eval is a lightweight, Docker-free task harness for measuring whether
// a prompt, tool, or model change made octo better or worse at real edits.
//
// Each task is a local fixture repo with a hidden verify step: it clones
// nothing and builds no Docker images. One task = one octo agentic run + one
// verify command, so a suite runs in seconds.
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Task is one eval case. Its directory layout is:
//
//	<dir>/
//	  task.yaml      // this struct
//	  repo/          // fixture source octo edits (copied to a scratch dir)
//	  hidden/        // files injected before verify; octo never sees them
//	  verify.sh      // run after hidden injection; exit 0 == resolved
//
// The hidden/ + verify.sh split is what stops octo gaming the score: the
// judging files don't exist in the working copy while octo edits, so it can't
// read or rewrite them — it only ever sees repo/.
type Task struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	// Timeout caps a single octo run for this task. Empty means the suite
	// default. Parsed from a Go duration string ("5m", "90s").
	TimeoutRaw string `yaml:"timeout"`

	Timeout time.Duration `yaml:"-"`
	Dir     string        `yaml:"-"` // absolute path to the task directory
}

// LoadTasks scans tasksDir for immediate subdirectories that contain a
// task.yaml, returning them sorted by name. A subdirectory without a task.yaml
// is skipped silently (lets you keep a README or shared helper alongside).
func LoadTasks(tasksDir string) ([]Task, error) {
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return nil, fmt.Errorf("read tasks dir: %w", err)
	}
	var tasks []Task
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tasksDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err != nil {
			continue // not a task directory
		}
		t, err := loadTask(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	return tasks, nil
}

func loadTask(dir string) (Task, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Task{}, err
	}
	raw, err := os.ReadFile(filepath.Join(abs, "task.yaml"))
	if err != nil {
		return Task{}, err
	}
	var t Task
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return Task{}, fmt.Errorf("parse task.yaml: %w", err)
	}
	t.Dir = abs
	if t.Name == "" {
		t.Name = filepath.Base(abs)
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return Task{}, fmt.Errorf("task.yaml has no prompt")
	}
	if t.TimeoutRaw != "" {
		d, err := time.ParseDuration(t.TimeoutRaw)
		if err != nil {
			return Task{}, fmt.Errorf("parse timeout %q: %w", t.TimeoutRaw, err)
		}
		t.Timeout = d
	}
	// Structural sanity: the pieces the harness depends on must exist.
	if _, err := os.Stat(filepath.Join(abs, "repo")); err != nil {
		return Task{}, fmt.Errorf("missing repo/ dir")
	}
	if _, err := os.Stat(filepath.Join(abs, "verify.sh")); err != nil {
		return Task{}, fmt.Errorf("missing verify.sh")
	}
	return t, nil
}
