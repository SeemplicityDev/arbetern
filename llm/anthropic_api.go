package llm

import (
	"context"
	"net/http"
	"strings"
)

const (
	anthropicMessagesURL = "https://api.anthropic.com/v1/messages"
	refusalFallbackBeta  = "server-side-fallback-2026-07-01"
)

var refusalFallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

// NewAnthropicClient creates an LLM client backed by the Anthropic API.
func NewAnthropicClient(apiKey, model string) *Client {
	return &Client{
		model:        resolveModel(providerAnthropic, model),
		httpClient:   &http.Client{Timeout: llmRequestTimeout},
		anthropicKey: strings.TrimSpace(apiKey),
	}
}

func (c *Client) useAnthropic() bool { return c.anthropicKey != "" }

type anthropicTransport struct{ c *Client }

func (t anthropicTransport) name() string { return "anthropic" }

func (t anthropicTransport) endpoint(string) string { return anthropicMessagesURL }

func (t anthropicTransport) stampEnvelope(req *anthropicRequest, model string) {
	req.Model = model
	if refusalFallbackModels[model] {
		req.Fallbacks = "default"
	}
}

func (t anthropicTransport) authorize(_ context.Context, r *http.Request, _ []byte) error {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", t.c.anthropicKey)
	r.Header.Set("anthropic-version", anthropicAPIVersion)
	if refusalFallbackModels[t.c.model] {
		r.Header.Set("anthropic-beta", refusalFallbackBeta)
	}
	return nil
}

func (c *Client) doAnthropic(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	return c.callMessages(ctx, anthropicTransport{c}, messages, tools)
}
