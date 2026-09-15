package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	githubEmbeddingsURL       = "https://models.github.ai/inference/embeddings"
	azureEmbeddingsAPIVersion = "2024-10-21"
	embeddingRequestTimeout   = 60 * time.Second
	maxEmbeddingInputChars    = 24000
)

// Embedder produces text embeddings through Bedrock (Amazon Titan), Azure
// OpenAI or GitHub Models, selected at construction.
type Embedder struct {
	model      string
	dims       int
	httpClient *http.Client

	bedrock       *bedrockConfig
	azureEndpoint string
	azureAPIKey   string
	token         string
}

// DefaultEmbeddingDimensions is the native output size of a known embedding
// model, or 0 when the model is unknown and the size must be configured.
func DefaultEmbeddingDimensions(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "titan-embed-text-v2"):
		return 1024
	case strings.Contains(m, "titan-embed-text-v1"), strings.Contains(m, "ada-002"), strings.Contains(m, "text-embedding-3-small"):
		return 1536
	case strings.Contains(m, "text-embedding-3-large"):
		return 3072
	}
	return 0
}

// NewBedrockEmbedder embeds with an Amazon Titan text-embedding model.
func NewBedrockEmbedder(ctx context.Context, region, model, apiKey string, dims int) (*Embedder, error) {
	if !strings.HasPrefix(strings.ToLower(model), "amazon.titan-embed") {
		return nil, fmt.Errorf("unsupported Bedrock embedding model %q (amazon.titan-embed-* is supported)", model)
	}
	bc, err := newBedrockConfig(ctx, region, apiKey)
	if err != nil {
		return nil, err
	}
	return &Embedder{model: model, dims: dims, httpClient: &http.Client{Timeout: embeddingRequestTimeout}, bedrock: bc}, nil
}

// NewAzureEmbedder embeds with an Azure OpenAI embeddings deployment.
func NewAzureEmbedder(endpoint, apiKey, deployment string, dims int) *Embedder {
	return &Embedder{
		model:         deployment,
		dims:          dims,
		httpClient:    &http.Client{Timeout: embeddingRequestTimeout},
		azureEndpoint: strings.TrimRight(endpoint, "/"),
		azureAPIKey:   apiKey,
	}
}

// NewGitHubEmbedder embeds with a GitHub Models embedding model.
func NewGitHubEmbedder(token, model string, dims int) *Embedder {
	return &Embedder{model: model, dims: dims, httpClient: &http.Client{Timeout: embeddingRequestTimeout}, token: token}
}

// Model is the embedding model or deployment name.
func (e *Embedder) Model() string { return e.model }

// Dimensions is the length of every vector Embed returns.
func (e *Embedder) Dimensions() int { return e.dims }

// Embed returns one vector per text, in order.
// Embed vectorises texts. Like compression it is guarded by a breaker: the
// semantic parts of a prompt are optional (retrieval falls back to recency) and
// indexing is retried from the queue, so a backend that has started failing is
// better skipped than waited on at the front of every turn.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	clipped := make([]string, len(texts))
	for i, t := range texts {
		if len(t) > maxEmbeddingInputChars {
			t = t[:maxEmbeddingInputChars]
		}
		clipped[i] = t
	}
	br := breakerFor("embeddings:"+e.model, "Embeddings ("+e.model+")")
	if !br.allow() {
		return nil, fmt.Errorf("%w: embeddings model %s", ErrDependencyDown, e.model)
	}
	var (
		out [][]float32
		err error
	)
	if e.bedrock != nil {
		out, err = e.embedTitan(ctx, clipped)
	} else {
		out, err = e.embedOpenAI(ctx, clipped)
	}
	// A caller that gave up says nothing about the backend.
	if err != nil && ctx.Err() == nil {
		br.fail(err)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	br.ok()
	return out, nil
}

func (e *Embedder) embedTitan(ctx context.Context, texts []string) ([][]float32, error) {
	v2 := strings.Contains(strings.ToLower(e.model), "v2")
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		body := map[string]any{"inputText": t}
		if v2 {
			body["dimensions"] = e.dims
			body["normalize"] = true
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		resp, err := e.post(ctx, e.bedrock.invokeURL(e.model), payload, func(r *http.Request) error {
			return e.bedrock.authorize(ctx, r, payload)
		})
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Embedding []float32 `json:"embedding"`
		}
		if err := json.Unmarshal(resp, &parsed); err != nil {
			return nil, fmt.Errorf("decode embedding: %w", err)
		}
		if len(parsed.Embedding) != e.dims {
			return nil, fmt.Errorf("model %s returned %d dimensions, expected %d", e.model, len(parsed.Embedding), e.dims)
		}
		out = append(out, parsed.Embedding)
	}
	return out, nil
}

func (e *Embedder) embedOpenAI(ctx context.Context, texts []string) ([][]float32, error) {
	body := map[string]any{"input": texts}
	if strings.Contains(strings.ToLower(e.model), "text-embedding-3") {
		body["dimensions"] = e.dims
	}
	var apiURL string
	authorize := func(r *http.Request) error {
		r.Header.Set("Content-Type", "application/json")
		if e.azureEndpoint != "" {
			r.Header.Set("api-key", e.azureAPIKey)
		} else {
			r.Header.Set("Authorization", "Bearer "+e.token)
		}
		return nil
	}
	if e.azureEndpoint != "" {
		apiURL = fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s", e.azureEndpoint, e.model, azureEmbeddingsAPIVersion)
	} else {
		apiURL = githubEmbeddingsURL
		body["model"] = e.model
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := e.post(ctx, apiURL, payload, authorize)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("model %s returned %d embeddings for %d inputs", e.model, len(parsed.Data), len(texts))
	}
	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) != e.dims {
			return nil, fmt.Errorf("model %s returned %d dimensions, expected %d", e.model, len(d.Embedding), e.dims)
		}
		out[i] = d.Embedding
	}
	return out, nil
}

func (e *Embedder) post(ctx context.Context, apiURL string, payload []byte, authorize func(*http.Request) error) ([]byte, error) {
	var lastStatus int
	var lastBody []byte
	for attempt := 0; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		if err := authorize(req); err != nil {
			return nil, err
		}
		resp, err := e.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("embeddings request failed: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		retryAfter := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read embeddings response: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}
		lastStatus, lastBody = resp.StatusCode, body
		if !isRetryable(resp.StatusCode) {
			break
		}
		wait := retryDelay(retryAfter, attempt)
		log.Printf("[llm] embeddings retryable %d, backing off %s", resp.StatusCode, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("embeddings API returned %d: %s", lastStatus, extractAPIErrorMessage(lastBody))
}
