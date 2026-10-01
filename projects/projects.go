// Package projects gives agents standing goals on a GitHub repository, pursued by Claude Code sessions on self-hosted runners.
package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Prefix        = "projects/"
	StatePrefix   = "project-state/"
	SessionPrefix = "project-sessions/"
	QueueTopic    = "project-tick"

	SignalDatadog = "datadog"

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
	maxStagger         = 60 * time.Second
	dispatchLostAfter  = 15 * time.Minute
	runningGrace       = 30 * time.Minute
	failedCooldown     = time.Hour
	unreadablePRGrace  = 72 * time.Hour
	openingStaleAfter  = 2 * time.Minute
	minRetryAfter      = time.Minute
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
	sampleMessageChars = 1000
	sampleStackChars   = 4096
	maxBriefChars      = 40000
	maxSkillsChars     = 16000
	maxFireText        = 300
	maxErrorChars      = 500
	backlogViewSize    = 20
	recentOutcomeCount = 10
	maxBodyBytes       = 64 << 10

	defaultLookback = 24 * time.Hour
	minLookback     = time.Hour
	maxLookback     = 168 * time.Hour
	defaultInterval = 15 * time.Minute
	minInterval     = 5 * time.Minute
	maxInterval     = 24 * time.Hour
)

var (
	ownerRe      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoNameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	branchRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	routineIDRe  = regexp.MustCompile(`^trig_[A-Za-z0-9]{8,64}$`)
	tokenNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	sessionKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	noteIDRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// Repo is the GitHub repository a project works on.
type Repo struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	BaseBranch string `json:"base_branch"`
}

// Signal is where a project finds the errors it fixes.
type Signal struct {
	Type     string `json:"type"`               // "datadog"
	Site     string `json:"site"`               // "us" | "eu"
	Query    string `json:"query"`              // Datadog log search query
	Pattern  string `json:"pattern,omitempty"`  // optional RE2, matched against kind, pattern and sample messages
	Lookback string `json:"lookback,omitempty"` // Go duration for the first scan, default "24h", 1h–168h
}

// Dispatch names the routine a project fires and the trigger token it fires with.
type Dispatch struct {
	RoutineID       string `json:"routine_id"`                 // trig_…
	Token           string `json:"token"`                      // token name → env PROJECT_TRIGGER_<NAME>
	TokenConfigured bool   `json:"token_configured,omitempty"` // transient, filled on read
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
	Daily                []Day   `json:"daily,omitempty"`        // last 30 UTC days, oldest first, zero days included
	UpdatedAt            string  `json:"updated_at,omitempty"`
}

// Project is the stored descriptor and the console API shape.
type Project struct {
	ID                  string   `json:"id"`
	Agent               string   `json:"agent"`
	Name                string   `json:"name"`                   // 1–80
	Description         string   `json:"description,omitempty"`  // ≤240
	Goal                string   `json:"goal"`                   // 1–4000
	Instructions        string   `json:"instructions,omitempty"` // ≤8000
	Repo                Repo     `json:"repo"`
	Signal              Signal   `json:"signal"`
	Dispatch            Dispatch `json:"dispatch"`
	Limits              Limits   `json:"limits"`
	Interval            string   `json:"interval"` // Go duration 5m–24h, default "15m"
	Enabled             bool     `json:"enabled"`
	DisabledReason      string   `json:"disabled_reason,omitempty"`
	ConsecutiveFailures int      `json:"consecutive_failures,omitempty"`
	CreatedBy           string   `json:"created_by,omitempty"`
	CreatedAt           string   `json:"created_at"`
	UpdatedAt           string   `json:"updated_at,omitempty"`
	LastTick            string   `json:"last_tick,omitempty"`
	LastError           string   `json:"last_error,omitempty"`
	NextDispatchAfter   string   `json:"next_dispatch_after,omitempty"` // set from a 429 Retry-After
	Stats               Stats    `json:"stats"`
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

func (s Signal) key() string {
	sum := sha256.Sum256([]byte(s.Site + "\x00" + s.Query + "\x00" + s.Pattern))
	return hex.EncodeToString(sum[:8])
}

func durationOr(v string, def, lo, hi time.Duration) time.Duration {
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		return def
	}
	return d
}

// TokenEnv names the environment variable that holds the routine trigger token called name.
func TokenEnv(name string) string {
	return "PROJECT_TRIGGER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func routineToken(name string) string {
	if !tokenNameRe.MatchString(name) {
		return ""
	}
	return strings.TrimSpace(os.Getenv(TokenEnv(name)))
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
	if !repoNameRe.MatchString(r.Name) || r.Name == "." || r.Name == ".." {
		return invalidf("repo.name %q is not a GitHub repository name", r.Name)
	}
	if r.BaseBranch != "" && !validBranchName(r.BaseBranch) {
		return invalidf("repo.base_branch %q is not a valid branch name", r.BaseBranch)
	}
	return nil
}

func (s *Signal) normalize() error {
	s.Type = strings.ToLower(strings.TrimSpace(s.Type))
	if s.Type == "" {
		s.Type = SignalDatadog
	}
	if s.Type != SignalDatadog {
		return invalidf("signal.type must be %q", SignalDatadog)
	}
	s.Site = strings.ToLower(strings.TrimSpace(s.Site))
	if s.Site != "us" && s.Site != "eu" {
		return invalidf("signal.site must be us or eu")
	}
	s.Query = strings.TrimSpace(s.Query)
	if err := checkText("signal.query", s.Query, 1, 1000); err != nil {
		return err
	}
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
	s.Lookback = strings.TrimSpace(s.Lookback)
	if s.Lookback == "" {
		s.Lookback = humanDuration(defaultLookback)
	}
	return checkDuration("signal.lookback", s.Lookback, minLookback, maxLookback)
}

func (d *Dispatch) normalize() error {
	d.RoutineID = strings.TrimSpace(d.RoutineID)
	d.Token = strings.TrimSpace(d.Token)
	d.TokenConfigured = false
	if !routineIDRe.MatchString(d.RoutineID) {
		return invalidf("dispatch.routine_id must look like trig_0123456789abcdef")
	}
	if !tokenNameRe.MatchString(d.Token) {
		return invalidf("dispatch.token must be a token name of up to 32 lowercase letters, digits and dashes")
	}
	return nil
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
		p.Dispatch.normalize(),
		p.Limits.normalize(),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	iv, err := normalizeInterval(p.Interval)
	if err != nil {
		return err
	}
	p.Interval = iv
	return nil
}
