// Package store is the S3-backed state layer shared by every registry. A
// Backend addresses one bucket prefix; Documents caches a prefix of JSON
// descriptors in memory with write-through, conditional writes and periodic
// reconciliation; Lease coordinates replicas through conditional PutObject.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"sync"
)

// AgentRe and IDRe constrain the segments accepted in object keys and URL routes.
var (
	AgentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	IDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// MaxSegment is the longest agent or id accepted by AgentRe and IDRe.
const MaxSegment = 63

// NewID returns a 16-character hex identifier.
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify lowercases s, collapses non-alphanumerics to "-", falls back to
// fallback when empty and caps the result at 48 characters.
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

// Key returns the document key of a descriptor scoped to an agent.
func Key(agent, id string) string { return agent + "/" + id + ".json" }

// AgentKeyRe matches the keys Key produces; FlatKeyRe matches <id>.json.
// Objects under a prefix whose keys match neither are not descriptors and
// are never loaded.
var (
	AgentKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}/[a-z0-9][a-z0-9-]{0,62}\.json$`)
	FlatKeyRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}\.json$`)
)

// InstanceID identifies this process to other replicas, for lease ownership.
var InstanceID = sync.OnceValue(func() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "arbetern"
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return host + "-" + hex.EncodeToString(b)
})
