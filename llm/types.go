// Package llm provides the LLM inference client and shared tool/message types
// used across all arbetern integrations. It supports both GitHub Models and
// Azure OpenAI backends.
package llm

import (
	"encoding/json"
	"fmt"
)

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

// Usage tracks token consumption from a single LLM API call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
	Usage *Usage `json:"usage,omitempty"`
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

// FormatUsageStamp returns a short Slack-formatted line showing token usage and
// estimated cost. Returns an empty string if usage is nil.
func FormatUsageStamp(u *Usage, model string) string {
	if u == nil || u.TotalTokens == 0 {
		return ""
	}
	// Rough cost estimates per 1K tokens (input/output blended).
	// These are approximate and will need updating as pricing changes.
	costPer1K := 0.005 // default fallback
	switch {
	case contains(model, "codex"):
		costPer1K = 0.015
	case contains(model, "gpt-5"):
		costPer1K = 0.01
	case contains(model, "gpt-4o"):
		costPer1K = 0.005
	case contains(model, "gpt-4"):
		costPer1K = 0.01
	}
	cost := float64(u.TotalTokens) / 1000.0 * costPer1K
	return fmt.Sprintf("\n\n_:bar_chart: %s | tokens: %d (in: %d, out: %d) | ~$%.4f_",
		model, u.TotalTokens, u.PromptTokens, u.CompletionTokens, cost)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && stringContains(s, substr)
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
