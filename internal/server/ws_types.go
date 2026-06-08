package server

import "encoding/json"

// ─── Client → Server messages (inbound) ─────────────────────────────────────

// wsInMessage is the base envelope for every client-sent message.
type wsInMessage struct {
	Type string `json:"type"`
}

type wsMsgListSessions struct{}

type wsMsgSubscribe struct {
	SessionID string `json:"session_id"`
}

type wsMsgUnsubscribe struct {
	SessionID string `json:"session_id"`
}

type wsMsgUserMessage struct {
	SessionID string          `json:"session_id"`
	Content   json.RawMessage `json:"content"` // string or array (multipart)
	Files     []wsUserFile    `json:"files,omitempty"`
}

type wsUserFile struct {
	Name    string `json:"name"`
	DataURL string `json:"data_url,omitempty"`
}

type wsMsgInterrupt struct {
	SessionID string `json:"session_id"`
}

type wsMsgConfirmation struct {
	ConfID string `json:"conf_id"`
	Result string `json:"result"`
}

type wsMsgUserQuestionAnswer struct {
	QuestionID string   `json:"question_id"`
	Choices    []string `json:"choices,omitempty"`
	Custom     string   `json:"custom,omitempty"`
	Cancelled  bool     `json:"cancelled,omitempty"`
}

type wsMsgRetry struct {
	SessionID string `json:"session_id"`
}

type wsMsgRunTask struct {
	SessionID string `json:"session_id"`
}

type wsMsgUpdateSettings struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// ─── Server → Client events (outbound) ────────────────────────────────────

// wsOutEvent is the base envelope for every server-sent event.
type wsOutEvent struct {
	Type string `json:"type"`
}

// wsEventSessionList carries the full session list sent on connect + refresh.
type wsEventSessionList struct {
	Type     string          `json:"type"`
	Sessions []wsSessionInfo `json:"sessions"`
}

type wsSessionInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status,omitempty"` // "idle" | "working"
	CreatedAt       int64  `json:"created_at"`       // unix ms
	Source          string `json:"source,omitempty"` // "manual" | "cron" | "channel" | "setup"
	Model           string `json:"model,omitempty"`
	TotalTurns      int    `json:"total_turns,omitempty"`
	WorkingDir      string `json:"working_dir,omitempty"`
	PermissionMode  string `json:"permission_mode,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ContextUsage    int    `json:"context_usage,omitempty"`
}

type wsEventHistoryUserMessage struct {
	Type      string   `json:"type"`
	Content   string   `json:"content"`
	CreatedAt int64    `json:"created_at,omitempty"`
	Images    []string `json:"images,omitempty"`
}

type wsEventAssistantMessage struct {
	Type    string   `json:"type"`
	Content string   `json:"content"`
	Files   []string `json:"files,omitempty"`
}

type wsEventToolCall struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Args    any    `json:"args"`
	Summary string `json:"summary,omitempty"`
}

type wsEventToolResult struct {
	Type      string `json:"type"`
	Result    string `json:"result"`
	UIPayload any    `json:"ui_payload,omitempty"`
}

type wsEventToolError struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

type wsEventToolStdout struct {
	Type  string   `json:"type"`
	Lines []string `json:"lines"`
}

type wsEventOutput struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type wsEventProgress struct {
	Type         string  `json:"type"`
	Message      string  `json:"message,omitempty"`
	ProgressType string  `json:"progress_type,omitempty"`
	Phase        string  `json:"phase"` // "active" | "done"
	Status       string  `json:"status,omitempty"`
	StartedAt    int64   `json:"started_at,omitempty"` // unix ms
	Elapsed      float64 `json:"elapsed,omitempty"`
}

type wsEventComplete struct {
	Type                 string  `json:"type"`
	Iterations           int     `json:"iterations"`
	Duration             float64 `json:"duration,omitempty"`
	CacheStats           any     `json:"cache_stats,omitempty"`
	AwaitingUserFeedback bool    `json:"awaiting_user_feedback,omitempty"`
}

type wsEventSessionUpdate struct {
	Type            string  `json:"type"`
	Status          string  `json:"status,omitempty"`
	Tasks           int     `json:"tasks,omitempty"`
	Latency         float64 `json:"latency,omitempty"`
	ContextUsage    int     `json:"context_usage,omitempty"` // 0–100 context window %
	WorkingDir      string  `json:"working_dir,omitempty"`
	PermissionMode  string  `json:"permission_mode,omitempty"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
}

type wsEventTodoUpdate struct {
	Type  string `json:"type"`
	Todos any    `json:"todos"`
}

type wsEventSessionDeleted struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

type wsEventRequestFeedback struct {
	Type     string   `json:"type"`
	Question string   `json:"question"`
	Context  string   `json:"context,omitempty"`
	Options  []string `json:"options,omitempty"`
}

type wsEventRequestConfirmation struct {
	Type    string `json:"type"`
	ConfID  string `json:"conf_id"`
	Message string `json:"message"`
	Kind    string `json:"kind"` // "yes_no" | "ok"
}

type wsEventRequestUserQuestion struct {
	Type        string   `json:"type"`
	QuestionID  string   `json:"question_id"`
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	MultiSelect bool     `json:"multi_select"`
	Header      string   `json:"header,omitempty"`
}

type wsEventBackgroundTaskUpdate struct {
	Type    string             `json:"type"`
	Running int                `json:"running"`
	Tasks   []wsBackgroundTask `json:"tasks"`
}

type wsBackgroundTask struct {
	HandleID string `json:"handle_id"`
	Command  string `json:"command"`
	Elapsed  int    `json:"elapsed"`
}

type wsEventBackgroundTaskNotice struct {
	Type     string `json:"type"`
	Command  string `json:"command"`
	HandleID string `json:"handle_id"`
	Status   string `json:"status"`
}

type wsEventUserMessageQueueStatus struct {
	Type    string `json:"type"`
	Pending int    `json:"pending"`
}

type wsEventNextMessageSuggestion struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wsEventDiff struct {
	Type      string `json:"type"`
	OldSize   int    `json:"old_size"`
	NewSize   int    `json:"new_size"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated,omitempty"`
}

type wsEventFilePreview struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
	IsNewFile bool   `json:"is_new_file,omitempty"`
}

type wsEventShellPreview struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}
