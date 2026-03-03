package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const modelsAPIURL = "https://models.github.ai/inference/chat/completions"

// azureAPIVersion is the Azure OpenAI REST API version to use for chat completions.
const azureAPIVersion = "2024-10-21"

// azureResponsesAPIVersion is the API version for the Responses API (codex models).
const azureResponsesAPIVersion = "2025-04-01-preview"

// maxResponseBody is the upper bound on response body reads to prevent OOM from
// unexpectedly large upstream responses (10 MB).
const maxResponseBody = 10 << 20

type ModelsClient struct {
	token      string
	model      string
	httpClient *http.Client

	// Azure OpenAI fields (empty when using GitHub Models).
	azureEndpoint string
	azureAPIKey   string
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

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

func NewModelsClient(token, model string) *ModelsClient {
	return &ModelsClient{
		token:      token,
		model:      model,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// NewAzureModelsClient creates a ModelsClient backed by Azure OpenAI.
// The deployment parameter is used as the model/deployment name in the URL.
func NewAzureModelsClient(endpoint, apiKey, deployment string) *ModelsClient {
	endpoint = strings.TrimRight(endpoint, "/")
	return &ModelsClient{
		model:         deployment,
		httpClient:    &http.Client{Timeout: 120 * time.Second},
		azureEndpoint: endpoint,
		azureAPIKey:   apiKey,
	}
}

// useAzure returns true when the client is configured for Azure OpenAI.
func (m *ModelsClient) useAzure() bool {
	return m.azureEndpoint != "" && m.azureAPIKey != ""
}

// Model returns the model/deployment name this client is using.
func (m *ModelsClient) Model() string {
	return m.model
}

func (m *ModelsClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	if m.isResponsesModel() {
		resp, err := m.doResponses(ctx, messages, nil)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("responses API returned no output")
		}
		return resp.Choices[0].Message.Content, nil
	}

	resp, err := m.doChat(ctx, messages, nil)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("GitHub Models returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

func (m *ModelsClient) CompleteWithTools(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	if m.isResponsesModel() {
		return m.doResponses(ctx, messages, tools)
	}
	return m.doChat(ctx, messages, tools)
}

func (m *ModelsClient) doChat(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	reqBody := chatRequest{
		Model:    m.model,
		Messages: messages,
		Tools:    tools,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var apiURL string
	if m.useAzure() {
		apiURL = fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			m.azureEndpoint, m.model, azureAPIVersion)
	} else {
		apiURL = modelsAPIURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if m.useAzure() {
		req.Header.Set("api-key", m.azureAPIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LLM API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API returned %d: %s", resp.StatusCode, string(body))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("LLM API error: %s", chatResp.Error.Message)
	}

	return &chatResp, nil
}

// ---------------------------------------------------------------------------
// Azure Responses API support (for codex / non-chat-completions models)
// ---------------------------------------------------------------------------

// isResponsesModel returns true when the deployment uses the Azure Responses API
// (/openai/responses) rather than the legacy Chat Completions endpoint.
// All current Azure OpenAI deployments (gpt-5.x, codex, etc.) use this API.
func (m *ModelsClient) isResponsesModel() bool {
	return m.useAzure()
}

// responsesRequest is the request body for the Azure Responses API.
type responsesRequest struct {
	Input        []responsesInputItem `json:"input"`
	Instructions string               `json:"instructions,omitempty"`
	Model        string               `json:"model"`
	Tools        []responsesTool      `json:"tools,omitempty"`
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

// doResponses calls the Azure Responses API (/responses) for codex models.
func (m *ModelsClient) doResponses(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	instructions, items := chatMessagesToResponsesInput(messages)

	reqBody := responsesRequest{
		Input:        items,
		Instructions: instructions,
		Model:        m.model,
		Tools:        chatToolsToResponsesTools(tools),
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal responses request: %w", err)
	}

	if len(tools) > 0 {
		log.Printf("[responses] sending %d tools, first tool: name=%q type=%q", len(reqBody.Tools), reqBody.Tools[0].Name, reqBody.Tools[0].Type)
	}

	apiURL := fmt.Sprintf("%s/openai/responses?api-version=%s",
		m.azureEndpoint, azureResponsesAPIVersion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create responses request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", m.azureAPIKey)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("responses API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("failed to read responses body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("responses API returned %d: %s", resp.StatusCode, string(body))
	}

	var rr responsesResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal responses: %w", err)
	}

	if rr.Error != nil {
		return nil, fmt.Errorf("responses API error: %s", rr.Error.Message)
	}

	return responsesOutputToChatResponse(&rr), nil
}

func NewChatMessage(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: content}
}

func NewToolResultMessage(toolCallID, content string) ChatMessage {
	return ChatMessage{Role: "tool", Content: content, ToolCallID: toolCallID}
}

// AzureModel describes a model returned by the Azure OpenAI /models endpoint.
type AzureModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created_at,omitempty"`
}

// ValidateModel verifies that the configured model/deployment is accessible
// by sending a minimal completion request. This works for both Azure
// deployments (whose names don't appear in the /openai/models list) and
// GitHub Models.
func (m *ModelsClient) ValidateModel(ctx context.Context) error {
	_, err := m.Complete(ctx, "ping", "reply with ok")
	if err != nil {
		return fmt.Errorf("model/deployment %q is not accessible: %w", m.model, err)
	}
	return nil
}

// ListModels queries the Azure OpenAI /openai/models endpoint and returns
// the model IDs accessible with the configured API key. Returns nil for
// non-Azure clients.
func (m *ModelsClient) ListModels(ctx context.Context) ([]string, error) {
	if !m.useAzure() {
		return nil, nil
	}

	apiURL := fmt.Sprintf("%s/openai/models?api-version=%s", m.azureEndpoint, azureAPIVersion)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("api-key", m.azureAPIKey)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models endpoint returned %d", resp.StatusCode)
	}

	var result struct {
		Data []AzureModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}

	ids := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// Endpoint returns the Azure endpoint URL, or empty for non-Azure clients.
func (m *ModelsClient) Endpoint() string {
	return m.azureEndpoint
}
