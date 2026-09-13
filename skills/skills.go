// Package skills manages instruction blocks appended to agent system prompts.
// Built-in skills mirror the prompt files shipped with each agent and are
// read-only; custom skills are created from the management UI, stored under
// Prefix in the state bucket, and injected into the prompts of the agents
// they target.
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justmike1/arbetern/internal/store"
)

// Prefix is the object prefix custom skills are stored under.
const Prefix = "skills/"

const (
	KindBuiltin = "builtin"
	KindCustom  = "custom"
	ScopeGlobal = "global"
	ScopeAgent  = "agent"

	maxNameLen         = 80
	maxDescriptionLen  = 240
	maxInstructionsLen = 16 << 10
)

// Skill is one instruction block. Agents is the allowlist of agent IDs it
// applies to; empty means every agent.
type Skill struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Instructions string   `json:"instructions"`
	Agents       []string `json:"agents"`
	Enabled      bool     `json:"enabled"`
	Kind         string   `json:"kind"`
	Scope        string   `json:"scope"`
	Source       string   `json:"source,omitempty"`
	CreatedBy    string   `json:"created_by,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
}

// Patch carries the fields a PATCH request may change; nil means unchanged.
type Patch struct {
	Name         *string   `json:"name"`
	Description  *string   `json:"description"`
	Instructions *string   `json:"instructions"`
	Agents       *[]string `json:"agents"`
	Enabled      *bool     `json:"enabled"`
}

var (
	ErrNotFound = errors.New("skill not found")
	ErrReadOnly = errors.New("built-in skills are defined in the agent prompt files and cannot be changed here")
)

// Registry serves custom skills from the state bucket plus the read-only
// built-in ones derived from prompt files.
type Registry struct {
	docs *store.Documents[Skill]

	mu          sync.RWMutex
	builtin     func() []Skill
	knownAgents map[string]bool
}

// New loads the custom skills stored under Prefix.
func New(ctx context.Context, b *store.Backend) (*Registry, error) {
	r := &Registry{
		docs: store.NewDocuments(b, Prefix, store.FlatKeyRe, func(s *Skill) error {
			if s.ID == "" || !store.IDRe.MatchString(s.ID) {
				return errors.New("missing or invalid id")
			}
			return nil
		}),
		knownAgents: map[string]bool{},
	}
	if err := r.docs.Load(ctx); err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	return r, nil
}

// StartRefresh picks up skills written by other replicas every interval.
func (r *Registry) StartRefresh(ctx context.Context, interval time.Duration) {
	r.docs.StartRefresh(ctx, interval, nil)
}

// Count is the number of custom skills.
func (r *Registry) Count() int { return r.docs.Len() }

func docKey(id string) string { return id + ".json" }

func normalize(s *Skill) {
	s.Kind = KindCustom
	if s.Scope == "" {
		s.Scope = scopeFor(s.Agents)
	}
}

// SetBuiltin installs the provider of read-only skills derived from prompts.
func (r *Registry) SetBuiltin(fn func() []Skill) {
	r.mu.Lock()
	r.builtin = fn
	r.mu.Unlock()
}

// SetKnownAgents restricts the agent IDs a skill may target.
func (r *Registry) SetKnownAgents(ids []string) {
	r.mu.Lock()
	r.knownAgents = make(map[string]bool, len(ids))
	for _, id := range ids {
		r.knownAgents[id] = true
	}
	r.mu.Unlock()
}

// List returns built-in skills (global first, then per agent) followed by
// custom skills sorted by name.
func (r *Registry) List() []Skill {
	r.mu.RLock()
	builtin := r.builtin
	r.mu.RUnlock()
	out := make([]Skill, 0, r.docs.Len())
	if builtin != nil {
		out = append(out, builtin()...)
	}
	custom := make([]Skill, 0, r.docs.Len())
	r.docs.Range(func(_ string, s *Skill) {
		normalize(s)
		custom = append(custom, *s)
	})
	sort.Slice(custom, func(i, j int) bool { return strings.ToLower(custom[i].Name) < strings.ToLower(custom[j].Name) })
	return append(out, custom...)
}

// Get returns a custom skill by ID.
func (r *Registry) Get(id string) (*Skill, bool) {
	s, ok := r.docs.Get(docKey(id))
	if !ok {
		return nil, false
	}
	normalize(s)
	return s, true
}

// Create validates and stores a new custom skill.
func (r *Registry) Create(ctx context.Context, in Skill) (*Skill, error) {
	s := Skill{
		Name:         strings.TrimSpace(in.Name),
		Description:  strings.TrimSpace(in.Description),
		Instructions: strings.TrimSpace(in.Instructions),
		Agents:       normalizeAgents(in.Agents),
		Enabled:      in.Enabled,
		Kind:         KindCustom,
		Source:       "ui",
		CreatedBy:    strings.TrimSpace(in.CreatedBy),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := r.validate(&s); err != nil {
		return nil, err
	}
	id, err := store.NewID()
	if err != nil {
		return nil, err
	}
	s.ID = id
	s.Scope = scopeFor(s.Agents)
	if err := r.docs.Create(ctx, docKey(id), &s); err != nil {
		return nil, err
	}
	cp := s
	return &cp, nil
}

// Update applies a partial change to a custom skill.
func (r *Registry) Update(ctx context.Context, id string, p Patch) (*Skill, error) {
	if strings.HasPrefix(id, "builtin-") {
		return nil, ErrReadOnly
	}
	s, err := r.docs.Update(ctx, docKey(id), func(s *Skill) error {
		normalize(s)
		if p.Name != nil {
			s.Name = strings.TrimSpace(*p.Name)
		}
		if p.Description != nil {
			s.Description = strings.TrimSpace(*p.Description)
		}
		if p.Instructions != nil {
			s.Instructions = strings.TrimSpace(*p.Instructions)
		}
		if p.Agents != nil {
			s.Agents = normalizeAgents(*p.Agents)
		}
		if p.Enabled != nil {
			s.Enabled = *p.Enabled
		}
		if err := r.validate(s); err != nil {
			return err
		}
		s.Scope = scopeFor(s.Agents)
		s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	return s, err
}

// Delete removes a custom skill.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if strings.HasPrefix(id, "builtin-") {
		return ErrReadOnly
	}
	if _, ok := r.docs.Get(docKey(id)); !ok {
		return ErrNotFound
	}
	if err := r.docs.Delete(ctx, docKey(id)); err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	return nil
}

// Instructions returns the prompt blocks of every enabled custom skill that
// applies to agentID, in name order.
func (r *Registry) Instructions(agentID string) []string {
	var list []*Skill
	r.docs.Range(func(_ string, s *Skill) {
		if s.Enabled && appliesTo(s, agentID) {
			list = append(list, s)
		}
	})
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, fmt.Sprintf("Skill — %s:\n%s", s.Name, s.Instructions))
	}
	return out
}

func appliesTo(s *Skill, agentID string) bool {
	if len(s.Agents) == 0 {
		return true
	}
	for _, a := range s.Agents {
		if a == agentID {
			return true
		}
	}
	return false
}

func scopeFor(agents []string) string {
	if len(agents) == 0 {
		return ScopeGlobal
	}
	return ScopeAgent
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

func (r *Registry) validate(s *Skill) error {
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case len(s.Name) > maxNameLen:
		return fmt.Errorf("name must be at most %d characters", maxNameLen)
	case len(s.Description) > maxDescriptionLen:
		return fmt.Errorf("description must be at most %d characters", maxDescriptionLen)
	case s.Instructions == "":
		return errors.New("instructions are required")
	case len(s.Instructions) > maxInstructionsLen:
		return fmt.Errorf("instructions must be at most %d characters", maxInstructionsLen)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.knownAgents) > 0 {
		for _, a := range s.Agents {
			if !r.knownAgents[a] {
				return fmt.Errorf("unknown agent %q", a)
			}
		}
	}
	return nil
}

// Humanize turns a prompt key like "slack_formatting" into "Slack formatting".
func Humanize(key string) string {
	words := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(key, "_", " "), "-", " "))
	if len(words) == 0 {
		return key
	}
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

// FirstLine returns the first non-empty line of text, trimmed to max runes.
func FirstLine(text string, max int) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-•*# "))
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > max {
			return string(r[:max-1]) + "…"
		}
		return line
	}
	return ""
}

// RegisterRoutes mounts the skills API:
//
//	GET    /api/skills          → list (built-in + custom)
//	POST   /api/skills          → create a custom skill
//	GET    /api/skills/{id}     → one custom skill
//	PATCH  /api/skills/{id}     → partial update
//	DELETE /api/skills/{id}     → delete
//
// userFor resolves the requesting user for created_by; may be nil.
func (r *Registry) RegisterRoutes(apiMux *http.ServeMux, userFor func(*http.Request) string) {
	apiMux.HandleFunc("/api/skills", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, r.List())
		case http.MethodPost:
			var in Skill
			if err := decodeBody(req, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if userFor != nil {
				in.CreatedBy = userFor(req)
			}
			s, err := r.Create(req.Context(), in)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusCreated, s)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	apiMux.HandleFunc("/api/skills/{id}", func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("id")
		switch req.Method {
		case http.MethodGet:
			s, ok := r.Get(id)
			if !ok {
				http.Error(w, ErrNotFound.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, s)
		case http.MethodPatch, http.MethodPut:
			var p Patch
			if err := decodeBody(req, &p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s, err := r.Update(req.Context(), id, p)
			if err != nil {
				http.Error(w, err.Error(), statusFor(err))
				return
			}
			writeJSON(w, http.StatusOK, s)
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
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrReadOnly):
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

func decodeBody(req *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(req.Body, 64<<10))
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
