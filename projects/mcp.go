package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/justmike1/arbetern/llm"
)

const (
	mcpProtocolVersion = "2025-06-18"
	mcpInstructions    = "Call get_task first; it returns your work order. The other tools read arbetern's integrations, such as Datadog, GitHub and Jira, with arbetern's own access, and transition_jira_issue moves a Jira issue to another status. Finish with report_outcome."

	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603

	maxBatch = 32
)

var mcpProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func objectSchema(props map[string]any, required ...string) json.RawMessage {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	raw, _ := json.Marshal(s)
	return raw
}

var workOrderTools = []toolDef{
	{
		Name:        "get_task",
		Description: "Return your work order: the goal, the problem with its production evidence, the rules, project notes and recent outcomes; in a follow-up round, the failing checks and the review comments to answer. Call it first.",
		InputSchema: objectSchema(nil),
	},
	{
		Name:        "report_outcome",
		Description: "Report the result of the work order exactly once. For status fixed, commit and push first; arbetern checks the branch and opens the pull request, or in a follow-up round adds your commits to it and posts your replies. Otherwise report why no change was made.",
		InputSchema: objectSchema(map[string]any{
			"status":  map[string]any{"type": "string", "enum": []string{OutcomeFixed, OutcomeNotReproducible, OutcomeCannotFix, OutcomeAlreadyFixed, OutcomeAnswered}, "description": "answered is for follow-up rounds only; not_reproducible and already_fixed are for first sessions only."},
			"summary": map[string]any{"type": "string", "maxLength": maxSessionSummary, "description": "What you found and, for fixed, what you changed and why; 4000 characters unless the work order makes it the whole pull request description. In a follow-up round it is posted on the pull request."},
			"title":   map[string]any{"type": "string", "maxLength": 120, "description": "Pull request title; required for fixed in a first session."},
			"branch":  map[string]any{"type": "string", "maxLength": 200, "description": "The branch you pushed, as the work order names it; required for fixed."},
			"repository": map[string]any{"type": "string", "maxLength": 140,
				"description": "The repository your branch is in, by name; required for fixed when the work order has you choose the repository."},
			"testing": map[string]any{"type": "string", "maxLength": 2000, "description": "How you verified the change."},
			"question": map[string]any{"type": "string", "maxLength": maxQuestionChars,
				"description": "A decision only a person can make, with the options named so a one-line reply picks one. A first session's pull request opens as a draft and stays one until a reviewer answers in it."},
			"headline": map[string]any{"type": "string", "maxLength": maxHeadlineChars, "description": "One line for the team's channel naming what failed, where, and how much."},
			"ticket": json.RawMessage(objectSchema(map[string]any{
				"summary":     map[string]any{"type": "string", "minLength": 1, "maxLength": maxTicketSummary},
				"description": map[string]any{"type": "string", "maxLength": maxTicketText},
				"fields":      map[string]any{"type": "object", "description": "Only the fields the work order names."},
			}, "summary")),
			"replies": map[string]any{
				"type": "array", "maxItems": maxReplies,
				"description": "Follow-up rounds: one entry per review comment in the work order, saying what you changed and why, or why you changed nothing. Each is posted as a reply to its comment.",
				"items": json.RawMessage(objectSchema(map[string]any{
					"id":   map[string]any{"type": "string", "maxLength": 64},
					"body": map[string]any{"type": "string", "minLength": 1, "maxLength": maxReplyChars},
				}, "id", "body")),
			},
			"rerun_checks": map[string]any{"type": "boolean", "description": "Follow-up rounds: the failing checks are unrelated to this change, so arbetern re-runs their failed jobs once."},
			"description": map[string]any{"type": "string", "maxLength": maxSessionSummary,
				"description": "Follow-up rounds, when the work order allows it: the whole corrected pull request description, replacing the current one."},
		}, "status", "summary"),
	},
	{
		Name:        "record_learning",
		Description: fmt.Sprintf("Save one durable fact about this project's code that future sessions need; name the repository it is about when the project spans several. At most %d per work order.", maxLearnings),
		InputSchema: objectSchema(map[string]any{
			"note": map[string]any{"type": "string", "minLength": 1, "maxLength": maxNoteChars},
		}, "note"),
	},
}

func integrationTools(tools []llm.Tool) []toolDef {
	out := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(bytes.TrimSpace(schema)) == 0 || !json.Valid(schema) {
			schema = objectSchema(nil)
		}
		out = append(out, toolDef{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	return out
}

func findTool(tools []toolDef, name string) (toolDef, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return toolDef{}, false
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
func serveRPC(ctx context.Context, body []byte, tools []toolDef, call toolCaller) (int, []byte) {
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
			if resp := handleRPC(ctx, msg, tools, call); resp != nil {
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
	resp := handleRPC(ctx, body, tools, call)
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

func handleRPC(ctx context.Context, raw json.RawMessage, tools []toolDef, call toolCaller) *rpcResponse {
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
	result, rerr := dispatchRPC(ctx, *m.Method, m.Params, tools, call)
	if rerr != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: id, Error: rerr}
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func dispatchRPC(ctx context.Context, method string, params json.RawMessage, tools []toolDef, call toolCaller) (any, *rpcError) {
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
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(params, &p) != nil || p.Name == "" {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "Invalid params: tools/call needs a tool name"}
		}
		if _, ok := findTool(tools, p.Name); !ok {
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
