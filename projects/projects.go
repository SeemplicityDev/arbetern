// Package projects gives arbetern standing goals on GitHub repositories, pursued by Claude Code sessions it runs as Jobs in its own cluster.
package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/llm"
)

const (
	Prefix        = "projects/"
	StatePrefix   = "project-state/"
	SessionPrefix = "project-sessions/"
	QueueTopic    = "project-tick"

	SignalDatadog        = "datadog"
	SignalDatadogMetrics = "datadog_metrics"

	StatusDispatching = "dispatching"
	StatusRunning     = "running"
	StatusPROpen      = "pr_open"
	StatusMerged      = "merged"
	StatusClosed      = "closed" // PR closed without merge
	StatusNoFix       = "no_fix" // session reported it could not or need not fix
	StatusFailed      = "failed" // dispatch failed, lost, timed out, or ended without a report

	OutcomeFixed           = "fixed"
	OutcomeNotReproducible = "not_reproducible"
	OutcomeCannotFix       = "cannot_fix"
	OutcomeAlreadyFixed    = "already_fixed"
	OutcomeAnswered        = "answered" // follow-up round only: replied without changing code

	PhaseChecking  = "checking"  // waiting for checks, comments or the quiet period
	PhaseFixing    = "fixing"    // a follow-up round is running
	PhaseWaiting   = "waiting"   // a question or blocker needs a person
	PhaseDone      = "done"      // checks pass and every comment is answered
	PhaseExhausted = "exhausted" // the follow-up rounds are used up

	RoundDone = "done"

	BranchPrefix = "claude/"

	MaxConsecutiveFailures = 3
)

const (
	tickTimeout        = 5 * time.Minute
	queueTimeout       = 10 * time.Minute
	leaseTTL           = 10 * time.Minute
	persistTimeout     = 30 * time.Second
	viewTimeout        = 10 * time.Second
	viewCacheTTL       = 5 * time.Second
	orgReposTTL        = 10 * time.Minute
	maxStagger         = 60 * time.Second
	dispatchLostAfter  = 15 * time.Minute
	runningGrace       = 30 * time.Minute
	jobGoneAfter       = 10 * time.Minute
	lostDispatch       = "dispatch outcome unknown"
	jobFinished        = "session ended without a report (the Job finished)"
	failedCooldown     = time.Hour
	unreadablePRGrace  = 72 * time.Hour
	openingStaleAfter  = 2 * time.Minute
	ingestionLag       = 2 * time.Minute
	maxTasks           = 200
	maxCandidates      = 50
	candidateMaxAge    = 7 * 24 * time.Hour
	maxTracks          = 2000
	dailyKeepDays      = 90
	statsDays          = 30
	ringSize           = 50
	maxNotes           = 60
	maxNoteChars       = 500
	maxLearnings       = 5
	maxOpenAttempts    = 10
	maxPRSync          = 20
	scanMaxEvents      = 5000
	maxScanCatchUp     = 24 * time.Hour
	scanMaxSamples     = 3
	maxServiceChars    = 200
	maxRepoPatterns    = 20
	maxBriefRepos      = 60
	maxTicketCloses    = 5
	ticketDoneWindow   = 14 * 24 * time.Hour
	sampleMessageChars = 1000
	sampleStackChars   = 4096
	maxBriefChars      = 40000
	maxSkillsChars     = 16000
	maxErrorChars      = 500
	backlogViewSize    = 20
	recentOutcomeCount = 10
	maxBodyBytes       = 64 << 10
	maxSkills          = 20
	maxConnectors      = 10

	metricRescan        = time.Hour
	maxSeriesPerSite    = 5000
	maxEvidenceCauses   = 5
	evidenceSearchLimit = 50
	evidenceSamples     = 2
	evidenceSampleChars = 12000
	maxEvidenceFields   = 12
	maxEvidenceAttrs    = 8

	ciNoChecksGrace     = 10 * time.Minute
	ciWaitLimit         = time.Hour
	roundFailedCooldown = 30 * time.Minute
	maxHandled          = 300
	maxRoundItems       = 15
	maxContextComments  = 5
	maxFailedChecks     = 3
	checkLogChars       = 6000
	feedbackBriefChars  = 1200
	contextBriefChars   = 4000
	maxReplies          = 50
	maxReplyChars       = 2000
	maxQuestionChars    = 1000

	defaultLookback = 24 * time.Hour
	minLookback     = time.Hour
	maxLookback     = 168 * time.Hour
	defaultWindow   = 48 * time.Hour
	defaultInterval = 15 * time.Minute
	minInterval     = 5 * time.Minute
	maxInterval     = 24 * time.Hour

	defaultMaxTurns = 200
	minMaxTurns     = 10
	maxMaxTurns     = 500

	defaultRounds        = 5
	minRounds            = 1
	maxRounds            = 10
	defaultSettleMinutes = 15
	minSettleMinutes     = 5
	maxSettleMinutes     = 240
)

var (
	ownerRe      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoNameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	repoGlobRe   = regexp.MustCompile(`^[A-Za-z0-9._*?\[\]-]{1,100}$`)
	branchRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	sessionKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	noteIDRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	groupedRe    = regexp.MustCompile(`(?i)\bby\s*\{\s*[^{}\s][^{}]*\}`)
	tagKeyRe     = regexp.MustCompile(`^[a-z][a-z0-9_./-]{0,199}$`)
	attributeRe  = regexp.MustCompile(`^@?[A-Za-z_][A-Za-z0-9_.-]{0,199}$`)
	channelIDRe  = regexp.MustCompile(`^[CG][A-Z0-9]{8,12}$`)
	modelRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,99}$`)
)

// Repo is the GitHub repository a project works on.
type Repo struct {
	Owner      string   `json:"owner"`
	Name       string   `json:"name,omitempty"`        // empty with Include: the project spans the owner's repositories
	BaseBranch string   `json:"base_branch,omitempty"` // one repository; an organization-scoped task uses its repository's default branch
	Include    []string `json:"include,omitempty"`     // organization scope: path.Match patterns of repository names, such as "*-engine"
}

// Signal is where a project finds the problems it fixes: Datadog log error groups, or Datadog metric series that cross a threshold.
type Signal struct {
	Type           string    `json:"type"`                      // "datadog" | "datadog_metrics"
	Site           string    `json:"site,omitempty"`            // logs: "us" | "eu"
	Sites          []string  `json:"sites,omitempty"`           // "us" and/or "eu"; a log signal on one site keeps it in Site
	Query          string    `json:"query"`                     // logs: search query; metrics: grouped query of what went wrong
	Pattern        string    `json:"pattern,omitempty"`         // optional RE2 over kind, pattern and samples (logs) or the series tags (metrics)
	GroupPattern   string    `json:"group_pattern,omitempty"`   // logs: optional RE2 whose capture groups name the group of each matching event, across services and sites
	Lookback       string    `json:"lookback,omitempty"`        // logs: Go duration for the first scan, default "24h", 1h–168h
	KeptQuery      string    `json:"kept_query,omitempty"`      // metrics: grouped query of what the same series handled correctly
	Window         string    `json:"window,omitempty"`          // metrics: Go duration each series is summed over, default "48h", 1h–168h
	Threshold      float64   `json:"threshold,omitempty"`       // metrics: a series breaches when its sum exceeds it
	ShareThreshold float64   `json:"share_threshold,omitempty"` // metrics: a series breaches when query / (query + kept) exceeds it
	Fill           bool      `json:"fill,omitempty"`            // metrics: spare capacity takes the largest series that breach nothing
	Evidence       *Evidence `json:"evidence,omitempty"`        // metrics: the logs that explain a series
}

// Evidence names the logs fetched for a metric series when it is dispatched.
type Evidence struct {
	Query      string            `json:"query"`              // Datadog log search query, narrowed per series by Attributes
	Attributes map[string]string `json:"attributes"`         // metric tag → log attribute holding the same value
	GroupBy    string            `json:"group_by,omitempty"` // log facet whose most frequent value is the dominant cause
	Fields     []string          `json:"fields,omitempty"`   // log attributes kept in samples; empty keeps them all
}

// Session configures the Claude Code session each task runs in.
type Session struct {
	Model    string `json:"model,omitempty"`     // model label or ID; empty uses the deployment's default
	MaxTurns int    `json:"max_turns,omitempty"` // 10–500, default 200
}

// Review controls how a project follows the pull requests it opens.
type Review struct {
	FollowUp      bool `json:"follow_up"`                // answer failing checks and review comments until the pull request is ready
	Draft         bool `json:"draft"`                    // open as a draft and mark it ready once checks pass and nothing is unanswered
	MaxRounds     int  `json:"max_rounds,omitempty"`     // 1–10, default 5, follow-up sessions per pull request
	SettleMinutes int  `json:"settle_minutes,omitempty"` // 5–240, default 15, quiet time before a pull request counts as ready
}

// Notify names where a project reports what it does.
type Notify struct {
	SlackChannel string `json:"slack_channel,omitempty"` // channel id, such as C0123456789; empty sends nothing
}

// Limits bound how much work a project starts.
type Limits struct {
	MaxOpenPRs            int `json:"max_open_prs"`            // 1–20, default 3 (open PRs + active sessions)
	MaxActiveSessions     int `json:"max_active_sessions"`     // 1–5, default 1
	MaxSessionsPerDay     int `json:"max_sessions_per_day"`    // 1–48, default 6 (UTC day)
	MaxAttempts           int `json:"max_attempts"`            // 1–5, default 2, per error group
	CooldownHours         int `json:"cooldown_hours"`          // 1–720, default 72, after a closed PR or no_fix
	SessionTimeoutMinutes int `json:"session_timeout_minutes"` // 30–600, default 240
	RecurrenceGraceHours  int `json:"recurrence_grace_hours"`  // 0–168, default 24 (deploy lag after merge)
	RecurrenceWindowHours int `json:"recurrence_window_hours"` // 24–720, default 168
}

// Day holds one UTC day's counters.
type Day struct {
	Date     string `json:"date"` // YYYY-MM-DD (UTC)
	Sessions int    `json:"sessions"`
	Opened   int    `json:"opened"`
	Merged   int    `json:"merged"`
	Closed   int    `json:"closed"`
}

// Stats is the quality snapshot stored on the descriptor after every tick.
type Stats struct {
	Sessions             int     `json:"sessions"`
	ActiveSessions       int     `json:"active_sessions"`
	FailedSessions       int     `json:"failed_sessions"`
	NoFix                int     `json:"no_fix"`
	PRsOpened            int     `json:"prs_opened"`
	PRsOpen              int     `json:"prs_open"`
	PRsDone              int     `json:"prs_done"` // open, with passing checks and every review comment answered
	PRsMerged            int     `json:"prs_merged"`
	PRsClosed            int     `json:"prs_closed"`
	MergeRate            float64 `json:"merge_rate"`             // merged / (merged + closed); 0 until one is decided
	MedianMergeHours     float64 `json:"median_merge_hours"`     // PR created → merged, last 50 merges
	MedianSessionMinutes float64 `json:"median_session_minutes"` // dispatched → reported, last 50 sessions
	LinesChanged         int     `json:"lines_changed"`          // additions + deletions over merged PRs
	ErrorGroups          int     `json:"error_groups"`           // fingerprints tracked
	Resolved             int     `json:"resolved"`               // merged fixes whose error stayed quiet through the window
	Recurred             int     `json:"recurred"`               // merged fixes whose error came back after the grace period
	ResolutionRate       float64 `json:"resolution_rate"`        // resolved / (resolved + recurred)
	Backlog              int     `json:"backlog"`                // candidate groups waiting for capacity
	Rounds               int     `json:"rounds"`                 // follow-up sessions on open pull requests
	Daily                []Day   `json:"daily,omitempty"`        // last 30 UTC days, oldest first, zero days included
	UpdatedAt            string  `json:"updated_at,omitempty"`
}

// Project is the stored descriptor and the console API shape.
type Project struct {
	ID                  string      `json:"id"`
	Name                string      `json:"name"`                   // 1–80
	Description         string      `json:"description,omitempty"`  // ≤240
	Goal                string      `json:"goal"`                   // 1–4000
	Instructions        string      `json:"instructions,omitempty"` // ≤8000
	Skills              []string    `json:"skills,omitempty"`       // console skill ids whose instructions go into every work order
	Connectors          []string    `json:"connectors,omitempty"`   // MCP connector ids whose tools sessions may call
	Repo                Repo        `json:"repo"`
	Signal              Signal      `json:"signal"`
	Session             Session     `json:"session"`
	Review              Review      `json:"review"`
	Notify              Notify      `json:"notify"`
	Conventions         Conventions `json:"conventions"`
	Limits              Limits      `json:"limits"`
	Interval            string      `json:"interval"` // Go duration 5m–24h, default "15m"
	Enabled             bool        `json:"enabled"`
	DisabledReason      string      `json:"disabled_reason,omitempty"`
	ConsecutiveFailures int         `json:"consecutive_failures,omitempty"`
	CreatedBy           string      `json:"created_by,omitempty"`
	CreatedAt           string      `json:"created_at"`
	UpdatedAt           string      `json:"updated_at,omitempty"`
	LastTick            string      `json:"last_tick,omitempty"`
	LastError           string      `json:"last_error,omitempty"`
	Stats               Stats       `json:"stats"`
}

func (p *Project) summary() *Project {
	cp := *p
	cp.Stats.Daily = nil
	return &cp
}

func (p *Project) interval() time.Duration {
	return durationOr(p.Interval, defaultInterval, minInterval, maxInterval)
}

func (s Signal) lookback() time.Duration {
	return durationOr(s.Lookback, defaultLookback, minLookback, maxLookback)
}

func (s Signal) window() time.Duration {
	return durationOr(s.Window, defaultWindow, minLookback, maxLookback)
}

func (s Signal) metric() bool { return s.Type == SignalDatadogMetrics }

func (s Signal) sites() []string {
	if s.metric() || len(s.Sites) > 0 {
		return s.Sites
	}
	return []string{s.Site}
}

func (s Signal) key() string {
	parts := []string{s.Site, s.Query, s.Pattern}
	if !s.metric() && (len(s.Sites) > 0 || s.GroupPattern != "") {
		parts = append(parts, strings.Join(s.Sites, ","), s.GroupPattern)
	}
	if s.metric() {
		parts = append(parts, s.Type, strings.Join(s.Sites, ","), s.KeptQuery, s.window().String(),
			strconv.FormatFloat(s.Threshold, 'g', -1, 64), strconv.FormatFloat(s.ShareThreshold, 'g', -1, 64), strconv.FormatBool(s.Fill))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func (s Session) maxTurns() int {
	if s.MaxTurns < minMaxTurns || s.MaxTurns > maxMaxTurns {
		return defaultMaxTurns
	}
	return s.MaxTurns
}

func durationOr(v string, def, lo, hi time.Duration) time.Duration {
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		return def
	}
	return d
}

type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

func invalidf(format string, args ...any) error {
	return &inputError{msg: fmt.Sprintf(format, args...)}
}

func isInputError(err error) bool {
	var ie *inputError
	return errors.As(err, &ie)
}

func checkText(field, v string, lo, hi int) error {
	n := utf8.RuneCountInString(v)
	switch {
	case n < lo && lo == 1:
		return invalidf("%s is required", field)
	case n < lo:
		return invalidf("%s must be at least %d characters", field, lo)
	case n > hi:
		return invalidf("%s must be at most %d characters", field, hi)
	}
	return nil
}

func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

func checkDuration(field, v string, lo, hi time.Duration) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return invalidf("%s %q is not a duration such as 15m or 24h", field, v)
	}
	if d < lo || d > hi {
		return invalidf("%s must be between %s and %s", field, humanDuration(lo), humanDuration(hi))
	}
	return nil
}

func validBranchName(b string) bool {
	return branchRe.MatchString(b) &&
		!strings.Contains(b, "..") && !strings.Contains(b, "//") &&
		!strings.HasPrefix(b, "/") && !strings.HasSuffix(b, "/") &&
		!strings.HasSuffix(b, ".") && !strings.HasSuffix(b, ".lock")
}

func (r *Repo) normalize() error {
	r.Owner = strings.TrimSpace(r.Owner)
	r.Name = strings.TrimSpace(r.Name)
	r.BaseBranch = strings.TrimSpace(r.BaseBranch)
	if !ownerRe.MatchString(r.Owner) {
		return invalidf("repo.owner %q is not a GitHub user or organization name", r.Owner)
	}
	var include []string
	for _, p := range r.Include {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && !slices.Contains(include, p) {
			include = append(include, p)
		}
	}
	r.Include = include
	if r.Name == "" && len(include) > 0 {
		if r.BaseBranch != "" {
			return invalidf("repo.base_branch names one repository's branch; an organization-scoped project uses each repository's default branch")
		}
		if len(include) > maxRepoPatterns {
			return invalidf("repo.include has %d patterns; at most %d", len(include), maxRepoPatterns)
		}
		for _, p := range include {
			if _, err := path.Match(p, ""); err != nil || !repoGlobRe.MatchString(p) {
				return invalidf("repo.include %q is not a repository name pattern", p)
			}
		}
		return nil
	}
	if len(include) > 0 {
		return invalidf("repo.include applies only to an organization-scoped project; leave repo.name empty to use it")
	}
	if !repoNameRe.MatchString(r.Name) || r.Name == "." || r.Name == ".." {
		return invalidf("repo.name %q is not a GitHub repository name", r.Name)
	}
	if r.BaseBranch != "" && !validBranchName(r.BaseBranch) {
		return invalidf("repo.base_branch %q is not a valid branch name", r.BaseBranch)
	}
	return nil
}

func (r Repo) org() bool { return r.Name == "" }

func (r Repo) String() string {
	if r.org() {
		return r.Owner + "/{" + strings.Join(r.Include, ",") + "}"
	}
	return r.Owner + "/" + r.Name
}

func (r Repo) includes(name string) bool {
	name = strings.ToLower(name)
	return slices.ContainsFunc(r.Include, func(p string) bool {
		ok, _ := path.Match(p, name)
		return ok
	})
}

func (p *Project) repoFor(t *Task) Repo {
	if t != nil && t.Repo != "" {
		return Repo{Owner: p.Repo.Owner, Name: t.Repo, BaseBranch: t.BaseBranch}
	}
	if !p.Repo.org() {
		return p.Repo
	}
	r := Repo{Owner: p.Repo.Owner}
	if t != nil && t.PR != nil {
		if _, name, _, err := github.ParsePRURL(t.PR.URL); err == nil {
			r.Name = name
		}
	}
	return r
}

func (s *Signal) normalize() error {
	s.Type = strings.ToLower(strings.TrimSpace(s.Type))
	if s.Type == "" {
		s.Type = SignalDatadog
	}
	s.Query = strings.TrimSpace(s.Query)
	if strings.TrimSpace(s.Pattern) == "" {
		s.Pattern = ""
	}
	if err := checkText("signal.pattern", s.Pattern, 0, 500); err != nil {
		return err
	}
	if s.Pattern != "" {
		if _, err := regexp.Compile(s.Pattern); err != nil {
			return invalidf("signal.pattern is not a valid regular expression: %v", err)
		}
	}
	switch s.Type {
	case SignalDatadog:
		return s.normalizeLogs()
	case SignalDatadogMetrics:
		return s.normalizeMetrics()
	}
	return invalidf("signal.type must be %q or %q", SignalDatadog, SignalDatadogMetrics)
}

func normalizeSite(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v != "us" && v != "eu" {
		return "", invalidf("signal sites must be us or eu")
	}
	return v, nil
}

func (s *Signal) normalizeLogs() error {
	picked, err := pickSites(s.Sites, s.Site)
	if err != nil {
		return err
	}
	s.Site, s.Sites = "", picked
	if len(picked) == 1 {
		s.Site, s.Sites = picked[0], nil
	}
	if err := checkText("signal.query", s.Query, 1, 1000); err != nil {
		return err
	}
	s.GroupPattern = strings.TrimSpace(s.GroupPattern)
	if err := checkText("signal.group_pattern", s.GroupPattern, 0, 500); err != nil {
		return err
	}
	if s.GroupPattern != "" {
		if _, err := regexp.Compile(s.GroupPattern); err != nil {
			return invalidf("signal.group_pattern is not a valid RE2 expression: %v", err)
		}
	}
	s.Lookback = strings.TrimSpace(s.Lookback)
	if s.Lookback == "" {
		s.Lookback = humanDuration(defaultLookback)
	}
	s.KeptQuery, s.Window, s.Threshold, s.ShareThreshold, s.Fill, s.Evidence = "", "", 0, 0, false, nil
	return checkDuration("signal.lookback", s.Lookback, minLookback, maxLookback)
}

func pickSites(sites []string, site string) ([]string, error) {
	if len(sites) == 0 && strings.TrimSpace(site) != "" {
		sites = []string{site}
	}
	var picked []string
	for _, v := range sites {
		site, err := normalizeSite(v)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(picked, site) {
			picked = append(picked, site)
		}
	}
	if len(picked) == 0 {
		return nil, invalidf("signal.sites needs us, eu or both")
	}
	slices.SortFunc(picked, func(a, b string) int { return strings.Compare(b, a) })
	return picked, nil
}

func (s *Signal) normalizeMetrics() error {
	picked, err := pickSites(s.Sites, s.Site)
	if err != nil {
		return err
	}
	s.Sites, s.Site, s.Lookback, s.GroupPattern = picked, "", "", ""
	s.KeptQuery = strings.TrimSpace(s.KeptQuery)
	for _, q := range []struct{ field, v string }{{"signal.query", s.Query}, {"signal.kept_query", s.KeptQuery}} {
		lo := 1
		if q.field == "signal.kept_query" {
			lo = 0
		}
		if err := checkText(q.field, q.v, lo, 1000); err != nil {
			return err
		}
		if q.v != "" && !groupedRe.MatchString(q.v) {
			return invalidf("%s must group its series, such as sum:errors{*} by {service}", q.field)
		}
	}
	s.Window = strings.TrimSpace(s.Window)
	if s.Window == "" {
		s.Window = humanDuration(defaultWindow)
	}
	if err := checkDuration("signal.window", s.Window, minLookback, maxLookback); err != nil {
		return err
	}
	switch {
	case math.IsNaN(s.Threshold) || math.IsInf(s.Threshold, 0) || s.Threshold < 0:
		return invalidf("signal.threshold must be zero or more")
	case math.IsNaN(s.ShareThreshold) || s.ShareThreshold < 0 || s.ShareThreshold >= 1:
		return invalidf("signal.share_threshold must be at least 0 and below 1")
	case s.Threshold == 0 && s.ShareThreshold == 0:
		return invalidf("set signal.threshold, signal.share_threshold or both")
	case s.ShareThreshold > 0 && s.KeptQuery == "":
		return invalidf("signal.share_threshold needs signal.kept_query")
	}
	if s.Evidence == nil {
		return nil
	}
	return s.Evidence.normalize()
}

func normalizeAttribute(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if !attributeRe.MatchString(v) {
		return "", invalidf("%s %q is not a log attribute such as @error.kind", field, v)
	}
	return "@" + strings.TrimPrefix(v, "@"), nil
}

func (e *Evidence) normalize() error {
	e.Query = strings.TrimSpace(e.Query)
	if err := checkText("signal.evidence.query", e.Query, 1, 1000); err != nil {
		return err
	}
	if len(e.Attributes) == 0 || len(e.Attributes) > maxEvidenceAttrs {
		return invalidf("signal.evidence.attributes must map 1 to %d metric tags to log attributes", maxEvidenceAttrs)
	}
	attrs := make(map[string]string, len(e.Attributes))
	for tag, attr := range e.Attributes {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if !tagKeyRe.MatchString(tag) {
			return invalidf("signal.evidence.attributes: %q is not a metric tag name", tag)
		}
		norm, err := normalizeAttribute("signal.evidence.attributes."+tag, attr)
		if err != nil {
			return err
		}
		attrs[tag] = norm
	}
	e.Attributes = attrs
	if strings.TrimSpace(e.GroupBy) == "" {
		e.GroupBy = ""
	} else {
		norm, err := normalizeAttribute("signal.evidence.group_by", e.GroupBy)
		if err != nil {
			return err
		}
		e.GroupBy = norm
	}
	if len(e.Fields) > maxEvidenceFields {
		return invalidf("signal.evidence.fields holds at most %d attributes", maxEvidenceFields)
	}
	var fields []string
	for _, f := range e.Fields {
		norm, err := normalizeAttribute("signal.evidence.fields", f)
		if err != nil {
			return err
		}
		if !slices.Contains(fields, norm) {
			fields = append(fields, norm)
		}
	}
	e.Fields = fields
	return nil
}

func (s *Session) normalize() error {
	s.Model = strings.TrimSpace(s.Model)
	if s.Model != "" && (!modelRe.MatchString(s.Model) || !llm.KnownModel(s.Model)) {
		return invalidf("session.model %q is not a model label such as sonnet or an Anthropic model ID such as claude-sonnet-5-5", s.Model)
	}
	switch {
	case s.MaxTurns == 0:
		s.MaxTurns = defaultMaxTurns
	case s.MaxTurns < minMaxTurns || s.MaxTurns > maxMaxTurns:
		return invalidf("session.max_turns must be between %d and %d", minMaxTurns, maxMaxTurns)
	}
	return nil
}

func (n *Notify) normalize() error {
	n.SlackChannel = strings.TrimSpace(n.SlackChannel)
	if n.SlackChannel != "" && !channelIDRe.MatchString(n.SlackChannel) {
		return invalidf("notify.slack_channel must be a Slack channel id such as C0123456789")
	}
	return nil
}

func (r *Review) normalize() error {
	if r.MaxRounds != 0 && (r.MaxRounds < minRounds || r.MaxRounds > maxRounds) {
		return invalidf("review.max_rounds must be between %d and %d", minRounds, maxRounds)
	}
	if r.SettleMinutes != 0 && (r.SettleMinutes < minSettleMinutes || r.SettleMinutes > maxSettleMinutes) {
		return invalidf("review.settle_minutes must be between %d and %d", minSettleMinutes, maxSettleMinutes)
	}
	*r = r.effective()
	return nil
}

func (r Review) effective() Review {
	if r.MaxRounds < minRounds || r.MaxRounds > maxRounds {
		r.MaxRounds = defaultRounds
	}
	if r.SettleMinutes < minSettleMinutes || r.SettleMinutes > maxSettleMinutes {
		r.SettleMinutes = defaultSettleMinutes
	}
	return r
}

type limitBound struct {
	name         string
	field        func(*Limits) *int
	lo, hi, dflt int
}

var limitBounds = []limitBound{
	{"max_open_prs", func(l *Limits) *int { return &l.MaxOpenPRs }, 1, 20, 3},
	{"max_active_sessions", func(l *Limits) *int { return &l.MaxActiveSessions }, 1, 5, 1},
	{"max_sessions_per_day", func(l *Limits) *int { return &l.MaxSessionsPerDay }, 1, 48, 6},
	{"max_attempts", func(l *Limits) *int { return &l.MaxAttempts }, 1, 5, 2},
	{"cooldown_hours", func(l *Limits) *int { return &l.CooldownHours }, 1, 720, 72},
	{"session_timeout_minutes", func(l *Limits) *int { return &l.SessionTimeoutMinutes }, 30, 600, 240},
	{"recurrence_grace_hours", func(l *Limits) *int { return &l.RecurrenceGraceHours }, 0, 168, 24},
	{"recurrence_window_hours", func(l *Limits) *int { return &l.RecurrenceWindowHours }, 24, 720, 168},
}

func (l *Limits) normalize() error {
	for _, b := range limitBounds {
		v := b.field(l)
		if *v == 0 {
			*v = b.dflt
			continue
		}
		if *v < b.lo || *v > b.hi {
			return invalidf("limits.%s must be between %d and %d", b.name, b.lo, b.hi)
		}
	}
	if l.RecurrenceGraceHours >= l.RecurrenceWindowHours {
		return invalidf("limits.recurrence_grace_hours must be shorter than limits.recurrence_window_hours")
	}
	return nil
}

// effective replaces missing or out-of-range limits with defaults, for descriptors edited outside the API.
func (l Limits) effective() Limits {
	for _, b := range limitBounds {
		if v := b.field(&l); *v < b.lo || *v > b.hi || (*v == 0 && b.lo == 0) {
			*v = b.dflt
		}
	}
	if l.RecurrenceGraceHours >= l.RecurrenceWindowHours {
		l.RecurrenceGraceHours = 0
	}
	return l
}

func normalizeInterval(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		v = humanDuration(defaultInterval)
	}
	if err := checkDuration("interval", v, minInterval, maxInterval); err != nil {
		return "", err
	}
	return v, nil
}

func normalizeIDs(field, noun string, in []string, max int) ([]string, error) {
	var out []string
	for _, id := range in {
		id = strings.TrimSpace(id)
		if !store.IDRe.MatchString(id) {
			return nil, invalidf("%s: %q is not %s id", field, id, noun)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) > max {
		return nil, invalidf("a project uses at most %d %s", max, field)
	}
	return out, nil
}

func (p *Project) normalize() error {
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)
	p.Goal = strings.TrimSpace(p.Goal)
	p.Instructions = strings.TrimSpace(p.Instructions)
	checks := []error{
		checkText("name", p.Name, 1, 80),
		checkText("description", p.Description, 0, 240),
		checkText("goal", p.Goal, 1, 4000),
		checkText("instructions", p.Instructions, 0, 8000),
		p.Repo.normalize(),
		p.Signal.normalize(),
		p.Session.normalize(),
		p.Review.normalize(),
		p.Notify.normalize(),
		p.Conventions.normalize(),
		p.Limits.normalize(),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	skills, err := normalizeIDs("skills", "a skill", p.Skills, maxSkills)
	if err != nil {
		return err
	}
	p.Skills = skills
	connectors, err := normalizeIDs("connectors", "an MCP connector", p.Connectors, maxConnectors)
	if err != nil {
		return err
	}
	p.Connectors = connectors
	iv, err := normalizeInterval(p.Interval)
	if err != nil {
		return err
	}
	p.Interval = iv
	return nil
}
