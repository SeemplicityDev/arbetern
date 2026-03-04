// Package llm provides the LLM inference client and shared tool/message types
// used across all arbetern integrations. It supports both GitHub Models and
// Azure OpenAI backends.
package llm

import "encoding/json"

// Tool describes a function the LLM can invoke during a tool-use loop.
// This struct is integration-agnostic — every integration (GitHub, Jira,
// Salesforce, Chorus, NVD, …) contributes tools through the same type.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function metadata nested inside a Tool definition.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall represents a single function invocation requested by the LLM.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatMessage is the unified message format used throughout the tool-use loop.
// It maps to both the Chat Completions and Azure Responses API formats.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ChatResponse wraps the LLM's reply, normalised from either the Chat
// Completions or Responses API format into a single struct.
type ChatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewChatMessage creates a ChatMessage with the given role and content.
func NewChatMessage(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: content}
}

// NewToolResultMessage creates a tool-result message that feeds a function
// call's output back into the conversation.
func NewToolResultMessage(toolCallID, content string) ChatMessage {
	return ChatMessage{Role: "tool", Content: content, ToolCallID: toolCallID}
}
