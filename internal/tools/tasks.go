package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tasks"
)

// TaskStore is the surface the task_* tools use to talk to the underlying
// store. internal/tasks.Store satisfies this directly; tests can substitute
// fakes without dragging in the tasks package.
type TaskStore interface {
	Create(subject, description, activeForm string) (int, error)
	Update(id int, u tasks.UpdateField) (tasks.Task, error)
	List() []tasks.Task
	Get(id int) (tasks.Task, bool)
	Summary() string
}

// activeTasks, when non-nil, backs the task_* tools and gates their
// advertisement in DefaultTools. Set by cmd/octo at REPL start with a fresh
// per-session Store. Nil → task_create / task_update / task_list don't
// appear in the catalog (single-turn mode and unattended runs).
var activeTasks TaskStore

// SetTaskStore registers the store the task_* tools delegate to. Pass nil to
// disable; the three tools then drop out of DefaultTools.
func SetTaskStore(s TaskStore) { activeTasks = s }

// ActiveTaskStore returns the currently registered store, or nil. Used by
// cmd/octo's /tasks REPL command (and the post-tool summary line) without
// going through a LLM-driven tool call.
func ActiveTaskStore() TaskStore { return activeTasks }

func tasksEnabled() bool { return activeTasks != nil }

// ============================================================================
// task_create
// ============================================================================

// TaskCreateTool adds a new pending task to the session's task list.
//
// Tool name: task_create.
type TaskCreateTool struct{}

func (TaskCreateTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "task_create",
		Description: "Add a new task to the session's task list. Use when you're breaking " +
			"down work into discrete, trackable steps the user can see. Each task lands as " +
			"`pending`; mark it `in_progress` via task_update when you start, `completed` when " +
			"done. Use sparingly — single trivial commands don't need a task. Returns the " +
			"assigned numeric ID.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subject": map[string]any{
					"type":        "string",
					"description": "Short imperative title (3-10 words). Example: 'Migrate auth middleware'.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Optional longer detail / acceptance criteria. Use when the subject alone isn't enough to know when the task is done.",
				},
				"active_form": map[string]any{
					"type":        "string",
					"description": "Optional present-continuous form for the spinner UI (e.g. 'Migrating auth middleware'). Defaults to the subject if omitted.",
				},
			},
			"required": []string{"subject"},
		},
	}
}

func (TaskCreateTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if !tasksEnabled() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_create: task tracking is not configured for this session")
	}
	subject := strings.TrimSpace(stringArg(input, "subject"))
	if subject == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_create: subject is required")
	}
	id, err := activeTasks.Create(
		subject,
		stringArg(input, "description"),
		stringArg(input, "active_form"),
	)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_create: %w", err)
	}
	return agent.ToolResult{Text: fmt.Sprintf("Created task #%d: %s", id, subject)}, nil
}

// ============================================================================
// task_update
// ============================================================================

// TaskUpdateTool mutates an existing task. Most commonly the model uses it to
// move tasks through the pending → in_progress → completed lifecycle.
//
// Tool name: task_update.
type TaskUpdateTool struct{}

func (TaskUpdateTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "task_update",
		Description: "Update an existing task. Most common use: shift status as work progresses " +
			"(`pending` → `in_progress` when you start a step, `in_progress` → `completed` when " +
			"you finish). Can also rewrite subject / description / active_form if the scope of " +
			"the task changes. Set status to `deleted` to drop a task that was misclassified or " +
			"no longer applies. Only the fields you provide are touched.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{
					"type":        "integer",
					"description": "The numeric ID returned by task_create.",
				},
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"pending", "in_progress", "completed", "deleted"},
					"description": "New status. `deleted` removes the task from /tasks views but the ID stays reserved (so stale references surface clearly).",
				},
				"subject": map[string]any{
					"type":        "string",
					"description": "Optional rewrite of the imperative title.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Optional rewrite of the longer detail.",
				},
				"active_form": map[string]any{
					"type":        "string",
					"description": "Optional rewrite of the present-continuous form.",
				},
			},
			"required": []string{"task_id"},
		},
	}
}

func (TaskUpdateTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	if !tasksEnabled() {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_update: task tracking is not configured for this session")
	}
	id := intArg(input, "task_id", 0)
	if id <= 0 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_update: task_id is required (positive integer)")
	}

	var u tasks.UpdateField
	if s := strings.TrimSpace(stringArg(input, "status")); s != "" {
		st := tasks.Status(s)
		u.Status = &st
	}
	if raw, present := input["subject"]; present {
		if s, ok := raw.(string); ok {
			u.Subject = &s
		}
	}
	if raw, present := input["description"]; present {
		if s, ok := raw.(string); ok {
			u.Description = &s
		}
	}
	if raw, present := input["active_form"]; present {
		if s, ok := raw.(string); ok {
			u.ActiveForm = &s
		}
	}

	if u.Status == nil && u.Subject == nil && u.Description == nil && u.ActiveForm == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_update: nothing to update (provide status, subject, description, or active_form)")
	}

	got, err := activeTasks.Update(id, u)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_update: %w", err)
	}
	return agent.ToolResult{Text: fmt.Sprintf("Updated task #%d (%s): %s", got.ID, got.Status, got.Subject)}, nil
}

// ============================================================================
// task_list
// ============================================================================

// TaskListTool returns the current task list, grouped by status.
//
// Tool name: task_list.
type TaskListTool struct{}

func (TaskListTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "task_list",
		Description: "Show the current session's task list, grouped by status (in-progress, " +
			"pending, completed; deleted tasks hidden). Use this to check progress or remind " +
			"yourself which tasks are still open before starting new work.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (TaskListTool) Execute(_ context.Context, _ string, _ map[string]any) (agent.ToolResult, error) {
	if !tasksEnabled() {
		return agent.ToolResult{}, fmt.Errorf("task_list: task tracking is not configured for this session")
	}
	return agent.ToolResult{Text: FormatTaskList(activeTasks.List())}, nil
}

// FormatTaskList renders a slice of tasks for display. Used both by the
// task_list tool's Execute and by the REPL's /tasks slash command.
//
// Layout: a one-line header followed by one row per task, status-prefixed
// so the user can scan at a glance:
//
//	Tasks: 1 in progress, 2 pending
//	  ▶ #3  Migrating auth middleware
//	  ○ #1  Add migration test
//	  ○ #4  Update README
func FormatTaskList(items []tasks.Task) string {
	if len(items) == 0 {
		return "No tasks yet."
	}
	var b strings.Builder
	b.WriteString("Tasks:\n")
	for _, t := range items {
		marker := statusMarker(t.Status)
		title := t.Subject
		if t.Status == tasks.InProgress && t.ActiveForm != "" {
			title = t.ActiveForm // surface the spinner-friendly form when in flight
		}
		fmt.Fprintf(&b, "  %s #%-3d %s\n", marker, t.ID, title)
		if t.Description != "" {
			for _, line := range strings.Split(t.Description, "\n") {
				fmt.Fprintf(&b, "       %s\n", line)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// statusMarker returns the one-rune glyph used in FormatTaskList. Chosen to
// be visually distinct in a monospace terminal:
//
//	▶ in progress (filled triangle = "now")
//	○ pending     (empty circle    = "open")
//	✓ completed   (check           = "done")
//	· anything else (deleted shouldn't appear, but be defensive)
func statusMarker(s tasks.Status) string {
	switch s {
	case tasks.InProgress:
		return "▶"
	case tasks.Pending:
		return "○"
	case tasks.Completed:
		return "✓"
	}
	return "·"
}

// (intArg is shared with read_file.go — package-wide helper.)
