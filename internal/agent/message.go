// Package agent implements the Octo agent core: messages, history, and the
// run loop that ties an LLM provider to user input.
//
// The package is intentionally thin in M1.2 — it covers the "send a user
// message, receive an assistant reply" path with no streaming, no tool use,
// and no multi-turn cancellation. Those land in M2 / M3 alongside the
// provider streaming aggregators and the tool registry.
package agent

// Role is the message author role, mirroring the role string used by all three
// LLM API protocols (Anthropic Messages, OpenAI, Bedrock).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn in the conversation. Content is plain text for now;
// when M2 adds tool calls and image attachments this expands into a richer
// content-block type, but the wire-level shape stays compatible.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
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
