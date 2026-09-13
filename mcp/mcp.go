// Package mcp registers Model Context Protocol servers as connectors, stores
// them under Prefix in the state bucket, discovers their tools, and exposes
// those tools to the agents allowed to use each connector.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/store"
)

// Prefix is the object prefix connectors are stored under.
const Prefix = "mcp/"

const (
	TransportHTTP = "http"

	// MaskedValue replaces literal header values in API responses; sending it
	// back on update keeps the stored value.
	MaskedValue = "••••••••"

	discoveryTimeout = 30 * time.Second
	callTimeout      = 90 * time.Second
	maxLLMToolName   = 64
)

var (
	ErrNotFound = errors.New("connector not found")
	ErrDisabled = errors.New("connector is disabled")
)

// ToolInfo is a tool advertised by an MCP server.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Connector is one registered MCP server. Agents is the allowlist of agent IDs
// that may call its tools; empty means every agent.
type Connector struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Transport       string            `json:"transport"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers,omitempty"`
	Agents          []string          `json:"agents"`
	Enabled         bool              `json:"enabled"`
	Tools           []ToolInfo        `json:"tools,omitempty"`
	ServerName      string            `json:"server_name,omitempty"`
	ServerVersion   string            `json:"server_version,omitempty"`
	ProtocolVersion string            `json:"protocol_version,omitempty"`
	LastCheck       string            `json:"last_check,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
	CreatedBy       string            `json:"created_by,omitempty"`
	CreatedAt       string            `json:"created_at"`
	UpdatedAt       string            `json:"updated_at,omitempty"`
}

// Patch carries the fields a PATCH request may change; nil means unchanged.
type Patch struct {
	Name        *string            `json:"name"`
	Description *string            `json:"description"`
	URL         *string            `json:"url"`
	Headers     *map[string]string `json:"headers"`
	Agents      *[]string          `json:"agents"`
	Enabled     *bool              `json:"enabled"`
}

// AgentTool is one MCP tool as exposed to an agent's LLM tool loop.
type AgentTool struct {
	ConnectorID   string
	ConnectorName string
	Tool          ToolInfo
	LLMName       string
}

// Description prefixes the tool description with its connector so the model
// can tell MCP tools apart from built-in ones.
func (t AgentTool) Description() string {
	d := strings.TrimSpace(t.Tool.Description)
	if d == "" {
		d = "MCP tool " + t.Tool.Name
	}
	return fmt.Sprintf("[MCP: %s] %s", t.ConnectorName, d)
}

// Schema returns the tool's input schema, defaulting to an empty object.
func (t AgentTool) Schema() json.RawMessage {
	s := t.Tool.InputSchema
	if len(s) == 0 || string(s) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return s
}

// Registry serves connectors from the state bucket.
type Registry struct {
	docs *store.Documents[Connector]

	mu          sync.RWMutex
	knownAgents map[string]bool
}

// New loads the connectors stored under Prefix.
func New(ctx context.Context, b *store.Backend) (*Registry, error) {
	r := &Registry{
		docs: store.NewDocuments(b, Prefix, func(c *Connector) error {
			if c.ID == "" || !store.IDRe.MatchString(c.ID) {
				return errors.New("missing or invalid id")
			}
			return nil
		}),
		knownAgents: map[string]bool{},
	}
	if err := r.docs.Load(ctx); err != nil {
		return nil, fmt.Errorf("load mcp connectors: %w", err)
	}
	return r, nil
}

// StartRefresh picks up connectors written by other replicas every interval.
func (r *Registry) StartRefresh(ctx context.Context, interval time.Duration) {
	r.docs.StartRefresh(ctx, interval, nil)
}

// Count is the number of registered connectors.
func (r *Registry) Count() int { return r.docs.Len() }

func docKey(id string) string { return id + ".json" }

func normalize(c *Connector) {
	if c.Transport == "" {
		c.Transport = TransportHTTP
	}
}

// SetKnownAgents restricts the agent IDs a connector may target.
func (r *Registry) SetKnownAgents(ids []string) {
	r.mu.Lock()
	r.knownAgents = make(map[string]bool, len(ids))
	for _, id := range ids {
		r.knownAgents[id] = true
	}
	r.mu.Unlock()
}

// List returns every connector with header values masked, sorted by name.
func (r *Registry) List() []Connector {
	var out []Connector
	r.docs.Range(func(_ string, c *Connector) {
		normalize(c)
		out = append(out, c.public())
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Get returns a connector with header values masked.
func (r *Registry) Get(id string) (*Connector, bool) {
	c, ok := r.docs.Get(docKey(id))
	if !ok {
		return nil, false
	}
	normalize(c)
	p := c.public()
	return &p, true
}

// Create validates and stores a new connector.
func (r *Registry) Create(ctx context.Context, in Connector) (*Connector, error) {
	c := Connector{
		Name:        strings.TrimSpace(in.Name),
		Description: strings.TrimSpace(in.Description),
		Transport:   TransportHTTP,
		URL:         strings.TrimSpace(in.URL),
		Headers:     cleanHeaders(in.Headers, nil),
		Agents:      normalizeAgents(in.Agents),
		Enabled:     in.Enabled,
		CreatedBy:   strings.TrimSpace(in.CreatedBy),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := r.validate(&c); err != nil {
		return nil, err
	}
	id, err := store.NewID()
	if err != nil {
		return nil, err
	}
	c.ID = id
	if err := r.docs.Create(ctx, docKey(id), &c); err != nil {
		return nil, err
	}
	p := c.public()
	return &p, nil
}

// Update applies a partial change. A changed URL or headers clears the
// discovered tools until the connector is tested again.
func (r *Registry) Update(ctx context.Context, id string, p Patch) (*Connector, error) {
	c, err := r.docs.Update(ctx, docKey(id), func(c *Connector) error {
		normalize(c)
		if p.Name != nil {
			c.Name = strings.TrimSpace(*p.Name)
		}
		if p.Description != nil {
			c.Description = strings.TrimSpace(*p.Description)
		}
		endpointChanged := false
		if p.URL != nil && strings.TrimSpace(*p.URL) != c.URL {
			c.URL = strings.TrimSpace(*p.URL)
			endpointChanged = true
		}
		if p.Headers != nil {
			c.Headers = cleanHeaders(*p.Headers, c.Headers)
			endpointChanged = true
		}
		if p.Agents != nil {
			c.Agents = normalizeAgents(*p.Agents)
		}
		if p.Enabled != nil {
			c.Enabled = *p.Enabled
		}
		if endpointChanged {
			c.Tools = nil
			c.ServerName, c.ServerVersion, c.ProtocolVersion = "", "", ""
			c.LastCheck, c.LastError = "", ""
		}
		if err := r.validate(c); err != nil {
			return err
		}
		c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pub := c.public()
	return &pub, nil
}

// Delete removes a connector.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if _, ok := r.docs.Get(docKey(id)); !ok {
		return ErrNotFound
	}
	if err := r.docs.Delete(ctx, docKey(id)); err != nil {
		return fmt.Errorf("delete connector: %w", err)
	}
	return nil
}

// Test performs the MCP handshake and tool discovery, recording the outcome
// on the connector. A failed probe is stored as LastError, not returned.
func (r *Registry) Test(ctx context.Context, id string) (*Connector, error) {
	snapshot, ok := r.docs.Get(docKey(id))
	if !ok {
		return nil, ErrNotFound
	}

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	client := newClient(snapshot.URL, snapshot.Headers, discoveryTimeout)
	info, err := client.Initialize(ctx)
	var tools []ToolInfo
	if err == nil {
		tools, err = client.ListTools(ctx)
	}

	c, uerr := r.docs.Update(ctx, docKey(id), func(c *Connector) error {
		normalize(c)
		c.LastCheck = time.Now().UTC().Format(time.RFC3339)
		if err != nil {
			c.LastError = err.Error()
			return nil
		}
		c.LastError = ""
		c.Tools = tools
		c.ServerName, c.ServerVersion, c.ProtocolVersion = info.Name, info.Version, info.ProtocolVersion
		return nil
	})
	if errors.Is(uerr, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if uerr != nil {
		return nil, uerr
	}
	p := c.public()
	return &p, nil
}

// ToolsFor returns the tools of every enabled connector agentID may use, each
// with a unique LLM-safe name.
func (r *Registry) ToolsFor(agentID string) []AgentTool {
	var eligible []*Connector
	r.docs.Range(func(_ string, c *Connector) {
		if c.Enabled && c.LastError == "" && len(c.Tools) > 0 && appliesTo(c, agentID) {
			eligible = append(eligible, c)
		}
	})
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	var out []AgentTool
	taken := map[string]bool{}
	for _, c := range eligible {
		prefix := "mcp_" + store.Slugify(c.Name, "server")
		for _, t := range c.Tools {
			name := llmToolName(prefix, t.Name, taken)
			taken[name] = true
			out = append(out, AgentTool{ConnectorID: c.ID, ConnectorName: c.Name, Tool: t, LLMName: name})
		}
	}
	return out
}

// Call invokes a tool on a connector and returns its text output. A tool-side
// error is returned as text prefixed with "Error" so the model can react.
func (r *Registry) Call(ctx context.Context, connectorID, tool string, args json.RawMessage) (string, error) {
	c, ok := r.docs.Get(docKey(connectorID))
	if !ok {
		return "", ErrNotFound
	}
	if !c.Enabled {
		return "", ErrDisabled
	}
	snapshot := *c

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	client := newClient(snapshot.URL, snapshot.Headers, callTimeout)
	if _, err := client.Initialize(ctx); err != nil {
		return "", err
	}
	text, isError, err := client.CallTool(ctx, tool, args)
	if err != nil {
		return "", err
	}
	if isError {
		return "Error from " + snapshot.Name + ": " + text, nil
	}
	if text == "" {
		return "(no output)", nil
	}
	return text, nil
}

func (c Connector) public() Connector {
	out := c
	if len(c.Headers) > 0 {
		out.Headers = make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			if envRefRe.MatchString(v) {
				out.Headers[k] = v
			} else {
				out.Headers[k] = MaskedValue
			}
		}
	}
	if out.Agents == nil {
		out.Agents = []string{}
	}
	return out
}

func appliesTo(c *Connector, agentID string) bool {
	if len(c.Agents) == 0 {
		return true
	}
	for _, a := range c.Agents {
		if a == agentID {
			return true
		}
	}
	return false
}

func normalizeAgents(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// cleanHeaders drops empty entries and keeps the previously stored value for
// any header sent back masked.
func cleanHeaders(in, previous map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if v == MaskedValue {
			if prev, ok := previous[k]; ok {
				out[k] = prev
			}
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *Registry) validate(c *Connector) error {
	switch {
	case c.Name == "":
		return errors.New("name is required")
	case len(c.Name) > 80:
		return errors.New("name must be at most 80 characters")
	case len(c.Description) > 240:
		return errors.New("description must be at most 240 characters")
	case c.URL == "":
		return errors.New("url is required")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	for k := range c.Headers {
		if !headerNameRe.MatchString(k) {
			return fmt.Errorf("invalid header name %q", k)
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.knownAgents) > 0 {
		for _, a := range c.Agents {
			if !r.knownAgents[a] {
				return fmt.Errorf("unknown agent %q", a)
			}
		}
	}
	return nil
}

var toolNameRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func llmToolName(prefix, tool string, taken map[string]bool) string {
	base := prefix + "_" + strings.Trim(toolNameRe.ReplaceAllString(tool, "_"), "_")
	if len(base) > maxLLMToolName {
		base = base[:maxLLMToolName]
	}
	name := base
	for i := 2; taken[name]; i++ {
		suffix := fmt.Sprintf("_%d", i)
		name = base[:min(len(base), maxLLMToolName-len(suffix))] + suffix
	}
	return name
}

// RegisterRoutes mounts the connector API:
//
//	GET    /api/mcp             → list connectors (header values masked)
//	POST   /api/mcp             → create a connector
//	GET    /api/mcp/{id}        → one connector
//	PATCH  /api/mcp/{id}        → partial update
//	DELETE /api/mcp/{id}        → delete
//	POST   /api/mcp/{id}/test   → handshake + tool discovery, result stored
//
// userFor resolves the requesting user for created_by; may be nil.
func (r *Registry) RegisterRoutes(apiMux *http.ServeMux, userFor func(*http.Request) string) {
	apiMux.HandleFunc("/api/mcp", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, r.List())
		case http.MethodPost:
			var in Connector
			if err := decodeBody(req, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if userFor != nil {
				in.CreatedBy = userFor(req)
			}
			c, err := r.Create(req.Context(), in)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusCreated, c)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	apiMux.HandleFunc("/api/mcp/{id}", func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("id")
		switch req.Method {
		case http.MethodGet:
			c, ok := r.Get(id)
			if !ok {
				http.Error(w, ErrNotFound.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, c)
		case http.MethodPatch, http.MethodPut:
			var p Patch
			if err := decodeBody(req, &p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c, err := r.Update(req.Context(), id, p)
			if err != nil {
				http.Error(w, err.Error(), statusFor(err))
				return
			}
			writeJSON(w, http.StatusOK, c)
		case http.MethodDelete:
			if err := r.Delete(req.Context(), id); err != nil {
				http.Error(w, err.Error(), statusFor(err))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	apiMux.HandleFunc("/api/mcp/{id}/test", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		c, err := r.Test(req.Context(), req.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), statusFor(err))
			return
		}
		writeJSON(w, http.StatusOK, c)
	})
}

func statusFor(err error) int {
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func decodeBody(req *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(req.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
