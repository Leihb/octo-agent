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

// taskStoreCtxKey carries a per-turn TaskStore so the task_* tools dispatch to
// it instead of the process-global activeTasks. A request/response transport
// (server, IM) stamps a fresh per-session store here; the interactive CLI
// leaves it unset and falls through to the global.
type taskStoreCtxKeyType struct{}

var taskStoreCtxKey = taskStoreCtxKeyType{}

// WithTaskStore returns ctx carrying store for the task_* tools to find.
func WithTaskStore(ctx context.Context, store TaskStore) context.Context {
	return context.WithValue(ctx, taskStoreCtxKey, store)
}

// taskStoreFromContext returns the ctx-scoped store, or nil.
func taskStoreFromContext(ctx context.Context) TaskStore {
	s, _ := ctx.Value(taskStoreCtxKey).(TaskStore)
	return s
}

// resolveTaskStore picks the store a task_* tool should use: the ctx-scoped one
// (per-turn, server/IM) first, then the process-global default (CLI). Returns
// nil when none is configured.
func resolveTaskStore(ctx context.Context) TaskStore {
	if s := taskStoreFromContext(ctx); s != nil {
		return s
	}
	return activeTasks
}

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

func (TaskCreateTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	store := resolveTaskStore(ctx)
	if store == nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_create: task tracking is not configured for this session")
	}
	subject := strings.TrimSpace(stringArg(input, "subject"))
	if subject == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_create: subject is required")
	}
	id, err := store.Create(
		subject,
		stringArg(input, "description"),
		stringArg(input, "active_form"),
	)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_create: %w", err)
	}
	return agent.ToolResult{
		Text: fmt.Sprintf("Created task #%d: %s", id, subject),
		UI:   taskUI(fmt.Sprintf("Created #%d", id), store),
	}, nil
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

func (TaskUpdateTool) Execute(ctx context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	store := resolveTaskStore(ctx)
	if store == nil {
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

	oldTask, _ := store.Get(id)
	got, err := store.Update(id, u)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("task_update: %w", err)
	}
	statusClause := string(got.Status)
	if u.Status != nil && oldTask.Status != got.Status {
		statusClause = fmt.Sprintf("%s → %s", oldTask.Status, got.Status)
	}
	return agent.ToolResult{
		Text: fmt.Sprintf("Updated task #%d (%s): %s", got.ID, statusClause, got.Subject),
		UI:   taskUI(fmt.Sprintf("Updated #%d", got.ID), store),
	}, nil
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

func (TaskListTool) Execute(ctx context.Context, _ string, _ map[string]any) (agent.ToolResult, error) {
	store := resolveTaskStore(ctx)
	if store == nil {
		return agent.ToolResult{}, fmt.Errorf("task_list: task tracking is not configured for this session")
	}
	return agent.ToolResult{Text: FormatTaskList(store.List()), UI: taskUI("Tasks", store)}, nil
}

// taskUI builds the "todo" UI payload: the current list (deleted tasks
// hidden) plus a done-count progress line for the collapsed summary.
func taskUI(action string, store TaskStore) map[string]any {
	var todos []map[string]any
	done, total := 0, 0
	for _, t := range store.List() {
		if t.Status == tasks.Deleted {
			continue
		}
		total++
		if t.Status == tasks.Completed {
			done++
		}
		if len(todos) < 20 {
			todos = append(todos, map[string]any{
				"task":   t.Subject,
				"status": string(t.Status),
			})
		}
	}
	return map[string]any{
		"type":     "todo",
		"action":   action,
		"progress": fmt.Sprintf("%d/%d done", done, total),
		"todos":    todos,
	}
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
	b.WriteString(taskCountHeader(items))
	b.WriteByte('\n')
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

// taskCountHeader builds the "Tasks: N in progress, M pending, K completed"
// summary line, listing only the non-zero buckets (and falling back to a bare
// "Tasks:" when every task is deleted). Matches the layout documented on
// FormatTaskList.
func taskCountHeader(items []tasks.Task) string {
	var inProgress, pending, completed int
	for _, t := range items {
		switch t.Status {
		case tasks.InProgress:
			inProgress++
		case tasks.Pending:
			pending++
		case tasks.Completed:
			completed++
		}
	}
	var parts []string
	if inProgress > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", inProgress))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	if completed > 0 {
		parts = append(parts, fmt.Sprintf("%d completed", completed))
	}
	if len(parts) == 0 {
		return "Tasks:"
	}
	return "Tasks: " + strings.Join(parts, ", ")
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
