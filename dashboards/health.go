package dashboards

import (
	"fmt"
	"strings"
	"time"

	"github.com/justmike1/arbetern/atlassian"
	"github.com/justmike1/arbetern/chorus"
	"github.com/justmike1/arbetern/datadog"
	"github.com/justmike1/arbetern/salesforce"
)

// Signal weights — sum to 100. These come from the product spec and should
// remain in sync with the breakdown shown on the dashboard.
const (
	weightTicketHealth = 25
	weightInfra        = 20
	weightEngagement   = 20
	weightComms        = 15
	weightCommercial   = 20
)

// AccountSnapshot is the raw data pulled from every integration for a single
// account. ComputeHealth is a pure function over this snapshot.
type AccountSnapshot struct {
	Account     salesforce.Account
	Opps        []salesforce.Opportunity
	JiraIssues  []atlassian.IssueSummary
	Engagements []chorus.Engagement
	Monitors    []datadog.Monitor
	Now         time.Time
}

// ComputeHealth turns an AccountSnapshot into a scored AccountSummary.
// The score is always clamped to [0, 100] and mapped onto a colour band per
// the spec: 80+ green, 60-79 yellow, 40-59 orange, <40 red.
func ComputeHealth(s AccountSnapshot) *AccountSummary {
	if s.Now.IsZero() {
		s.Now = time.Now().UTC()
	}

	signals := []Signal{
		ticketHealthSignal(s),
		infraSignal(s),
		engagementSignal(s),
		commsSignal(s),
		commercialSignal(s),
	}

	total := 0
	for _, sig := range signals {
		total += sig.Score
	}
	total = clamp(total, 0, 100)

	risks, actions := deriveRisksAndActions(signals, s)

	return &AccountSummary{
		AccountName: s.Account.Name,
		AccountID:   s.Account.ID,
		Score:       total,
		Band:        bandFor(total),
		Risks:       risks,
		Actions:     actions,
		Signals:     signals,
		GeneratedAt: s.Now.Format(time.RFC3339),
	}
}

func bandFor(score int) string {
	switch {
	case score >= 80:
		return "green"
	case score >= 60:
		return "yellow"
	case score >= 40:
		return "orange"
	default:
		return "red"
	}
}

// ── Per-signal evaluators ──────────────────────────────────────────────────

func ticketHealthSignal(s AccountSnapshot) Signal {
	sig := Signal{Name: "Ticket Health", Weight: weightTicketHealth, Score: weightTicketHealth}
	var p0p1, stale, unassigned int
	for _, i := range s.JiraIssues {
		if isClosedStatus(i.Status) {
			continue
		}
		if isP0P1(i.Priority) {
			p0p1++
		}
		if isStale(i.Updated, s.Now, 14*24*time.Hour) {
			stale++
		}
		if strings.TrimSpace(i.Assignee) == "" {
			unassigned++
		}
	}
	// Penalties: -15 per P0/P1, -10 per stale, -5 per unassigned.
	penalty := 15*p0p1 + 10*stale + 5*unassigned
	sig.Score = clamp(sig.Weight-penalty, 0, sig.Weight)
	if p0p1 > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d open P0/P1 ticket%s", p0p1, plural(p0p1)))
	}
	if stale > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d stale (>14d) ticket%s", stale, plural(stale)))
	}
	if unassigned > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d unassigned ticket%s", unassigned, plural(unassigned)))
	}
	if len(sig.Reasons) == 0 {
		sig.Reasons = append(sig.Reasons, "no open tickets detected for this account")
	}
	return sig
}

func infraSignal(s AccountSnapshot) Signal {
	sig := Signal{Name: "Infra Stability", Weight: weightInfra, Score: weightInfra}
	var crit, warn int
	for _, m := range s.Monitors {
		switch strings.ToLower(m.OverallState) {
		case "alert":
			crit++
		case "warn", "warning":
			warn++
		}
	}
	// -20 per critical, -10 per warning. No error-spike signal without logs data.
	penalty := 20*crit + 10*warn
	sig.Score = clamp(sig.Weight-penalty, 0, sig.Weight)
	if crit > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d monitor%s in ALERT", crit, plural(crit)))
	}
	if warn > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d monitor%s in WARN", warn, plural(warn)))
	}
	if len(sig.Reasons) == 0 {
		sig.Reasons = append(sig.Reasons, "no alerting monitors tagged for this account")
	}
	return sig
}

func engagementSignal(s AccountSnapshot) Signal {
	sig := Signal{Name: "Engagement (Chorus)", Weight: weightEngagement, Score: weightEngagement}
	if len(s.Engagements) == 0 {
		// Treat complete absence of data as unknown, not a failure — keep full
		// credit but flag it as a reason.
		sig.Reasons = append(sig.Reasons, "no Chorus calls linked to this account")
		return sig
	}
	var competitor, overdue, mostRecent int
	mostRecent = -1
	for _, e := range s.Engagements {
		if d := daysSinceUnix(e.DateTime, s.Now); d >= 0 && (mostRecent < 0 || d < mostRecent) {
			mostRecent = d
		}
		for _, t := range e.TrackerMatches {
			if strings.Contains(strings.ToLower(t.Name), "competitor") {
				competitor++
				break
			}
		}
		// Overdue action items: we don't have due dates, so count all open items
		// on calls older than 14 days as "overdue" (a pragmatic proxy).
		if len(e.ActionItems) > 0 && daysSinceUnix(e.DateTime, s.Now) > 14 {
			overdue += len(e.ActionItems)
		}
	}
	penalty := 10*competitor + 5*overdue
	if mostRecent > 30 || mostRecent < 0 {
		penalty += 20
		sig.Reasons = append(sig.Reasons, "no calls in the last 30 days")
	}
	sig.Score = clamp(sig.Weight-penalty, 0, sig.Weight)
	if competitor > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d competitor mention%s on recent calls", competitor, plural(competitor)))
	}
	if overdue > 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("%d overdue action item%s (call >14d old)", overdue, plural(overdue)))
	}
	if len(sig.Reasons) == 0 {
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("last call %d day%s ago, no flags", mostRecent, plural(mostRecent)))
	}
	return sig
}

// commsSignal is deliberately a stub in v1 — sentiment/email/Slack-activity
// ingestion is out of scope. Full credit is awarded so this bucket doesn't
// drag scores down without data.
func commsSignal(s AccountSnapshot) Signal {
	return Signal{
		Name:    "Comms (Slack/Email)",
		Weight:  weightComms,
		Score:   weightComms,
		Reasons: []string{"not yet instrumented — full credit"},
	}
}

func commercialSignal(s AccountSnapshot) Signal {
	sig := Signal{Name: "License & Commercial", Weight: weightCommercial, Score: weightCommercial}

	// Find the best renewal / new-business opp to judge against.
	hasOpenRenewal := false
	var soonestRenewalDays = -1
	for _, o := range s.Opps {
		if o.IsClosed {
			continue
		}
		if strings.Contains(strings.ToLower(o.Type), "renewal") ||
			strings.Contains(strings.ToLower(o.Name), "renewal") {
			hasOpenRenewal = true
		}
		if d := daysUntilISO(o.CloseDate, s.Now); d >= 0 && (soonestRenewalDays < 0 || d < soonestRenewalDays) {
			soonestRenewalDays = d
		}
	}

	penalty := 0
	if soonestRenewalDays >= 0 && soonestRenewalDays < 60 && !hasOpenRenewal {
		penalty += 30
		sig.Reasons = append(sig.Reasons, fmt.Sprintf("renewal in %dd with no open renewal opp", soonestRenewalDays))
	}
	// usage >90% and no QBR 6mo require data we don't currently collect.
	sig.Score = clamp(sig.Weight-penalty, 0, sig.Weight)
	if len(sig.Reasons) == 0 {
		if hasOpenRenewal {
			sig.Reasons = append(sig.Reasons, "open renewal opportunity on file")
		} else {
			sig.Reasons = append(sig.Reasons, "no imminent renewal risk detected")
		}
	}
	return sig
}

// ── Derivation helpers ─────────────────────────────────────────────────────

// deriveRisksAndActions picks the top 3 reasons across signals weighted by the
// penalty each bucket took, plus a short action list based on Jira + Chorus.
func deriveRisksAndActions(signals []Signal, s AccountSnapshot) (risks, actions []string) {
	// Risks: reasons from signals that lost the most points.
	type rr struct {
		reason  string
		penalty int
	}
	var scored []rr
	for _, sig := range signals {
		lost := sig.Weight - sig.Score
		if lost <= 0 {
			continue
		}
		for _, reason := range sig.Reasons {
			scored = append(scored, rr{reason: reason, penalty: lost})
		}
	}
	// Simple sort: larger penalty first (bubble is fine, n is tiny).
	for i := 0; i < len(scored); i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].penalty > scored[i].penalty {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}
	for i := 0; i < len(scored) && i < 3; i++ {
		risks = append(risks, scored[i].reason)
	}

	// Actions: up to 3 concrete items derived from open tickets and overdue Chorus AIs.
	for _, i := range s.JiraIssues {
		if len(actions) >= 3 {
			break
		}
		if isClosedStatus(i.Status) {
			continue
		}
		if !isP0P1(i.Priority) && !isStale(i.Updated, s.Now, 14*24*time.Hour) {
			continue
		}
		actions = append(actions, fmt.Sprintf("Triage %s — %s", i.Key, truncate(i.Summary, 80)))
	}
	for _, e := range s.Engagements {
		if len(actions) >= 3 {
			break
		}
		if daysSinceUnix(e.DateTime, s.Now) > 14 {
			for _, ai := range e.ActionItems {
				if len(actions) >= 3 {
					break
				}
				actions = append(actions, fmt.Sprintf("Follow up: %s", truncate(ai, 80)))
			}
		}
	}
	return risks, actions
}

// ── Generic helpers ────────────────────────────────────────────────────────

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func isP0P1(priority string) bool {
	p := strings.ToLower(priority)
	switch p {
	case "highest", "critical", "p0", "p1", "high":
		return true
	}
	return false
}

func isClosedStatus(status string) bool {
	s := strings.ToLower(status)
	return s == "done" || s == "closed" || s == "resolved" || s == "cancelled"
}

// isStale reports whether the given RFC3339-ish timestamp is older than 'age'.
func isStale(ts string, now time.Time, age time.Duration) bool {
	t, err := parseFlexTime(ts)
	if err != nil {
		return false
	}
	return now.Sub(t) > age
}

// daysSinceUnix is the float-seconds variant used by Chorus engagement
// timestamps (which arrive as unix seconds).
func daysSinceUnix(sec float64, now time.Time) int {
	if sec <= 0 {
		return -1
	}
	t := time.Unix(int64(sec), 0).UTC()
	return int(now.Sub(t).Hours() / 24)
}

// daysUntilISO returns days until an ISO date (YYYY-MM-DD). -1 on parse failure
// or past dates are returned as 0.
func daysUntilISO(date string, now time.Time) int {
	if date == "" {
		return -1
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		if t, err = parseFlexTime(date); err != nil {
			return -1
		}
	}
	d := int(t.Sub(now).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// parseFlexTime accepts RFC3339 and common Jira-style timestamps.
func parseFlexTime(ts string) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		"2006-01-02",
	}
	var lastErr error
	for _, l := range layouts {
		if t, err := time.Parse(l, ts); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
