package llm

// ---------------------------------------------------------------------------
// Azure Responses API protocol types and conversion helpers.
//
// The Responses API (/openai/responses) is the modern Azure OpenAI endpoint
// used by gpt-5.x and codex models. It has a different request/response
// schema than Chat Completions: tools are flat (not nested under "function"),
// messages are "input items", and replies are "output items".
//
// The helpers in this file translate between the internal ChatMessage/Tool
// types and the Responses API wire format so the rest of the codebase can
// stay backend-agnostic.
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"strings"
)

// responsesRequest is the request body for the Azure Responses API.
type responsesRequest struct {
	Input        []responsesInputItem `json:"input"`
	Instructions string               `json:"instructions,omitempty"`
	Model        string               `json:"model"`
	Tools        []responsesTool      `json:"tools,omitempty"`
	Truncation   string               `json:"truncation,omitempty"` // "auto" or "disabled"
}

// responsesTool is the tool definition format for the Azure Responses API.
// Unlike Chat Completions (which nests under "function"), the Responses API
// expects name/description/parameters at the top level.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatToolsToResponsesTools converts Chat Completions tool definitions to the
// flat format expected by the Azure Responses API.
func chatToolsToResponsesTools(tools []Tool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responsesTool, len(tools))
	for i, t := range tools {
		out[i] = responsesTool{
			Type:        t.Type,
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
	}
	return out
}

// responsesInputItem can represent a user/assistant message, a function_call,
// or a function_call_output.
type responsesInputItem struct {
	// Common fields
	Type string `json:"type,omitempty"` // "message", "function_call", "function_call_output"
	Role string `json:"role,omitempty"` // for type "message"

	// For type "message" — content can be a string or structured.
	Content string `json:"content,omitempty"`

	// For type "function_call"
	ID        string `json:"id,omitempty"` // function call ID
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// For type "function_call_output"
	Output string `json:"output,omitempty"`
}

// responsesResponse is the response body from the Azure Responses API.
type responsesResponse struct {
	ID     string                `json:"id"`
	Output []responsesOutputItem `json:"output"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"` // "message" or "function_call"
	Role    string                   `json:"role,omitempty"`
	Content []responsesOutputContent `json:"content,omitempty"` // for type "message"

	// For type "function_call"
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responsesOutputContent struct {
	Type string `json:"type"` // "output_text"
	Text string `json:"text"`
}

// chatMessagesToResponsesInput converts the internal ChatMessage slice into
// Responses API input items. The first "system" message is extracted as
// the instructions string.
func chatMessagesToResponsesInput(msgs []ChatMessage) (instructions string, items []responsesInputItem) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if instructions == "" {
				instructions = m.Content
			} else {
				instructions += "\n\n" + m.Content
			}
		case "user":
			items = append(items, responsesInputItem{
				Type:    "message",
				Role:    "user",
				Content: m.Content,
			})
		case "assistant":
			if len(m.ToolCalls) > 0 {
				// Each tool call becomes a separate function_call input item.
				for _, tc := range m.ToolCalls {
					items = append(items, responsesInputItem{
						Type:      "function_call",
						CallID:    tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					})
				}
			} else {
				items = append(items, responsesInputItem{
					Type:    "message",
					Role:    "assistant",
					Content: m.Content,
				})
			}
		case "tool":
			items = append(items, responsesInputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		}
	}
	return
}

// responsesOutputToChatResponse converts a Responses API response into
// the internal ChatResponse format so the rest of the codebase is unchanged.
func responsesOutputToChatResponse(rr *responsesResponse) *ChatResponse {
	cr := &ChatResponse{}
	if rr.Error != nil {
		cr.Error = &struct {
			Message string `json:"message"`
		}{Message: rr.Error.Message}
		return cr
	}

	var choice struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}

	var textParts []string
	for _, item := range rr.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					textParts = append(textParts, c.Text)
				}
			}
		case "function_call":
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}

	choice.Message.Content = strings.Join(textParts, "")
	if len(choice.Message.ToolCalls) > 0 {
		choice.FinishReason = "tool_calls"
	} else {
		choice.FinishReason = "stop"
	}

	cr.Choices = append(cr.Choices, choice)
	return cr
}
