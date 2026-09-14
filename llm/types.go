// Package llm provides the LLM inference client and shared tool/message types
// used across all arbetern integrations. It supports GitHub Models, Azure
// OpenAI, and AWS Bedrock backends.
package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
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
	// Volatile marks system content that changes from one turn to the next
	// (retrieved memory, recent channel messages, freshly fetched logs). It is
	// emitted after the prompt-cache breakpoint so it cannot invalidate the
	// cached prefix. Ignored by backends without prompt caching, which simply
	// concatenate it onto the system prompt as before.
	Volatile bool `json:"-"`
}

// Usage tracks token consumption from a single LLM API call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedPromptTokens are prompt tokens served from the provider's prompt
	// cache at a steep discount (e.g. Anthropic ~0.1x). They are billed far
	// cheaper than fresh PromptTokens, so they are tracked separately for
	// accurate cost estimation. Not included in PromptTokens.
	CachedPromptTokens int `json:"cached_prompt_tokens,omitempty"`
	// CacheWriteTokens are prompt tokens written into the provider's cache on
	// this call. They cost *more* than fresh input (Anthropic charges 1.25x at
	// the 5-minute TTL), so they are priced separately rather than folded into
	// PromptTokens. Not included in PromptTokens.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Add accumulates another round's usage.
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CachedPromptTokens += o.CachedPromptTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
}

// CompressionStats reports the tokens Headroom removed from an LLM request's
// messages before the provider call. Zero-valued when compression made no change.
type CompressionStats struct {
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	TokensSaved  int `json:"tokens_saved"`
}

// Add accumulates another round's savings.
func (c *CompressionStats) Add(o CompressionStats) {
	c.TokensBefore += o.TokensBefore
	c.TokensAfter += o.TokensAfter
	c.TokensSaved += o.TokensSaved
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
	// Compression, when non-nil, reports Headroom savings applied to this
	// request. Set by CompleteWithTools; never decoded from a provider response.
	Compression *CompressionStats `json:"-"`
	Error       *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewChatMessage creates a ChatMessage with the given role and content.
func NewChatMessage(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: content}
}

// NewVolatileSystemMessage creates system content that is expected to differ
// on every turn. See ChatMessage.Volatile.
func NewVolatileSystemMessage(content string) ChatMessage {
	return ChatMessage{Role: "system", Content: content, Volatile: true}
}

// NewToolResultMessage creates a tool-result message that feeds a function
// call's output back into the conversation.
func NewToolResultMessage(toolCallID, content string) ChatMessage {
	return ChatMessage{Role: "tool", Content: content, ToolCallID: toolCallID}
}

// FormatUsageStamp returns a short Slack-formatted line showing token usage,
// model metadata and how long the turn took. Enabled by default; set
// SHOW_USAGE_STAMP=false to disable.
func FormatUsageStamp(u *Usage, model string, elapsed time.Duration) string {
	if u == nil || u.TotalTokens == 0 {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SHOW_USAGE_STAMP")), "false") {
		return ""
	}
	stamp := fmt.Sprintf("\n\n_:bar_chart: %s | tokens: %d", model, u.TotalTokens)
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		stamp += fmt.Sprintf(" (in: %d, out: %d)", u.PromptTokens, u.CompletionTokens)
	}
	if elapsed > 0 {
		stamp += " | " + elapsed.Round(time.Second).String()
	}
	return stamp + "_"
}
