package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const modelsAPIURL = "https://models.github.ai/inference/chat/completions"

// azureAPIVersion is the Azure OpenAI REST API version for chat completions.
const azureAPIVersion = "2024-10-21"

// azureResponsesAPIVersion is the API version for the Responses API (codex models).
const azureResponsesAPIVersion = "2025-04-01-preview"

// anthropicAPIVersion is the Anthropic Messages API version header value sent
// to Foundry's /anthropic/v1/messages endpoint for Claude deployments.
const anthropicAPIVersion = "2023-06-01"

// anthropicMaxTokens bounds the completion length for Anthropic Messages API
// calls (the API requires max_tokens and has no default).
const anthropicMaxTokens = 4096

// maxResponseBody is the upper bound on response body reads to prevent OOM from
// unexpectedly large upstream responses (10 MB).
const maxResponseBody = 10 << 20

// maxRetries is the number of additional attempts for retryable HTTP errors.
// Anthropic 429s are fairly common on long tool-loops (workflow ticks with
// dozens of search_code_org / modify_file rounds). Three retries was too
// stingy — a single burst of rate-limiting during a 20-round tick was
// enough to abort the whole tick.
const maxRetries = 6

// baseRetryDelay is the initial backoff delay between retries.
const baseRetryDelay = 2 * time.Second

// maxRetryDelay caps the wait between retries. Anthropic Retry-After headers
// occasionally request very long waits (e.g. 120s) under sustained load;
// clamping them prevents a single tick from stalling for minutes.
const maxRetryDelay = 60 * time.Second

// isRetryable returns true for HTTP status codes that warrant a retry.
func isRetryable(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

// retryDelay calculates how long to wait before the next retry, respecting
// a Retry-After header (seconds) if present. Capped at maxRetryDelay.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	var d time.Duration
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			d = time.Duration(secs) * time.Second
		}
	}
	if d == 0 {
		d = baseRetryDelay * (1 << attempt)
	}
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	return d
}

// Client provides LLM inference through either GitHub Models or Azure OpenAI.
// The backend is selected at construction time; the rest of the codebase uses
// the same Complete / CompleteWithTools interface regardless of backend.
type Client struct {
	token      string
	model      string
	httpClient *http.Client

	// compressURL is the base URL of a Headroom compression proxy (empty =
	// disabled); messages pass through its /v1/compress endpoint before each
	// backend call. Applies to all backends.
	compressURL string

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

// SetCompressionURL enables Headroom compression via the given proxy base URL
// (empty disables it). See compressMessages for behaviour.
func (c *Client) SetCompressionURL(u string) {
	c.compressURL = strings.TrimRight(u, "/")
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

// azureServicesEndpoint returns the Azure AI Services hostname used for
// partner-model protocol endpoints (e.g. /anthropic/v1/messages). When the
// user configured the classic "*.openai.azure.com" endpoint, we rewrite it to
// the sibling "*.services.ai.azure.com" host (same resource, different
// surface) so Claude and OpenAI deployments work from one config value.
func (c *Client) azureServicesEndpoint() string {
	return strings.Replace(c.azureEndpoint, ".openai.azure.com", ".services.ai.azure.com", 1)
}

// isAnthropicModel reports whether the given Azure deployment name is a
// Claude model served through Foundry's Anthropic Messages API endpoint
// (/anthropic/v1/messages).
func isAnthropicModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// Model returns the model/deployment name this client is using.
func (c *Client) Model() string {
	return c.model
}

// Complete sends a simple system+user prompt pair and returns the text response
// along with token usage information.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, *Usage, error) {
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	if c.isResponsesModel() {
		resp, err := c.doResponses(ctx, messages, nil)
		if err != nil {
			return "", nil, err
		}
		if len(resp.Choices) == 0 {
			return "", nil, fmt.Errorf("responses API returned no output")
		}
		return resp.Choices[0].Message.Content, resp.Usage, nil
	}

	if c.useAzure() && isAnthropicModel(c.model) {
		resp, err := c.doAnthropic(ctx, messages, nil)
		if err != nil {
			return "", nil, err
		}
		if len(resp.Choices) == 0 {
			return "", nil, fmt.Errorf("anthropic API returned no output")
		}
		return resp.Choices[0].Message.Content, resp.Usage, nil
	}

	resp, err := c.doChat(ctx, messages, nil)
	if err != nil {
		return "", nil, err
	}
	if len(resp.Choices) == 0 {
		return "", nil, fmt.Errorf("GitHub Models returned no choices")
	}
	return resp.Choices[0].Message.Content, resp.Usage, nil
}

// CompleteWithTools sends messages and tool definitions, returning the LLM's
// response which may include tool calls to execute.
func (c *Client) CompleteWithTools(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	messages = c.compressMessages(ctx, messages)
	if c.isResponsesModel() {
		return c.doResponses(ctx, messages, tools)
	}
	if c.useAzure() && isAnthropicModel(c.model) {
		return c.doAnthropic(ctx, messages, tools)
	}
	return c.doChat(ctx, messages, tools)
}

// compressRequest is the body for Headroom's POST /v1/compress endpoint.
type compressRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

// compressResponse is the subset of the /v1/compress reply we consume.
type compressResponse struct {
	Messages     []ChatMessage `json:"messages"`
	TokensBefore int           `json:"tokens_before"`
	TokensAfter  int           `json:"tokens_after"`
	TokensSaved  int           `json:"tokens_saved"`
}

// compressModel qualifies the model name with its provider before sending it to
// Headroom's /v1/compress endpoint. Headroom token-counts (and prices) via
// LiteLLM, which logs a noisy "Provider List" hint for every request whose
// model it can't map to a provider — e.g. a bare Azure Foundry Claude
// deployment name. An explicit provider prefix keeps that lookup unambiguous.
func (c *Client) compressModel() string {
	switch {
	case isAnthropicModel(c.model):
		return "anthropic/" + c.model
	case c.useAzure():
		return "azure/" + c.model
	default:
		return c.model
	}
}

// compressMessages returns the conversation compressed via Headroom's
// /v1/compress endpoint, or the messages unchanged when no proxy is configured
// or on any error (fail-open: compression never breaks an LLM call).
func (c *Client) compressMessages(ctx context.Context, messages []ChatMessage) []ChatMessage {
	if c.compressURL == "" || len(messages) == 0 {
		return messages
	}

	payload, err := json.Marshal(compressRequest{Model: c.compressModel(), Messages: messages})
	if err != nil {
		return messages
	}

	// Bound the compression round-trip independently of the 120s LLM timeout.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.compressURL+"/v1/compress", bytes.NewReader(payload))
	if err != nil {
		return messages
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("[llm] headroom compress unavailable, sending uncompressed: %v", err)
		return messages
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return messages
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("[llm] headroom compress returned %d, sending uncompressed", resp.StatusCode)
		return messages
	}

	var cr compressResponse
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Messages) == 0 {
		return messages
	}
	if cr.TokensSaved > 0 {
		pct := float64(cr.TokensSaved) / float64(cr.TokensBefore) * 100
		log.Printf("[llm] headroom compressed %d->%d tokens (saved %d, %.1f%%)", cr.TokensBefore, cr.TokensAfter, cr.TokensSaved, pct)
	}
	return cr.Messages
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

	headers := map[string]string{"Content-Type": "application/json"}
	if c.useAzure() {
		headers["api-key"] = c.azureAPIKey
	} else {
		headers["Authorization"] = "Bearer " + c.token
	}

	body, statusCode, err := c.doPostWithRetry(ctx, apiURL, payload, headers, "chat")
	if err != nil {
		return nil, err
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API returned %d: %s", statusCode, extractAPIErrorMessage(body))
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

// doPostWithRetry performs an HTTP POST with automatic retry on transient errors
// (429, 5xx). It returns the response body, the final HTTP status code, and any
// transport-level error. The label parameter is used in log messages.
func (c *Client) doPostWithRetry(ctx context.Context, url string, payload []byte, headers map[string]string, label string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create %s request: %w", label, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s API request failed: %w", label, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read %s response body: %w", label, err)
	}

	if isRetryable(resp.StatusCode) {
		for attempt := 0; attempt < maxRetries; attempt++ {
			wait := retryDelay(resp, attempt)
			log.Printf("[llm] %s retryable %d, backing off %s (attempt %d/%d)", label, resp.StatusCode, wait, attempt+1, maxRetries)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			retryReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, 0, fmt.Errorf("failed to create %s retry request: %w", label, err)
			}
			for k, v := range headers {
				retryReq.Header.Set(k, v)
			}
			resp, err = c.httpClient.Do(retryReq)
			if err != nil {
				return nil, 0, fmt.Errorf("%s API retry request failed: %w", label, err)
			}
			body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
			_ = resp.Body.Close()
			if err != nil {
				return nil, 0, fmt.Errorf("failed to read %s retry response body: %w", label, err)
			}
			if !isRetryable(resp.StatusCode) {
				break
			}
		}
	}

	return body, resp.StatusCode, nil
}

// ValidateModel verifies that the configured model/deployment is accessible
// by sending a minimal completion request.
func (c *Client) ValidateModel(ctx context.Context) error {
	_, _, err := c.Complete(ctx, "ping", "reply with ok")
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
// Only the gpt-5.x / codex / o-series families support Responses. Other
// deployments (gpt-4.x, claude-*, etc.) fall back to Chat Completions.
func (c *Client) isResponsesModel() bool {
	if !c.useAzure() {
		return false
	}
	if isAnthropicModel(c.model) {
		return false
	}
	m := strings.ToLower(c.model)
	switch {
	case strings.HasPrefix(m, "gpt-5"),
		strings.HasPrefix(m, "codex"),
		strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"):
		return true
	}
	return false
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

	headers := map[string]string{
		"Content-Type": "application/json",
		"api-key":      c.azureAPIKey,
	}

	body, statusCode, err := c.doPostWithRetry(ctx, apiURL, payload, headers, "responses")
	if err != nil {
		return nil, err
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("responses API returned %d: %s", statusCode, extractAPIErrorMessage(body))
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
