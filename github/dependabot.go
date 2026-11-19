package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v91/github"
)

const (
	dependabotPageSize = 100
	maxDependabotPages = 30
)

// ErrDependabotOff is returned for a repository whose Dependabot alerts are turned off.
var ErrDependabotOff = errors.New("the repository has Dependabot alerts turned off")

// DependabotAlert is one open Dependabot alert: a vulnerable version of a package that one manifest pins.
type DependabotAlert struct {
	Number       int
	Ecosystem    string
	Package      string
	Manifest     string
	Scope        string // runtime | development; empty when GitHub does not say
	Relationship string // direct | transitive | inconclusive | unknown; empty when GitHub does not say
	Severity     string // low | medium | high | critical
	GHSA         string
	CVE          string
	Summary      string
	Vulnerable   string // vulnerable version range
	Patched      string // first patched version; empty while none is published
	CreatedAt    time.Time
}

// go-github's Dependency type has no relationship field, so alerts are decoded here.
type dependabotAlertJSON struct {
	Number     int       `json:"number"`
	CreatedAt  time.Time `json:"created_at"`
	Dependency struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		ManifestPath string `json:"manifest_path"`
		Scope        string `json:"scope"`
		Relationship string `json:"relationship"`
	} `json:"dependency"`
	Advisory struct {
		GHSA     string `json:"ghsa_id"`
		CVE      string `json:"cve_id"`
		Summary  string `json:"summary"`
		Severity string `json:"severity"`
	} `json:"security_advisory"`
	Vulnerability struct {
		Severity     string `json:"severity"`
		Range        string `json:"vulnerable_version_range"`
		FirstPatched *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
}

func (a dependabotAlertJSON) alert() DependabotAlert {
	patched := ""
	if a.Vulnerability.FirstPatched != nil {
		patched = a.Vulnerability.FirstPatched.Identifier
	}
	return DependabotAlert{
		Number:       a.Number,
		Ecosystem:    a.Dependency.Package.Ecosystem,
		Package:      a.Dependency.Package.Name,
		Manifest:     a.Dependency.ManifestPath,
		Scope:        a.Dependency.Scope,
		Relationship: a.Dependency.Relationship,
		Severity:     cmp.Or(a.Advisory.Severity, a.Vulnerability.Severity),
		GHSA:         a.Advisory.GHSA,
		CVE:          a.Advisory.CVE,
		Summary:      a.Advisory.Summary,
		Vulnerable:   a.Vulnerability.Range,
		Patched:      patched,
		CreatedAt:    a.CreatedAt,
	}
}

// OpenDependabotAlerts lists a repository's open Dependabot alerts, narrowed to the given severities and ecosystems when any are named.
func (c *Client) OpenDependabotAlerts(ctx context.Context, owner, repo string, severities, ecosystems []string) ([]DependabotAlert, error) {
	q := url.Values{"state": {"open"}, "per_page": {strconv.Itoa(dependabotPageSize)}}
	if len(severities) > 0 {
		q.Set("severity", strings.Join(severities, ","))
	}
	if len(ecosystems) > 0 {
		q.Set("ecosystem", strings.Join(ecosystems, ","))
	}
	path := "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/dependabot/alerts?"
	var out []DependabotAlert
	for range maxDependabotPages {
		req, err := c.api.NewRequest(ctx, http.MethodGet, path+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page []dependabotAlertJSON
		resp, err := c.api.Do(req, &page)
		switch {
		case dependabotOff(err):
			return nil, ErrDependabotOff
		case err != nil:
			return nil, fmt.Errorf("failed to list the Dependabot alerts of %s/%s: %w", owner, repo, err)
		}
		for _, a := range page {
			out = append(out, a.alert())
		}
		if resp.After == "" {
			return out, nil
		}
		q.Set("after", resp.After)
	}
	return nil, fmt.Errorf("%s/%s has more than %d open Dependabot alerts", owner, repo, maxDependabotPages*dependabotPageSize)
}

// dependabotOff tells alerts that are turned off from a token that may not read them: GitHub answers both with 403.
func dependabotOff(err error) bool {
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusForbidden &&
		strings.Contains(strings.ToLower(ghErr.Message), "disabled")
}
