package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/justmike1/arbetern/internal/text"
)

// Tier is how much model a request needs.
type Tier string

// The tiers a request can be routed to, cheapest first.
const (
	TierThanks  Tier = "thanks"
	TierLight   Tier = "light"
	TierGeneral Tier = "general"
	TierCode    Tier = "code"
	TierHeavy   Tier = "heavy"
)

var allTiers = []Tier{TierThanks, TierLight, TierGeneral, TierCode, TierHeavy}

// RouterAPI is the protocol a ModelRouter speaks.
type RouterAPI string

// The supported router protocols.
const (
	RouterAPIOpenAI    RouterAPI = "openai"    // chat completions: llama.cpp, Ollama, vLLM
	RouterAPISystemOne RouterAPI = "systemone" // TypeSafe System One decisions: Jev and compatible servers
)

const (
	defaultRouterTimeout = 5 * time.Second
	routerWarmTimeout    = 2 * time.Minute
	routerWarmRetry      = 10 * time.Second
	// A CPU-served model pays for every input token in latency, and the opening
	// of a request is enough to judge it.
	routerInputMax        = 1000
	routerMaxTokens       = 16
	routerResponseMax     = 64 << 10
	defaultSystemOneModel = "jev-latest"
	// TypeSafe's guidance: verify below 0.5, act automatically from 0.76.
	systemOneMinConfidence    = 0.5
	systemOneThanksConfidence = 0.76
)

var tierCriteria = []struct {
	tier Tier
	desc string
}{
	{TierThanks, "the message is nothing but gratitude or a goodbye. If anything is left after the thank-you (a request, a question, a correction, an approval, an instruction), it is not thanks: put the message in the tier of what is left."},
	{TierLight, "a greeting, or one quick lookup such as a status, an owner, a date, a definition or a short list."},
	{TierGeneral, `a task with a few steps: a summary or report from one source, an explanation, a draft, or a short reply that approves or confirms, such as "yes", "ok", "go ahead", "lgtm" or a thumbs-up.`},
	{TierCode, "reading, searching, reviewing or changing source code, files, branches or pull requests, or a failing build, test or CI run."},
	{TierHeavy, "an open-ended investigation that has to correlate several systems: root-cause analysis, incident reviews, architecture reviews, spend analysis across accounts or services, or changes across many repositories."},
}

const routerAssistant = "an internal engineering assistant that answers questions and does work through company tools (GitHub, Jira, Datadog, AWS, Slack, databases)"

const routerPromptTail = `
When unsure, answer general.

Examples:
"thank you, that solved it" -> thanks
"amazing, much appreciated!" -> thanks
"yep go for it" -> general
"sure" -> general
"thanks! can you also check staging" -> general
"ty, but that's the wrong repo, use payments-api" -> general
"which team owns the search service?" -> light
"write release notes from the PRs merged this week" -> general
"the nightly build is red, find out why and fix it" -> code
"why did checkout latency double since Tuesday? check datadog, the deploys and recent PRs" -> heavy

Reply with JSON: {"tier": "<tier>"}`

var (
	routerSystemPrompt      = buildRouterPrompt()
	routerSystemOneCriteria = buildSystemOneCriteria()
)

const systemOneInstructions = "Which tier of model does `message` need? It was sent to " + routerAssistant + ". When unsure, choose general."

func buildRouterPrompt() string {
	var sb strings.Builder
	sb.WriteString("You route messages sent to " + routerAssistant + ". Put the message in exactly one tier:\n\n")
	for _, c := range tierCriteria {
		fmt.Fprintf(&sb, "- %s: %s\n", c.tier, c.desc)
	}
	sb.WriteString(routerPromptTail)
	return sb.String()
}

func buildSystemOneCriteria() json.RawMessage {
	// encoding/json would sort the keys; keep the options cheapest first.
	var b bytes.Buffer
	b.WriteByte('{')
	for i, c := range tierCriteria {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(string(c.tier))
		v, _ := json.Marshal(c.desc)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

var routerResponseFormat = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"route","strict":true,"schema":{"type":"object","properties":{"tier":{"type":"string","enum":["thanks","light","general","code","heavy"]}},"required":["tier"],"additionalProperties":false}}}`)

type routerRequest struct {
	Model          string          `json:"model,omitempty"`
	Messages       []ChatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat json.RawMessage `json:"response_format"`
}

type systemOneRequest struct {
	State     map[string]string            `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

type systemOneAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// ModelRouter picks a request's tier through the in-pod sidecar's chat completions API or a System One decision API such as Jev.
type ModelRouter struct {
	api        RouterAPI
	url        string
	model      string
	apiKey     string
	timeout    time.Duration
	httpClient *http.Client
}

// NewModelRouter returns a router for the server at baseURL; model may be empty for a single-model server or Jev's default.
func NewModelRouter(api RouterAPI, baseURL, model, apiKey string, timeout time.Duration) *ModelRouter {
	if timeout <= 0 {
		timeout = defaultRouterTimeout
	}
	model = strings.TrimSpace(model)
	if api == RouterAPISystemOne && model == "" {
		model = defaultSystemOneModel
	}
	return &ModelRouter{
		api:        api,
		url:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		model:      model,
		apiKey:     strings.TrimSpace(apiKey),
		timeout:    timeout,
		httpClient: &http.Client{},
	}
}

// Model names the classifier in logs and the performance series.
func (r *ModelRouter) Model() string {
	if r.model == "" {
		return "model-router"
	}
	return r.model
}

// Timeout is the deadline of one classification.
func (r *ModelRouter) Timeout() time.Duration { return r.timeout }

// Route classifies a request without retrying, so a failing router costs a turn
// at most one timeout before its breaker takes it out of the path.
func (r *ModelRouter) Route(ctx context.Context, request string) (Tier, error) {
	br := breakerFor("router:"+r.url, "Model router")
	if !br.allow() {
		return "", ErrDependencyDown
	}
	started := time.Now()
	tier, err := r.classify(ctx, request, r.timeout)
	switch {
	case err == nil:
		br.ok()
	case ctx.Err() != nil:
		br.abandon()
		return "", err
	default:
		br.fail(err)
	}
	if obs := currentObserver(); obs != nil {
		obs.ObserveCall(CallStats{Model: r.Model(), Backend: "model-router", Latency: time.Since(started), Attempts: 1, Failed: err != nil})
	}
	return tier, err
}

// Warm loads the routing prompt into slots server slots, so no turn pays the seconds a cold slot spends on it.
func (r *ModelRouter) Warm(ctx context.Context, slots int) {
	if r.api == RouterAPISystemOne {
		return
	}
	started := time.Now()
	var wg sync.WaitGroup
	for range slots {
		wg.Go(func() {
			for ctx.Err() == nil {
				if _, err := r.classify(ctx, "hello", routerWarmTimeout); err == nil {
					return
				}
				select {
				case <-ctx.Done():
				case <-time.After(routerWarmRetry):
				}
			}
		})
	}
	wg.Wait()
	if ctx.Err() == nil {
		log.Printf("[llm] model router warmed %d slot(s) in %s", slots, time.Since(started).Round(time.Second))
	}
}

func (r *ModelRouter) classify(ctx context.Context, request string, timeout time.Duration) (Tier, error) {
	message := text.Truncate(strings.TrimSpace(request), routerInputMax)
	if r.api == RouterAPISystemOne {
		return r.classifySystemOne(ctx, message, timeout)
	}
	payload, err := json.Marshal(routerRequest{
		Model: r.model,
		Messages: []ChatMessage{
			{Role: "system", Content: routerSystemPrompt},
			{Role: "user", Content: "Message:\n<<<\n" + message + "\n>>>"},
		},
		MaxTokens:      routerMaxTokens,
		ResponseFormat: routerResponseFormat,
	})
	if err != nil {
		return "", err
	}
	body, err := r.post(ctx, timeout, "/v1/chat/completions", payload)
	if err != nil {
		return "", err
	}
	var out ChatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("undecodable response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("response has no choices")
	}
	return parseTier(out.Choices[0].Message.Content)
}

func (r *ModelRouter) classifySystemOne(ctx context.Context, message string, timeout time.Duration) (Tier, error) {
	payload, err := json.Marshal(systemOneRequest{
		State: map[string]string{"message": message},
		Model: r.model,
		Questions: map[string]systemOneQuestion{
			"tier": {Type: "choice", Instructions: systemOneInstructions, Criteria: routerSystemOneCriteria},
		},
	})
	if err != nil {
		return "", err
	}
	body, err := r.post(ctx, timeout, "/v1/systemone", payload)
	if err != nil {
		return "", err
	}
	var out struct {
		Answers map[string]systemOneAnswer `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("undecodable response: %w", err)
	}
	a, ok := out.Answers["tier"]
	if !ok || a.Type != "choice" {
		return "", errors.New("response has no tier choice")
	}
	tier, ok := tierNamed(a.Choice)
	if !ok {
		return "", fmt.Errorf("unknown tier %q", text.Truncate(a.Choice, 40))
	}
	if a.Confidence < systemOneMinConfidence || (tier == TierThanks && a.Confidence < systemOneThanksConfidence) {
		return TierGeneral, nil
	}
	return tier, nil
}

func (r *ModelRouter) post(ctx context.Context, timeout time.Duration, path string, payload []byte) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := func(err error) error {
		if ctx.Err() == nil && cctx.Err() != nil {
			return fmt.Errorf("no answer within %s: %w", timeout, context.DeadlineExceeded)
		}
		return err
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, r.url+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, deadline(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, routerResponseMax))
	if err != nil {
		return nil, deadline(err)
	}
	if resp.StatusCode != http.StatusOK {
		if msg := extractAPIErrorMessage(body); msg != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func parseTier(content string) (Tier, error) {
	var out struct {
		Tier string `json:"tier"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(content)), &out) == nil {
		if t, ok := tierNamed(out.Tier); ok {
			return t, nil
		}
	}
	// A server that ignores response_format answers in prose.
	for _, word := range strings.FieldsFunc(strings.ToLower(content), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if t, ok := tierNamed(word); ok {
			return t, nil
		}
	}
	return "", fmt.Errorf("no tier in reply %q", text.Truncate(content, 80))
}

func tierNamed(s string) (Tier, bool) {
	t := Tier(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range allTiers {
		if t == known {
			return t, true
		}
	}
	return "", false
}
