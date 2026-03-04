package llm

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

// azureAPIVersion is the Azure OpenAI REST API version for chat completions.
const azureAPIVersion = "2024-10-21"

// azureResponsesAPIVersion is the API version for the Responses API (codex models).
const azureResponsesAPIVersion = "2025-04-01-preview"

// maxResponseBody is the upper bound on response body reads to prevent OOM from
// unexpectedly large upstream responses (10 MB).
const maxResponseBody = 10 << 20

// Client provides LLM inference through either GitHub Models or Azure OpenAI.
// The backend is selected at construction time; the rest of the codebase uses
// the same Complete / CompleteWithTools interface regardless of backend.
type Client struct {
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

// NewClient creates an LLM client backed by GitHub Models.
func NewClient(token, model string) *Client {
	return &Client{
		token:      token,
		model:      model,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// NewAzureClient creates an LLM client backed by Azure OpenAI.
// The deployment parameter is used as the model/deployment name in the URL.
func NewAzureClient(endpoint, apiKey, deployment string) *Client {
	endpoint = strings.TrimRight(endpoint, "/")
	return &Client{
		model:         deployment,
		httpClient:    &http.Client{Timeout: 120 * time.Second},
		azureEndpoint: endpoint,
		azureAPIKey:   apiKey,
	}
}

// useAzure returns true when the client is configured for Azure OpenAI.
func (c *Client) useAzure() bool {
	return c.azureEndpoint != "" && c.azureAPIKey != ""
}

// Model returns the model/deployment name this client is using.
func (c *Client) Model() string {
	return c.model
}

// Complete sends a simple system+user prompt pair and returns the text response.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	if c.isResponsesModel() {
		resp, err := c.doResponses(ctx, messages, nil)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("responses API returned no output")
		}
		return resp.Choices[0].Message.Content, nil
	}

	resp, err := c.doChat(ctx, messages, nil)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("GitHub Models returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// CompleteWithTools sends messages and tool definitions, returning the LLM's
// response which may include tool calls to execute.
func (c *Client) CompleteWithTools(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	if c.isResponsesModel() {
		return c.doResponses(ctx, messages, tools)
	}
	return c.doChat(ctx, messages, tools)
}

func (c *Client) doChat(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	reqBody := chatRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var apiURL string
	if c.useAzure() {
		apiURL = fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			c.azureEndpoint, c.model, azureAPIVersion)
	} else {
		apiURL = modelsAPIURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.useAzure() {
		req.Header.Set("api-key", c.azureAPIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LLM API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API returned %d: %s", resp.StatusCode, extractAPIErrorMessage(body))
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

// ValidateModel verifies that the configured model/deployment is accessible
// by sending a minimal completion request.
func (c *Client) ValidateModel(ctx context.Context) error {
	_, err := c.Complete(ctx, "ping", "reply with ok")
	if err != nil {
		return fmt.Errorf("model/deployment %q is not accessible: %w", c.model, err)
	}
	return nil
}

// AzureModel describes a model returned by the Azure OpenAI /models endpoint.
type AzureModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created_at,omitempty"`
}

// ListModels queries the Azure OpenAI /openai/models endpoint and returns
// the model IDs accessible with the configured API key. Returns nil for
// non-Azure clients.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	if !c.useAzure() {
		return nil, nil
	}

	apiURL := fmt.Sprintf("%s/openai/models?api-version=%s", c.azureEndpoint, azureAPIVersion)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("api-key", c.azureAPIKey)

	resp, err := c.httpClient.Do(req)
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
func (c *Client) Endpoint() string {
	return c.azureEndpoint
}

// isResponsesModel returns true when the deployment uses the Azure Responses API
// (/openai/responses) rather than the legacy Chat Completions endpoint.
// All current Azure OpenAI deployments (gpt-5.x, codex, etc.) use this API.
func (c *Client) isResponsesModel() bool {
	return c.useAzure()
}

// doResponses calls the Azure Responses API (/responses) for codex models.
func (c *Client) doResponses(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	instructions, items := chatMessagesToResponsesInput(messages)

	reqBody := responsesRequest{
		Input:        items,
		Instructions: instructions,
		Model:        c.model,
		Tools:        chatToolsToResponsesTools(tools),
		Truncation:   "auto",
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal responses request: %w", err)
	}

	if len(tools) > 0 {
		log.Printf("[responses] sending %d tools, first tool: name=%q type=%q", len(reqBody.Tools), reqBody.Tools[0].Name, reqBody.Tools[0].Type)
	}

	apiURL := fmt.Sprintf("%s/openai/responses?api-version=%s",
		c.azureEndpoint, azureResponsesAPIVersion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create responses request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.azureAPIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("responses API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("failed to read responses body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("responses API returned %d: %s", resp.StatusCode, extractAPIErrorMessage(body))
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
