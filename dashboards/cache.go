package dashboards

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// AccountCacheTTL is the default per-account cache lifetime. First request of
// the day fetches fresh data; subsequent requests within TTL serve from disk
// unless the caller passes refresh=true.
const AccountCacheTTL = 24 * time.Hour

// AccountCache is a tiny filesystem-backed cache for account-dashboard
// snapshots. Stored at <dashboardsDir>/_cache/account/<slug>.json so it
// survives pod restarts on the same PVC that backs the dashboard registry.
type AccountCache struct {
	root string
	ttl  time.Duration
}

// NewAccountCache returns a cache rooted at <dir>/_cache/account.
// Using an underscore prefix keeps the cache directory out of Registry.LoadAll.
func NewAccountCache(dir string) (*AccountCache, error) {
	root := filepath.Join(dir, "_cache", "account")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create account cache dir: %w", err)
	}
	return &AccountCache{root: root, ttl: AccountCacheTTL}, nil
}

// Get returns a cached dashboard when one exists and is younger than ttl.
// (nil, false) is returned on miss or stale. IO errors are treated as misses.
func (c *AccountCache) Get(slug string) (*Dashboard, bool) {
	slug = CacheSlug(slug)
	if slug == "" {
		return nil, false
	}
	path := filepath.Join(c.root, slug+".json")
	st, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(st.ModTime()) > c.ttl {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var d Dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, false
	}
	return &d, true
}

// Put atomically persists a dashboard snapshot under its account slug.
func (c *AccountCache) Put(slug string, d *Dashboard) error {
	slug = CacheSlug(slug)
	if slug == "" {
		return fmt.Errorf("cache slug is empty")
	}
	path := filepath.Join(c.root, slug+".json")
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Invalidate removes a cached entry, ignoring missing-file errors.
func (c *AccountCache) Invalidate(slug string) error {
	slug = CacheSlug(slug)
	if slug == "" {
		return nil
	}
	path := filepath.Join(c.root, slug+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

var cacheSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// CacheSlug normalises a free-form account name into a safe filename segment.
// Exported so the command layer can reason about cache hits before calling Get.
func CacheSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = cacheSlugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
