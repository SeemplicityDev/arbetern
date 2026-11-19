// Package pypi reads public release metadata from the Python Package Index.
package pypi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/httpx"
	"github.com/justmike1/arbetern/internal/ttlcache"
)

const (
	baseURL        = "https://pypi.org/pypi/"
	maxProjectBody = 64 << 20 // a package's JSON lists every file of every release
	maxReleaseBody = 8 << 20
	maxAttempts    = 3
	fetchTimeout   = time.Minute
	cacheTTL       = 10 * time.Minute
	cacheEntries   = 256
)

var (
	nameRe       = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,98}[A-Za-z0-9])?$`)
	versionRe    = regexp.MustCompile(`^[0-9][0-9A-Za-z.!+_-]{0,63}$`)
	separatorsRe = regexp.MustCompile(`[-_.]+`)
)

// ErrNotFound is returned when PyPI has no such package or release.
var ErrNotFound = errors.New("not found on PyPI")

// Project is a package's latest version and its releases, newest first.
type Project struct {
	Name     string
	Latest   string
	Releases []Summary
}

// Summary is one release of a package.
type Summary struct {
	Version        string
	UploadedAt     time.Time
	RequiresPython string
	Yanked         bool
}

// Release is one version of a package: what it requires and the files it ships.
type Release struct {
	Name           string
	Version        string
	RequiresPython string
	RequiresDist   []string
	Yanked         bool
	Files          []File
}

// File is one distribution of a release.
type File struct {
	Filename   string
	URL        string
	SHA256     string
	Size       int64
	UploadedAt time.Time
	Sdist      bool
}

type fileJSON struct {
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	PackageType string `json:"packagetype"`
	Size        int64  `json:"size"`
	Digests     struct {
		SHA256 string `json:"sha256"`
	} `json:"digests"`
	RequiresPython string `json:"requires_python"`
	UploadTime     string `json:"upload_time_iso_8601"`
	Yanked         bool   `json:"yanked"`
}

func (f fileJSON) file() File {
	at, _ := time.Parse(time.RFC3339Nano, f.UploadTime)
	return File{Filename: f.Filename, URL: f.URL, SHA256: f.Digests.SHA256, Size: f.Size, UploadedAt: at, Sdist: f.PackageType == "sdist"}
}

// Client reads package metadata from PyPI's JSON API.
type Client struct {
	http     *http.Client
	base     string
	projects *ttlcache.Keyed[string, *Project]
	releases *ttlcache.Keyed[string, *Release]
}

// NewClient returns a client for pypi.org.
func NewClient() *Client {
	c := &Client{http: &http.Client{Timeout: fetchTimeout}, base: baseURL}
	c.projects = ttlcache.NewKeyed(cacheTTL, cacheEntries, c.fetchProject)
	c.releases = ttlcache.NewKeyed(cacheTTL, cacheEntries, c.fetchRelease)
	return c
}

// Normalize returns a package name the way PyPI compares names (PEP 503).
func Normalize(name string) string {
	return separatorsRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("%q is not a Python package name", name)
	}
	return Normalize(name), nil
}

// Project returns a package's latest version and its releases, newest first.
func (c *Client) Project(ctx context.Context, name string) (*Project, error) {
	name, err := checkName(name)
	if err != nil {
		return nil, err
	}
	return c.projects.Get(ctx, name)
}

// Release returns what one version of a package requires and the files it ships.
func (c *Client) Release(ctx context.Context, name, version string) (*Release, error) {
	name, err := checkName(name)
	if err != nil {
		return nil, err
	}
	if version = strings.TrimSpace(version); !versionRe.MatchString(version) {
		return nil, fmt.Errorf("%q is not a package version", version)
	}
	return c.releases.Get(ctx, name+"/"+version)
}

func (c *Client) fetchProject(ctx context.Context, name string) (*Project, error) {
	var raw struct {
		Info struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string][]fileJSON `json:"releases"`
	}
	if err := c.get(ctx, url.PathEscape(name)+"/json", maxProjectBody, &raw); err != nil {
		return nil, err
	}
	p := &Project{Name: cmp.Or(raw.Info.Name, name), Latest: raw.Info.Version}
	for version, files := range raw.Releases {
		if len(files) == 0 {
			continue
		}
		s := Summary{Version: version, Yanked: true}
		for _, f := range files {
			at, _ := time.Parse(time.RFC3339Nano, f.UploadTime)
			if s.UploadedAt.IsZero() || (!at.IsZero() && at.Before(s.UploadedAt)) {
				s.UploadedAt = at
			}
			s.RequiresPython = cmp.Or(s.RequiresPython, f.RequiresPython)
			s.Yanked = s.Yanked && f.Yanked
		}
		p.Releases = append(p.Releases, s)
	}
	slices.SortFunc(p.Releases, func(a, b Summary) int {
		return cmp.Or(b.UploadedAt.Compare(a.UploadedAt), strings.Compare(b.Version, a.Version))
	})
	return p, nil
}

func (c *Client) fetchRelease(ctx context.Context, key string) (*Release, error) {
	name, version, _ := strings.Cut(key, "/")
	var raw struct {
		Info struct {
			Name           string   `json:"name"`
			Version        string   `json:"version"`
			RequiresPython string   `json:"requires_python"`
			RequiresDist   []string `json:"requires_dist"`
			Yanked         bool     `json:"yanked"`
		} `json:"info"`
		URLs []fileJSON `json:"urls"`
	}
	if err := c.get(ctx, url.PathEscape(name)+"/"+url.PathEscape(version)+"/json", maxReleaseBody, &raw); err != nil {
		return nil, err
	}
	r := &Release{Name: cmp.Or(raw.Info.Name, name), Version: cmp.Or(raw.Info.Version, version), RequiresPython: raw.Info.RequiresPython,
		RequiresDist: raw.Info.RequiresDist, Yanked: raw.Info.Yanked}
	for _, f := range raw.URLs {
		r.Files = append(r.Files, f.file())
	}
	return r, nil
}

// A cached fetch is shared by every caller, so one caller's cancellation must not fail it for the others.
func (c *Client) get(ctx context.Context, path string, limit int64, v any) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		retry, hint, err := c.try(ctx, path, limit, v)
		if !retry || attempt == maxAttempts {
			return err
		}
		if err := httpx.SleepCtx(ctx, httpx.Backoff(attempt, hint)); err != nil {
			return err
		}
	}
}

func (c *Client) try(ctx context.Context, path string, limit int64, v any) (retry bool, hint time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ctx.Err() == nil, 0, fmt.Errorf("reading PyPI: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, 0, ErrNotFound
	case httpx.RetryableStatus(resp.StatusCode):
		return true, httpx.RetryAfter(resp.Header), fmt.Errorf("PyPI answered %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return false, 0, fmt.Errorf("PyPI answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(v); err != nil {
		return false, 0, fmt.Errorf("decoding PyPI's answer: %w", err)
	}
	return false, 0, nil
}
