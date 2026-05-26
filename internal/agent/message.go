// Package agent implements the Octo agent core: messages, history, and the
// run loop that ties an LLM provider to user input.
//
// The package is intentionally thin in M1.2 — it covers the "send a user
// message, receive an assistant reply" path with no streaming, no tool use,
// and no multi-turn cancellation. Those land in M2 / M3 alongside the
// provider streaming aggregators and the tool registry.
package agent

// Role is the message author role, mirroring the role string used by both
// supported LLM API protocols (Anthropic Messages, OpenAI Chat Completions).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn in the conversation.
//
// Content carries plain text for simple turns. Blocks carries the richer
// multi-part form used when the assistant performs tool calls or when the user
// returns tool results. When both fields are set the provider adapter should
// prefer Blocks; when only Content is set the adapter encodes it as a single
// text block.
type Message struct {
	Role    Role           `json:"role"`
	Content string         `json:"content,omitempty"`
	Blocks  []ContentBlock `json:"blocks,omitempty"`
}

// NewUserMessage constructs a Message with RoleUser.
func NewUserMessage(content string) Message {
	return Message{Role: RoleUser, Content: content}
}

// NewAssistantMessage constructs a Message with RoleAssistant.
func NewAssistantMessage(content string) Message {
	return Message{Role: RoleAssistant, Content: content}
}

// NewSystemMessage constructs a Message with RoleSystem.
//
// Note: Anthropic's Messages API takes the system prompt as a top-level field
// rather than as a role within the messages array; the agent's provider
// adapter is responsible for that translation.
func NewSystemMessage(content string) Message {
	return Message{Role: RoleSystem, Content: content}
}

// NewToolUseMessage constructs an assistant Message whose content is a slice
// of blocks that may include tool_use blocks. The blocks slice typically
// contains one or more tool_use blocks (and optionally a preceding text block
// with the model's reasoning).
func NewToolUseMessage(blocks []ContentBlock) Message {
	return Message{Role: RoleAssistant, Blocks: blocks}
}

// NewToolResultMessage constructs a user Message that carries tool execution
// results back to the model. Each element of results must be a tool_result
// block whose ToolUseID matches a tool_use block in the preceding assistant
// message.
func NewToolResultMessage(results []ContentBlock) Message {
	return Message{Role: RoleUser, Blocks: results}
}
