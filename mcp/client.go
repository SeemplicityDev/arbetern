package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// protocolVersion is the MCP revision this client speaks (Streamable HTTP).
const protocolVersion = "2025-03-26"

const (
	clientName      = "arbetern"
	maxResponseBody = 4 << 20
	maxToolOutput   = 32 << 10
)

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv resolves ${NAME} references so header values can point at
// environment variables instead of storing secrets on disk.
func expandEnv(v string) string {
	return envRefRe.ReplaceAllStringFunc(v, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// Client speaks JSON-RPC 2.0 to one MCP server over Streamable HTTP. One
// Client is one session: Initialize first, then ListTools / CallTool.
type Client struct {
	url       string
	headers   map[string]string
	http      *http.Client
	sessionID string
	nextID    atomic.Int64
	ready     bool
}

// ServerInfo is what the server reports about itself during initialize.
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func newClient(url string, headers map[string]string, timeout time.Duration) *Client {
	return &Client{url: url, headers: headers, http: &http.Client{Timeout: timeout}}
}

// Initialize performs the MCP handshake and records the session ID.
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": clientName, "version": "1.0"},
	}
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return ServerInfo{}, err
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return ServerInfo{}, fmt.Errorf("decode initialize result: %w", err)
	}
	c.ready = true
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{Name: res.ServerInfo.Name, Version: res.ServerInfo.Version, ProtocolVersion: res.ProtocolVersion}, nil
}

// ListTools pages through tools/list and returns every tool the server offers.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var out []ToolInfo
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("decode tools/list result: %w", err)
		}
		for _, t := range res.Tools {
			if strings.TrimSpace(t.Name) == "" {
				continue
			}
			out = append(out, ToolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// CallTool invokes one tool and flattens its content blocks to text. isError
// mirrors the server's isError flag; err covers transport and protocol errors.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (text string, isError bool, err error) {
	var arguments any = map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 {
		var parsed any
		if jerr := json.Unmarshal(args, &parsed); jerr != nil {
			return "", false, fmt.Errorf("tool arguments are not valid JSON: %w", jerr)
		}
		if parsed != nil {
			arguments = parsed
		}
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return "", false, err
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, fmt.Errorf("decode tools/call result: %w", err)
	}
	var sb strings.Builder
	for _, block := range res.Content {
		switch block.Type {
		case "text":
			sb.WriteString(block.Text)
			sb.WriteString("\n")
		case "resource":
			if block.Resource.Text != "" {
				fmt.Fprintf(&sb, "[resource %s]\n%s\n", block.Resource.URI, block.Resource.Text)
			} else {
				fmt.Fprintf(&sb, "[resource %s]\n", block.Resource.URI)
			}
		case "image", "audio":
			fmt.Fprintf(&sb, "[%s content omitted]\n", block.Type)
		}
	}
	if sb.Len() == 0 && len(res.StructuredContent) > 0 {
		sb.Write(res.StructuredContent)
	}
	out := strings.TrimSpace(sb.String())
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "\n… [output truncated]"
	}
	return out, res.IsError, nil
}

func (c *Client) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method})
	resp, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: server returned HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("%s: server returned HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var rpc *rpcResponse
	if mediaType == "text/event-stream" {
		rpc, err = readEventStream(io.LimitReader(resp.Body, maxResponseBody), id)
	} else {
		rpc, err = readJSON(io.LimitReader(resp.Body, maxResponseBody), id)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("%s: %s (code %d)", method, rpc.Error.Message, rpc.Error.Code)
	}
	return rpc.Result, nil
}

func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.ready {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	for k, v := range c.headers {
		if k = strings.TrimSpace(k); k != "" {
			req.Header.Set(k, expandEnv(v))
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func readJSON(r io.Reader, wantID int64) (*rpcResponse, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("empty response")
	}
	if data[0] == '[' {
		var batch []rpcResponse
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		for i := range batch {
			if idMatches(batch[i].ID, wantID) {
				return &batch[i], nil
			}
		}
		return nil, errors.New("response batch had no matching id")
	}
	var one rpcResponse
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &one, nil
}

func readEventStream(r io.Reader, wantID int64) (*rpcResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxResponseBody)
	var data strings.Builder
	flush := func() (*rpcResponse, bool) {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil, false
		}
		var rpc rpcResponse
		if err := json.Unmarshal([]byte(payload), &rpc); err != nil {
			return nil, false
		}
		if idMatches(rpc.ID, wantID) {
			return &rpc, true
		}
		return nil, false
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if rpc, ok := flush(); ok {
				return rpc, nil
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteString("\n")
		}
	}
	if rpc, ok := flush(); ok {
		return rpc, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("event stream ended without a response")
}

func idMatches(raw json.RawMessage, want int64) bool {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == want
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == fmt.Sprint(want)
	}
	return false
}
