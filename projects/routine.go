package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/sessiontoken"
	"github.com/justmike1/arbetern/internal/text"
)

const (
	routineAPIBase   = "https://api.anthropic.com"
	anthropicVersion = "2023-06-01"
	routineBeta      = "experimental-cc-routine-2026-04-01"
	fireTimeout      = 30 * time.Second
	maxFireResponse  = 64 << 10
)

// Dispatcher starts a Claude Code session by firing a routine.
type Dispatcher interface {
	Fire(ctx context.Context, routineID, token, text string) (*Fired, error)
}

// Fired identifies the session a routine fire created.
type Fired struct {
	SessionID, SessionURL string
}

// FireError is a routine fire the API rejected.
type FireError struct {
	Status        int
	Type, Message string
	RetryAfter    time.Duration
}

func (e *FireError) Error() string {
	msg := fmt.Sprintf("routine fire returned HTTP %d", e.Status)
	if e.Type != "" {
		msg += " " + e.Type
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

type routineClient struct {
	http    *http.Client
	baseURL string
}

func newRoutineClient() *routineClient {
	return &routineClient{http: httpx.SafeClient(fireTimeout, 0), baseURL: routineAPIBase}
}

func (c *routineClient) Fire(ctx context.Context, routineID, token, text string) (*Fired, error) {
	if !routineIDRe.MatchString(routineID) {
		return nil, errors.New("invalid routine id")
	}
	if token == "" {
		return nil, errors.New("routine trigger token is empty")
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fireTimeout)
	defer cancel()
	endpoint := c.baseURL + "/v1/claude_code/routines/" + url.PathEscape(routineID) + "/fire"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", routineBeta)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firing routine %s: %w", routineID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFireResponse))
	if err != nil {
		return nil, fmt.Errorf("reading the routine fire response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fireError(resp, raw)
	}
	var out struct {
		SessionID  string `json:"claude_code_session_id"`
		SessionURL string `json:"claude_code_session_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding the routine fire response: %w", err)
	}
	if out.SessionID == "" || out.SessionURL == "" {
		return nil, errors.New("the routine fire response has no session id or url")
	}
	if sessiontoken.NormalizeSessionID(out.SessionID) == "" {
		return nil, errors.New("the routine fire response has a malformed session id")
	}
	return &Fired{SessionID: out.SessionID, SessionURL: out.SessionURL}, nil
}

func fireError(resp *http.Response, raw []byte) *FireError {
	fe := &FireError{Status: resp.StatusCode, RetryAfter: httpx.RetryAfter(resp.Header)}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		fe.Type = strings.TrimSpace(body.Error.Type)
		fe.Message = strings.TrimSpace(body.Error.Message)
	}
	if fe.Message == "" {
		fe.Message = http.StatusText(resp.StatusCode)
	}
	fe.Type = text.Truncate(fe.Type, 64)
	fe.Message = text.Truncate(fe.Message, maxErrorChars)
	return fe
}

func fireText(agent, project, task string) string {
	return text.Truncate("arbetern work order "+agent+"/"+project+"/"+task+". Fetch it with the get_task tool of the arbetern MCP server.", maxFireText)
}
