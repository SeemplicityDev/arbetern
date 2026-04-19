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
	"strings"
)

// anthropicRequest is the wire body for POST /anthropic/v1/messages.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
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
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
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
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
		resp.Usage = &Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
		}
	}
	return resp
}

// doAnthropic calls Foundry's Anthropic Messages API for Claude deployments.
func (c *Client) doAnthropic(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	system, anthMessages := chatMessagesToAnthropic(messages)

	reqBody := anthropicRequest{
		Model:     c.model,
		MaxTokens: anthropicMaxTokens,
		System:    system,
		Messages:  anthMessages,
		Tools:     chatToolsToAnthropicTools(tools),
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
