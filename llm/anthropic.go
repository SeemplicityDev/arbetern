package llm

// ---------------------------------------------------------------------------
// Anthropic Messages API protocol adapter.
//
// Azure Foundry hosts Claude models behind the native Anthropic Messages API
// at {services-host}/anthropic/v1/messages (authenticated with the Azure API
// key sent as `x-api-key`). That protocol differs from OpenAI Chat
// Completions in several places:
//
//   - `system` is a top-level string, not a message with role "system".
//   - Message content may be either a string or an array of typed blocks
//     ({type: "text"|"tool_use"|"tool_result", ...}).
//   - Tools are defined with `{name, description, input_schema}` (no outer
//     "function" wrapper, no "type: function" field).
//   - Tool invocations are reported as `tool_use` content blocks on the
//     assistant message; tool results are fed back as `tool_result` blocks
//     on a user message.
//   - The response carries `usage.input_tokens` / `usage.output_tokens`
//     rather than prompt/completion counts.
//
// The translators below convert between the internal ChatMessage / Tool /
// ChatResponse types and this wire format so the rest of the codebase can
// remain backend-agnostic.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// anthropicRequest is the wire body for POST /anthropic/v1/messages.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    interface{}        `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

// anthropicMessage is a single turn. Content may be string or block array.
type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// anthropicTool mirrors Anthropic's tool-definition schema.
type anthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl interface{}     `json:"cache_control,omitempty"`
}

// anthropicResponse is the wire body returned by the Messages API.
type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      *anthropicUsage         `json:"usage,omitempty"`
	Error      *anthropicResponseError `json:"error,omitempty"`
}

type anthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicResponseError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// chatToolsToAnthropicTools converts the internal Tool list to Anthropic's
// flat tool-definition schema.
func chatToolsToAnthropicTools(tools []Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out
}

// ephemeralCache is the Anthropic prompt-cache breakpoint marker. A breakpoint
// caches the entire prefix up to and including the block it is attached to.
var ephemeralCache = map[string]string{"type": "ephemeral"}

// promptCacheEnabled reports whether Anthropic prompt caching is on (default
// true). Set LLM_PROMPT_CACHE=false to disable as a kill-switch.
func promptCacheEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("LLM_PROMPT_CACHE")), "false")
}

// markLastMessageCache attaches a cache breakpoint to the final content block
// of the last message, so each tool-loop round can re-read the conversation
// prefix accumulated so far at the cache-read rate. String content is promoted
// to a single text block to carry the marker.
func markLastMessageCache(msgs []anthropicMessage) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	switch c := last.Content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return
		}
		last.Content = []map[string]interface{}{{"type": "text", "text": c, "cache_control": ephemeralCache}}
	case []map[string]interface{}:
		if len(c) == 0 {
			return
		}
		c[len(c)-1]["cache_control"] = ephemeralCache
	}
}

// chatMessagesToAnthropic converts the unified ChatMessage list to the
// Anthropic system-string + messages-array layout.
//
// Role mapping:
//   - "system"       → appended to the top-level `system` string (joined by
//     blank lines if multiple are present).
//   - "user"/"assistant" → emitted verbatim with string content.
//   - "tool"         → emitted as a user message carrying a single
//     `tool_result` block keyed by ToolCallID.
//
// Assistant messages that include ToolCalls are emitted with a structured
// content array containing an optional text block followed by one `tool_use`
// block per call.
func chatMessagesToAnthropic(messages []ChatMessage) (string, []anthropicMessage) {
	var systemParts []string
	out := make([]anthropicMessage, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "system":
			if s := strings.TrimSpace(m.Content); s != "" {
				systemParts = append(systemParts, s)
			}
		case "tool":
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []map[string]interface{}{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
				}},
			})
		case "assistant":
			if len(m.ToolCalls) == 0 {
				out = append(out, anthropicMessage{Role: "assistant", Content: m.Content})
				continue
			}
			blocks := make([]map[string]interface{}, 0, len(m.ToolCalls)+1)
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				var input json.RawMessage
				if tc.Function.Arguments != "" {
					input = json.RawMessage(tc.Function.Arguments)
				} else {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		default: // "user" and anything else
			out = append(out, anthropicMessage{Role: "user", Content: m.Content})
		}
	}

	return strings.Join(systemParts, "\n\n"), out
}

// anthropicResponseToChat translates the Messages API reply into the shared
// ChatResponse shape used by the tool-use loop.
func anthropicResponseToChat(r *anthropicResponse) *ChatResponse {
	var text strings.Builder
	var toolCalls []ToolCall
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			tc := ToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			tc.Function.Arguments = args
			toolCalls = append(toolCalls, tc)
		}
	}

	resp := &ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}{
		Message: struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		}{Content: text.String(), ToolCalls: toolCalls},
		FinishReason: r.StopReason,
	})
	if r.Usage != nil {
		// PromptTokens are full-rate input (fresh input + cache writes); cache
		// reads are tracked separately so they can be priced at the provider's
		// steep cache-read discount. TotalTokens covers every category.
		resp.Usage = &Usage{
			PromptTokens:       r.Usage.InputTokens + r.Usage.CacheCreationInputTokens,
			CachedPromptTokens: r.Usage.CacheReadInputTokens,
			CompletionTokens:   r.Usage.OutputTokens,
			TotalTokens:        r.Usage.InputTokens + r.Usage.CacheCreationInputTokens + r.Usage.CacheReadInputTokens + r.Usage.OutputTokens,
		}
	}
	return resp
}

// doAnthropic calls Foundry's Anthropic Messages API for Claude deployments.
func (c *Client) doAnthropic(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	system, anthMessages := chatMessagesToAnthropic(messages)
	anthTools := chatToolsToAnthropicTools(tools)

	// Prompt caching: place breakpoints on the static prefix (tool schemas +
	// system prompt) and on the rolling conversation tail. In a tool-loop the
	// same large prefix is re-sent every round, so caching lets rounds 2..N
	// read it at the provider's ~0.1x cache-read rate instead of full price.
	// This is quality-neutral — the model sees identical input; only billing
	// and latency improve. Disable with LLM_PROMPT_CACHE=false.
	var systemField interface{}
	if s := strings.TrimSpace(system); s != "" {
		if promptCacheEnabled() {
			systemField = []map[string]interface{}{{"type": "text", "text": system, "cache_control": ephemeralCache}}
		} else {
			systemField = system
		}
	}
	if promptCacheEnabled() {
		if n := len(anthTools); n > 0 {
			anthTools[n-1].CacheControl = ephemeralCache
		}
		markLastMessageCache(anthMessages)
	}

	reqBody := anthropicRequest{
		Model:     c.model,
		MaxTokens: anthropicMaxTokens,
		System:    systemField,
		Messages:  anthMessages,
		Tools:     anthTools,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal anthropic request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/anthropic/v1/messages", c.azureServicesEndpoint())

	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         c.azureAPIKey,
		"anthropic-version": anthropicAPIVersion,
	}

	body, statusCode, err := c.doPostWithRetry(ctx, apiURL, payload, headers, "anthropic")
	if err != nil {
		return nil, err
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic API returned %d: %s", statusCode, extractAPIErrorMessage(body))
	}

	var anthResp anthropicResponse
	if err := json.Unmarshal(bytes.TrimSpace(body), &anthResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal anthropic response: %w", err)
	}
	if anthResp.Error != nil {
		return nil, fmt.Errorf("anthropic API error: %s", anthResp.Error.Message)
	}

	return anthropicResponseToChat(&anthResp), nil
}
