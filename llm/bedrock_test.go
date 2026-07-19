package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// roundTripFunc adapts a function to http.RoundTripper so a test can intercept
// the outbound request without any network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// staticCreds is a fixed CredentialsProvider so signing is deterministic and
// needs no AWS environment.
type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (awsv2.Credentials, error) {
	return awsv2.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "SECRETTEST", Source: "test"}, nil
}

// TestBedrockTransportWireFormat exercises the full Bedrock path — build, stamp,
// marshal, SigV4-sign, POST, parse — and locks the wire contract that differs
// from the Azure Foundry transport: model in the URL, anthropic_version in the
// body, and SigV4 auth instead of x-api-key.
func TestBedrockTransportWireFormat(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(r.Body)
		captured = r
		const reply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude",` +
			`"stop_reason":"end_turn","content":[{"type":"text","text":"hello from bedrock"}],` +
			`"usage":{"input_tokens":10,"output_tokens":5}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(reply)),
			Header:     make(http.Header),
		}, nil
	})

	c := &Client{
		model:      "anthropic.claude-opus-4-8",
		httpClient: &http.Client{Transport: rt},
		bedrock: &bedrockConfig{
			region: "us-east-1",
			creds:  staticCreds{},
			signer: v4.NewSigner(),
		},
	}

	resp, err := c.doBedrock(context.Background(),
		[]ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("doBedrock: %v", err)
	}

	// Model belongs in the URL path, targeting the invoke action.
	const wantURL = "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-opus-4-8/invoke"
	if got := captured.URL.String(); got != wantURL {
		t.Errorf("URL = %q, want %q", got, wantURL)
	}

	// Auth is SigV4, not Foundry's x-api-key.
	if !strings.HasPrefix(captured.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing SigV4 Authorization header: %q", captured.Header.Get("Authorization"))
	}
	if captured.Header.Get("X-Amz-Date") == "" {
		t.Error("missing X-Amz-Date header")
	}
	if captured.Header.Get("x-api-key") != "" {
		t.Error("x-api-key must not be set on Bedrock requests")
	}

	// Body carries anthropic_version and omits model (which is in the URL).
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["anthropic_version"] != bedrockAnthropicVersion {
		t.Errorf("anthropic_version = %v, want %q", body["anthropic_version"], bedrockAnthropicVersion)
	}
	if _, ok := body["model"]; ok {
		t.Error("model must not appear in the Bedrock request body")
	}

	// Reply is parsed into the shared ChatResponse shape.
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from bedrock" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Usage == nil || resp.Usage.CompletionTokens != 5 {
		t.Errorf("usage not parsed: %+v", resp.Usage)
	}
}

// TestBedrockAPIKeyAuth verifies that a configured Bedrock API key authenticates
// with a bearer token and skips SigV4 entirely.
func TestBedrockAPIKeyAuth(t *testing.T) {
	var captured *http.Request
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured = r
		const reply = `{"type":"message","role":"assistant","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(reply)),
			Header:     make(http.Header),
		}, nil
	})

	c := &Client{
		model:      "us.anthropic.claude-opus-4-8",
		httpClient: &http.Client{Transport: rt},
		bedrock:    &bedrockConfig{region: "us-east-1", apiKey: "ABSK-test-key"},
	}

	if _, err := c.doBedrock(context.Background(),
		[]ChatMessage{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("doBedrock: %v", err)
	}

	if got := captured.Header.Get("Authorization"); got != "Bearer ABSK-test-key" {
		t.Errorf("Authorization = %q, want bearer token", got)
	}
	// Bearer auth must not add SigV4 headers.
	if captured.Header.Get("X-Amz-Date") != "" {
		t.Error("X-Amz-Date must not be set with API-key auth")
	}
}
