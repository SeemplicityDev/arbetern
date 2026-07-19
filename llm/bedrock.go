package llm

// ---------------------------------------------------------------------------
// AWS Bedrock transport for the Anthropic Messages API.
//
// Bedrock's Claude runtime speaks the same Messages protocol as Azure Foundry,
// so the message/tool translation, prompt-cache markers, and response parsing
// are shared (see anthropic.go). Only the transport differs:
//
//   - The model ID lives in the URL path, not the body
//     (POST https://bedrock-runtime.<region>.amazonaws.com/model/<id>/invoke).
//   - The body carries anthropic_version: "bedrock-2023-05-31" instead of an
//     anthropic-version header.
//   - Auth is one of two schemes:
//       * A Bedrock API key (bearer token) sent as "Authorization: Bearer <key>"
//         — set via AWS_BEARER_TOKEN_BEDROCK. No AWS credential resolution.
//       * Otherwise SigV4 for the "bedrock" service using the standard AWS
//         credential chain (env vars, shared profile, EKS IRSA / IMDS) — the
//         same chain the cost-explorer integration uses.
//
// Model IDs carry an "anthropic." provider prefix, e.g. "anthropic.claude-opus-4-8";
// most accounts use a cross-region inference profile such as
// "us.anthropic.claude-opus-4-8".
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
)

// bedrockAnthropicVersion is the anthropic_version value Bedrock requires in the
// request body (in place of Foundry's anthropic-version header).
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// bedrockConfig holds the AWS Bedrock transport state hung off Client.bedrock.
// A non-nil Client.bedrock selects the Bedrock backend. When apiKey is set,
// requests use bearer-token auth and creds/signer are nil; otherwise they are
// SigV4-signed. The signer is safe for concurrent use and the credentials
// provider caches/refreshes internally, so a WithModel clone can share this by
// value without extra AWS round-trips.
type bedrockConfig struct {
	region string
	apiKey string // Bedrock API key (bearer token); empty means SigV4.
	creds  awsv2.CredentialsProvider
	signer *v4.Signer
}

// NewBedrockClient creates an LLM client backed by Amazon Bedrock's Anthropic
// Messages runtime. region must be a region where the model or inference profile
// is available; model is the Bedrock model or inference-profile ID (e.g.
// "anthropic.claude-opus-4-8" or "us.anthropic.claude-opus-4-8").
//
// Authentication:
//   - A non-empty apiKey (a Bedrock API key, i.e. AWS_BEARER_TOKEN_BEDROCK) is
//     used as a bearer token — no AWS credential resolution happens.
//   - Otherwise credentials resolve through the standard AWS SDK chain (env vars
//     / shared profile / EKS IRSA / IMDS) and requests are SigV4-signed. The
//     credentials are probed at construction so a misconfiguration surfaces at
//     startup rather than on the first completion.
func NewBedrockClient(ctx context.Context, region, model, apiKey string) (*Client, error) {
	bc := &bedrockConfig{region: region, apiKey: strings.TrimSpace(apiKey)}
	if bc.apiKey == "" {
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("load AWS config: %w", err)
		}
		if _, err := awsCfg.Credentials.Retrieve(ctx); err != nil {
			return nil, fmt.Errorf("resolve AWS credentials: %w", err)
		}
		bc.creds = awsCfg.Credentials
		bc.signer = v4.NewSigner()
	}
	return &Client{
		model:      model,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		bedrock:    bc,
	}, nil
}

// useBedrock reports whether the client is configured for AWS Bedrock.
func (c *Client) useBedrock() bool { return c.bedrock != nil }

// bedrockTransport speaks the Anthropic Messages API as hosted by Amazon
// Bedrock's InvokeModel endpoint: model in the URL, anthropic_version in the
// body, and SigV4 authentication for the "bedrock" service.
type bedrockTransport struct{ c *Client }

func (t bedrockTransport) name() string { return "bedrock" }

func (t bedrockTransport) endpoint(model string) string {
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/invoke",
		t.c.bedrock.region, url.PathEscape(model))
}

func (t bedrockTransport) stampEnvelope(req *anthropicRequest, _ string) {
	req.AnthropicVersion = bedrockAnthropicVersion
}

// authorize sets the JSON headers and authenticates the request — a bearer
// token when a Bedrock API key is configured, otherwise SigV4. It runs on every
// attempt (see doPostWithRetry) so a retry after a backoff carries a fresh
// signature rather than an expired one.
func (t bedrockTransport) authorize(ctx context.Context, r *http.Request, body []byte) error {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	if t.c.bedrock.apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+t.c.bedrock.apiKey)
		return nil
	}
	creds, err := t.c.bedrock.creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("resolve AWS credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	return t.c.bedrock.signer.SignHTTP(ctx, creds, r, hex.EncodeToString(sum[:]), "bedrock", t.c.bedrock.region, time.Now())
}

// doBedrock calls Bedrock's Anthropic Messages runtime for Claude models.
func (c *Client) doBedrock(ctx context.Context, messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	return c.callMessages(ctx, bedrockTransport{c}, messages, tools)
}
