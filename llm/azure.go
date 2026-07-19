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
//
// responsesInputItem can represent a user/assistant message, a function_call,
// or a function_call_output. Each type has a different set of required fields,
// so we use a custom MarshalJSON to emit only the fields relevant to each type.
// This avoids two failure modes:
//   - omitempty on Output/CallID/Arguments drops required fields → 400
//     "Missing required parameter: 'input[N].output'"
//   - no omitempty sends call_id/arguments/output on message items → 400
//     "Unknown parameter: 'input[N].call_id'"
type responsesInputItem struct {
	// Common fields
	Type string // "message", "function_call", "function_call_output"
	Role string // for type "message"

	// For type "message" — content can be a string or structured.
	Content string

	// For type "function_call"
	ID        string // function call ID
	CallID    string
	Name      string
	Arguments string

	// For type "function_call_output"
	Output string
}

// MarshalJSON serialises only the fields the Responses API expects for each
// item type, ensuring required fields are always present and unknown fields
// are never sent.
func (r responsesInputItem) MarshalJSON() ([]byte, error) {
	switch r.Type {
	case "function_call":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id,omitempty"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{r.Type, r.ID, r.CallID, r.Name, r.Arguments})

	case "function_call_output":
		return json.Marshal(struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}{r.Type, r.CallID, r.Output})

	default: // "message"
		return json.Marshal(struct {
			Type    string `json:"type"`
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		}{r.Type, r.Role, r.Content})
	}
}

// responsesResponse is the response body from the Azure Responses API.
type responsesResponse struct {
	ID     string                `json:"id"`
	Output []responsesOutputItem `json:"output"`
	Usage  *Usage                `json:"usage,omitempty"`
	// Status is the run status ("completed", "incomplete", "failed", …).
	// IncompleteDetails.Reason explains an "incomplete" run — "max_output_tokens"
	// means the turn was truncated at the output ceiling. Both are needed so a
	// truncated turn is reported as such rather than as a normal "stop".
	Status            string `json:"status,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason,omitempty"`
	} `json:"incomplete_details,omitempty"`
	Error *struct {
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
	switch {
	case rr.Status == "incomplete" && rr.IncompleteDetails != nil && rr.IncompleteDetails.Reason != "":
		// Cut short (e.g. "max_output_tokens"). Surface the real reason so the
		// tool loop can react instead of mistaking it for a normal completion.
		choice.FinishReason = rr.IncompleteDetails.Reason
	case len(choice.Message.ToolCalls) > 0:
		choice.FinishReason = "tool_calls"
	default:
		choice.FinishReason = "stop"
	}

	cr.Choices = append(cr.Choices, choice)
	cr.Usage = rr.Usage
	return cr
}
