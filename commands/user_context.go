package commands

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// UserContextStore persists a small rolling log of per-user / per-agent
// interactions on local disk, so that follow-up requests from the same user
// can be enriched with prior-topic context. The store is intentionally
// ephemeral — files live outside the persistent mount and are garbage
// collected after UserContextTTL of inactivity.
//
// Layout: <BaseDir>/<agentID>/<userID>/context.txt
type UserContextStore struct {
	baseDir string
	ttl     time.Duration
	mu      sync.Mutex // serialises read/append per-process; files are small
}

const (
	// UserContextTTL is the garbage collection horizon for context files.
	UserContextTTL = 7 * 24 * time.Hour
	// userContextMaxEntries caps the number of turns kept in a single file
	// so it never grows unbounded.
	userContextMaxEntries = 30
	// userContextMaxAnswerLen truncates assistant responses before storing
	// them — we only need a topical hint, not the full output.
	userContextMaxAnswerLen = 2000
	// userContextMaxQuestionLen truncates the user question similarly.
	userContextMaxQuestionLen = 1500
	// userContextMaxFileBytes is a hard ceiling on the file size. Defence in
	// depth against pathological inputs slipping past the per-entry limits.
	userContextMaxFileBytes = 128 * 1024
	// userContextEntrySep separates entries inside the file.
	userContextEntrySep = "\n---\n"
)

// safeIDRe restricts agent IDs and Slack user IDs to characters that are
// safe for filesystem paths. Slack IDs are `[A-Z0-9]+` and our agent IDs
// are lowercase alnum + dashes.
var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// NewUserContextStore returns a store rooted at baseDir. If baseDir is
// empty, a subdirectory of the OS temp dir is used. The directory is
// created lazily on write; missing-directory reads are not errors.
func NewUserContextStore(baseDir string) *UserContextStore {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = filepath.Join(os.TempDir(), "arbetern-user-context")
	}
	return &UserContextStore{
		baseDir: baseDir,
		ttl:     UserContextTTL,
	}
}

// BaseDir returns the root directory the store writes to.
func (s *UserContextStore) BaseDir() string { return s.baseDir }

// pathFor returns the on-disk path for the given agent/user pair, or empty
// string if either identifier is unsafe.
func (s *UserContextStore) pathFor(agentID, userID string) string {
	if !safeIDRe.MatchString(agentID) || !safeIDRe.MatchString(userID) {
		return ""
	}
	return filepath.Join(s.baseDir, agentID, userID, "context.txt")
}

// Read returns the stored context for the user, or an empty string if the
// file does not exist or cannot be read. Missing files are not logged.
func (s *UserContextStore) Read(agentID, userID string) string {
	if s == nil {
		return ""
	}
	p := s.pathFor(agentID, userID)
	if p == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[user-context] read failed agent=%s user=%s: %v", agentID, userID, err)
		}
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Append records a new (question, answer) turn for the user. It keeps only
// the most recent userContextMaxEntries turns and caps the file size.
// Errors are logged but never returned — context persistence is best-effort.
func (s *UserContextStore) Append(agentID, userID, question, answer string) {
	if s == nil {
		return
	}
	p := s.pathFor(agentID, userID)
	if p == "" {
		return
	}
	question = truncate(strings.TrimSpace(question), userContextMaxQuestionLen)
	answer = truncate(strings.TrimSpace(answer), userContextMaxAnswerLen)
	if question == "" && answer == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		log.Printf("[user-context] mkdir failed agent=%s user=%s: %v", agentID, userID, err)
		return
	}

	existing, _ := os.ReadFile(p)
	entries := splitEntries(string(existing))

	newEntry := fmt.Sprintf("[%s]\nQ: %s\nA: %s",
		time.Now().UTC().Format(time.RFC3339),
		question,
		answer,
	)
	entries = append(entries, newEntry)
	if len(entries) > userContextMaxEntries {
		entries = entries[len(entries)-userContextMaxEntries:]
	}

	out := strings.Join(entries, userContextEntrySep)
	if len(out) > userContextMaxFileBytes {
		// Drop oldest entries until under the byte cap.
		for len(entries) > 1 && len(out) > userContextMaxFileBytes {
			entries = entries[1:]
			out = strings.Join(entries, userContextEntrySep)
		}
	}

	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		log.Printf("[user-context] write failed agent=%s user=%s: %v", agentID, userID, err)
	}
}

func splitEntries(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, userContextEntrySep)
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// StartGC launches a background goroutine that sweeps the store every
// `interval`, deleting any context file whose mtime is older than the
// store's TTL. The goroutine exits when ctx is cancelled.
func (s *UserContextStore) StartGC(ctx context.Context, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		// Run once at startup so stale files are cleared even if the
		// process restarts before the first tick.
		s.sweep()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweep()
			}
		}
	}()
}

// sweep removes context.txt files older than the TTL and prunes empty dirs.
func (s *UserContextStore) sweep() {
	cutoff := time.Now().Add(-s.ttl)
	removed := 0
	err := filepath.WalkDir(s.baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if rerr := os.Remove(path); rerr == nil {
				removed++
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		log.Printf("[user-context] sweep error: %v", err)
	}
	// Prune empty user/agent directories.
	s.pruneEmptyDirs()
	if removed > 0 {
		log.Printf("[user-context] gc removed %d stale file(s)", removed)
	}
}

func (s *UserContextStore) pruneEmptyDirs() {
	_ = filepath.WalkDir(s.baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == s.baseDir {
			return nil
		}
		entries, rerr := os.ReadDir(path)
		if rerr == nil && len(entries) == 0 {
			_ = os.Remove(path)
		}
		return nil
	})
}
