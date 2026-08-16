// Package store contains small helpers shared by registries that persist
// JSON descriptors on disk under `<root>/<agent>/<id>.json`.
//
// The two existing registries (workflows and dashboards) historically
// duplicated this code verbatim; centralising it keeps id/slug rules and
// the atomic-write contract in one place.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AgentRe and IDRe constrain the path segments we accept on disk and in
// URL routing. Both use the same alphabet — lowercase alphanumerics and
// hyphens, 1..63 chars, must start with an alphanumeric.
var (
	AgentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	IDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// MaxSegment is the longest agent/id accepted by AgentRe and IDRe. Callers
// that derive an id from user input (e.g. dashboard prompt instances) must
// keep the result within this bound or the descriptor will be rejected here
// and, if it were written anyway, dropped on the next load.
const MaxSegment = 63

// ErrUnsafePath is returned when an agent or id would resolve to a path
// outside its root directory, or is otherwise not a legal path segment.
var ErrUnsafePath = errors.New("unsafe descriptor path")

// contained returns the cleaned form of p when it lies inside root, and
// ErrUnsafePath otherwise. Agent and id values reach the store from HTTP
// routes, GitOps YAML and LLM tool calls, so containment is enforced here at
// the filesystem sink rather than trusting every caller to have validated
// first (CWE-22, path traversal).
func contained(root, p string) (string, error) {
	cleanRoot := filepath.Clean(root)
	clean := filepath.Clean(p)
	if clean != cleanRoot && !strings.HasPrefix(clean, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q escapes %q", ErrUnsafePath, clean, cleanRoot)
	}
	return clean, nil
}

// NewID returns an 8-byte hex identifier (16 chars). Suitable for both
// workflows and dashboards.
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify lowercases s, replaces runs of non-alphanumerics with `-`, trims
// leading/trailing `-`, falls back to fallback when empty, and caps the
// result at 48 chars.
func Slugify(s, fallback string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = fallback
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// PathFor returns the on-disk path for a descriptor at <root>/<agent>/<id>.json.
// It rejects any agent or id that is not a legal path segment, or that would
// resolve outside root.
func PathFor(root, agent, id string) (string, error) {
	dir, err := AgentDir(root, agent)
	if err != nil {
		return "", err
	}
	if !IDRe.MatchString(id) {
		return "", fmt.Errorf("%w: invalid id %q", ErrUnsafePath, id)
	}
	return contained(root, filepath.Join(dir, id+".json"))
}

// AgentDir returns the directory holding an agent's descriptors: <root>/<agent>.
// It rejects any agent that is not a legal path segment, or that would resolve
// outside root.
func AgentDir(root, agent string) (string, error) {
	if !AgentRe.MatchString(agent) {
		return "", fmt.Errorf("%w: invalid agent %q", ErrUnsafePath, agent)
	}
	return contained(root, filepath.Join(root, agent))
}

// WriteJSON atomically writes v as indented JSON to <root>/<agent>/<id>.json,
// creating the agent directory as needed. The write goes to <path>.tmp first
// and is renamed into place so concurrent readers always see a complete file.
func WriteJSON(root, agent, id string, v any) error {
	path, err := PathFor(root, agent, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteJSONAt atomically writes v to a path under root. Used by callers
// (e.g. the account-dashboard cache) whose layout is not <root>/<agent>/<id>;
// root is the directory the caller owns, and path must resolve inside it.
func WriteJSONAt(root, path string, v any) error {
	path, err := contained(root, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSON unmarshals a JSON file into a fresh *T. The optional validate
// hook lets the caller enforce required fields (typical pattern: check ID
// and Agent against AgentRe / IDRe). validate may be nil.
func ReadJSON[T any](path string, validate func(*T) error) (*T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(&v); err != nil {
			return nil, fmt.Errorf("invalid descriptor: %w", err)
		}
	}
	return &v, nil
}
