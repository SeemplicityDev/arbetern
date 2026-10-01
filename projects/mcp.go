package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

const (
	mcpProtocolVersion = "2025-06-18"
	mcpInstructions    = "Call get_task first; it returns your work order. Finish with report_outcome."

	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603

	maxBatch = 32
)

var mcpProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var mcpTools = []toolDef{
	{
		Name:        "get_task",
		Description: "Return your work order: the goal, the error group with production log samples, the rules, project notes and recent outcomes. Call it first.",
		InputSchema: objectSchema(nil),
	},
	{
		Name:        "report_outcome",
		Description: "Report the result of the work order exactly once. For status fixed, commit and push your " + BranchPrefix + " branch first; arbetern checks it and opens the pull request. Otherwise report why no change was made.",
		InputSchema: objectSchema(map[string]any{
			"status":  map[string]any{"type": "string", "enum": []string{OutcomeFixed, OutcomeNotReproducible, OutcomeCannotFix, OutcomeAlreadyFixed}},
			"summary": map[string]any{"type": "string", "maxLength": 4000, "description": "What you found and, for fixed, what you changed and why."},
			"title":   map[string]any{"type": "string", "maxLength": 120, "description": "Pull request title; required for fixed."},
			"branch":  map[string]any{"type": "string", "maxLength": 200, "description": "The " + BranchPrefix + " branch you pushed; required for fixed."},
			"testing": map[string]any{"type": "string", "maxLength": 2000, "description": "How you verified the change."},
		}, "status", "summary"),
	},
	{
		Name:        "record_learning",
		Description: fmt.Sprintf("Save one durable fact about this repository that future sessions on this project need. At most %d per work order.", maxLearnings),
		InputSchema: objectSchema(map[string]any{
			"note": map[string]any{"type": "string", "minLength": 1, "maxLength": maxNoteChars},
		}, "note"),
	},
}

func knownTool(name string) bool {
	for _, t := range mcpTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolText `json:"content"`
	IsError bool       `json:"isError"`
}

// toolCaller runs one tool: err is an internal failure, isError a problem the session can act on.
type toolCaller func(ctx context.Context, name string, args json.RawMessage) (text string, isError bool, err error)

var nullID = json.RawMessage("null")

func rpcFailure(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func encodeRPC(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
	}
	return body
}

// serveRPC answers one MCP POST body; 202 with no body means it held only notifications or responses.
func serveRPC(ctx context.Context, body []byte, call toolCaller) (int, []byte) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return http.StatusOK, encodeRPC(rpcFailure(nullID, rpcParseError, "Parse error"))
		}
		if len(batch) == 0 || len(batch) > maxBatch {
			return http.StatusOK, encodeRPC(rpcFailure(nullID, rpcInvalidRequest, "Invalid Request"))
		}
		var out []*rpcResponse
		for _, msg := range batch {
			if resp := handleRPC(ctx, msg, call); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			return http.StatusAccepted, nil
		}
		return http.StatusOK, encodeRPC(out)
	}
	if !json.Valid(body) {
		return http.StatusOK, encodeRPC(rpcFailure(nullID, rpcParseError, "Parse error"))
	}
	resp := handleRPC(ctx, body, call)
	if resp == nil {
		return http.StatusAccepted, nil
	}
	return http.StatusOK, encodeRPC(resp)
}

func validID(id json.RawMessage) bool {
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

func handleRPC(ctx context.Context, raw json.RawMessage, call toolCaller) *rpcResponse {
	var m struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  *string         `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return rpcFailure(nullID, rpcInvalidRequest, "Invalid Request")
	}
	id := nullID
	if m.ID != nil && validID(m.ID) {
		id = m.ID
	}
	if m.Method == nil {
		if m.Result != nil || m.Error != nil {
			return nil
		}
		return rpcFailure(id, rpcInvalidRequest, "Invalid Request")
	}
	if m.ID == nil {
		return nil
	}
	if m.JSONRPC != "2.0" || !validID(m.ID) {
		return rpcFailure(id, rpcInvalidRequest, "Invalid Request")
	}
	result, rerr := dispatchRPC(ctx, *m.Method, m.Params, call)
	if rerr != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: id, Error: rerr}
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func dispatchRPC(ctx context.Context, method string, params json.RawMessage, call toolCaller) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(params) > 0 && json.Unmarshal(params, &p) != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "Invalid params"}
		}
		version := mcpProtocolVersion
		if mcpProtocols[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "arbetern", "version": "1"},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(params, &p) != nil || p.Name == "" {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "Invalid params: tools/call needs a tool name"}
		}
		if !knownTool(p.Name) {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "Invalid params: unknown tool " + oneLine(p.Name, 64)}
		}
		args := bytes.TrimSpace(p.Arguments)
		if len(args) == 0 || bytes.Equal(args, nullID) {
			args = []byte("{}")
		}
		if args[0] != '{' {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "Invalid params: arguments must be an object"}
		}
		text, isError, err := call(ctx, p.Name, args)
		if err != nil {
			log.Printf("[projects] mcp tool %s failed: %v", p.Name, err)
			return nil, &rpcError{Code: rpcInternalError, Message: "Internal error"}
		}
		return toolResult{Content: []toolText{{Type: "text", Text: text}}, IsError: isError}, nil
	}
	return nil, &rpcError{Code: rpcMethodNotFound, Message: "Method not found"}
}
