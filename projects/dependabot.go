package projects

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
)

var (
	alertSeverities = []string{"low", "medium", "high", "critical"}
	pipSeparatorsRe = regexp.MustCompile(`[-_.]+`)
)

func severityRank(s string) int { return slices.Index(alertSeverities, s) }

func alertSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if severityRank(s) < 0 {
		return ""
	}
	return s
}

func (r *Registry) scanDependabot(ctx context.Context, p *Project, led *Ledger, limits Limits, out *tickOutcome, force bool) (*Ledger, error) {
	now := time.Now().UTC()
	signal := p.signalKey()
	if !force && led.Signal == signal && now.Sub(parseTime(led.Cursor)) < alertRescan {
		return led, nil
	}
	if r.gh == nil {
		return led, errors.New("GitHub is not configured, so no Dependabot alert can be read")
	}
	var keep *regexp.Regexp
	if p.Signal.Pattern != "" {
		re, err := regexp.Compile(p.Signal.Pattern)
		if err != nil {
			return led, fmt.Errorf("signal.pattern: %w", err)
		}
		keep = re
	}
	repos, err := r.alertRepos(ctx, p.Repo)
	if err != nil {
		return led, err
	}
	var groups []Group
	// Every scan replaces the candidates and judges merged fixes, so a repository that cannot be read fails the whole scan.
	for _, name := range repos {
		alerts, err := r.gh.OpenDependabotAlerts(ctx, p.Repo.Owner, name, p.Signal.Severities, p.Signal.Ecosystems)
		if errors.Is(err, github.ErrDependabotOff) {
			log.Printf("[projects] %s: %s/%s has Dependabot alerts turned off", p.ID, p.Repo.Owner, name)
			continue
		}
		if err != nil {
			return led, fmt.Errorf("reading Dependabot alerts: %w", err)
		}
		out.scanned += len(alerts)
		groups = append(groups, alertGroups(p.Repo.Owner, name, alerts, keep, now)...)
	}
	rankAlertGroups(groups)
	out.groups = len(groups)
	grace := time.Duration(limits.RecurrenceGraceHours) * time.Hour
	updated, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.applySnapshot(signal, groups, grace, limits, now) })
	if err != nil {
		return led, fmt.Errorf("recording the scan: %w", err)
	}
	return updated, nil
}

func (r *Registry) alertRepos(ctx context.Context, repo Repo) ([]string, error) {
	if !repo.org() {
		return []string{repo.Name}, nil
	}
	list, err := r.scopeRepos(ctx, repo)
	switch {
	case err != nil:
		return nil, err
	case len(list) == 0:
		return nil, errors.New("repo.include matches none of the repositories arbetern can see")
	}
	names := make([]string, 0, len(list))
	for _, o := range list {
		names = append(names, o.Name)
	}
	return names, nil
}

func alertGroups(owner, repo string, alerts []github.DependabotAlert, keep *regexp.Regexp, now time.Time) []Group {
	byPackage := map[string][]github.DependabotAlert{}
	var keys []string
	for _, a := range alerts {
		if keep != nil && !keep.MatchString(a.Package) && !keep.MatchString(a.Manifest) {
			continue
		}
		k := packageKey(a.Ecosystem, a.Package)
		if _, seen := byPackage[k]; !seen {
			keys = append(keys, k)
		}
		a.Severity = alertSeverity(a.Severity)
		byPackage[k] = append(byPackage[k], a)
	}
	groups := make([]Group, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, dependencyGroup(alertFingerprint(owner, repo, k), repo, byPackage[k], now))
	}
	return groups
}

// packageKey compares package names as their ecosystem does: pip ignores case and treats runs of '-', '_' and '.' alike.
func packageKey(ecosystem, name string) string {
	ecosystem, name = strings.ToLower(ecosystem), strings.ToLower(name)
	if ecosystem == "pip" {
		name = pipSeparatorsRe.ReplaceAllString(name, "-")
	}
	return ecosystem + "/" + name
}

func alertFingerprint(owner, repo, pkg string) string {
	sum := sha256.Sum256([]byte("dependabot\x00" + strings.ToLower(owner+"/"+repo) + "\x00" + pkg))
	return hex.EncodeToString(sum[:8])
}

func dependencyGroup(fingerprint, repo string, alerts []github.DependabotAlert, now time.Time) Group {
	slices.SortFunc(alerts, func(a, b github.DependabotAlert) int {
		return cmp.Or(cmp.Compare(severityRank(b.Severity), severityRank(a.Severity)), strings.Compare(a.Manifest, b.Manifest), cmp.Compare(a.Number, b.Number))
	})
	top := alerts[0]
	d := &Dependency{Repo: repo, Ecosystem: oneLine(top.Ecosystem, 40), Package: oneLine(top.Package, 200), Severity: top.Severity}
	g := Group{Fingerprint: fingerprint, Count: len(alerts), LastSeen: stamp(now), Dependency: d}
	for _, a := range alerts {
		g.FirstSeen = earliest(g.FirstSeen, stamp(a.CreatedAt))
		if len(d.Alerts) < maxGroupAlerts {
			d.Alerts = append(d.Alerts, Alert{
				Number:       a.Number,
				Advisory:     oneLine(a.GHSA, 40),
				Manifest:     oneLine(a.Manifest, 300),
				Scope:        oneLine(a.Scope, 20),
				Relationship: oneLine(a.Relationship, 20),
				CreatedAt:    stamp(a.CreatedAt),
			})
		}
		if len(d.Advisories) < maxGroupAdvisories && !slices.ContainsFunc(d.Advisories, func(x Advisory) bool { return x.ID == a.GHSA }) {
			d.Advisories = append(d.Advisories, Advisory{
				ID:         oneLine(a.GHSA, 40),
				CVE:        oneLine(a.CVE, 40),
				Severity:   a.Severity,
				Summary:    oneLine(a.Summary, maxAdvisoryChars),
				Vulnerable: oneLine(a.Vulnerable, 200),
				Patched:    oneLine(a.Patched, 100),
			})
		}
	}
	return g
}

// rankAlertGroups puts the most severe dependency with the most alerts first; one with no patched release waits behind the rest.
func rankAlertGroups(groups []Group) {
	patched := func(g Group) int {
		if slices.ContainsFunc(g.Dependency.Advisories, func(a Advisory) bool { return a.Patched != "" }) {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(groups, func(a, b Group) int {
		return cmp.Or(
			cmp.Compare(patched(b), patched(a)),
			cmp.Compare(severityRank(b.Dependency.Severity), severityRank(a.Dependency.Severity)),
			cmp.Compare(b.Count, a.Count),
			strings.Compare(a.Dependency.Repo, b.Dependency.Repo),
			strings.Compare(a.Dependency.Package, b.Dependency.Package),
		)
	})
}

func (r *Registry) taskRepo(ctx context.Context, p *Project, g Group) (Repo, error) {
	name := g.repo()
	if name == "" || !p.Repo.org() {
		return Repo{}, nil
	}
	found, ok, err := r.scopeRepo(ctx, p.Repo, name)
	switch {
	case err != nil:
		return Repo{}, err
	case !ok:
		return Repo{}, fmt.Errorf("%s/%s is no longer one of the repositories this project may change", p.Repo.Owner, name)
	}
	return Repo{Owner: p.Repo.Owner, Name: found.Name, BaseBranch: found.DefaultBranch}, nil
}
