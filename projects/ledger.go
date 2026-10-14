package projects

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	reasonShare     = "share"
	reasonThreshold = "threshold"
	reasonFill      = "fill"

	recurrenceResolved = "resolved"
	recurrenceRecurred = "recurred"

	prStateOpen   = "open"
	prStateMerged = "merged"
	prStateClosed = "closed"

	noteAdmin   = "admin"
	noteSession = "session"
	noteReview  = "review"
)

// Sample is one redacted log event of an error group or of a metric series' evidence.
type Sample struct {
	At      string `json:"at"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Fields  string `json:"fields,omitempty"` // evidence: the selected log attributes as JSON
}

// Cause is one value of the evidence group-by facet and how many log lines carried it.
type Cause struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Group is an error group, or a metric series, as the project has seen it so far.
type Group struct {
	Fingerprint   string   `json:"fingerprint"`
	Service       string   `json:"service"`
	Kind          string   `json:"kind"`
	Pattern       string   `json:"pattern"`
	Frame         string   `json:"frame,omitempty"`
	Count         int      `json:"count"` // occurrences seen by this project so far; for a series, its sum rounded
	FirstSeen     string   `json:"first_seen"`
	LastSeen      string   `json:"last_seen"`
	Samples       []Sample `json:"samples,omitempty"` // redacted and truncated (message 1000, stack tail 4096)
	Site          string   `json:"site,omitempty"`    // metric series from here on
	Tags          []string `json:"tags,omitempty"`
	Value         float64  `json:"value,omitempty"` // sum over the window
	Kept          float64  `json:"kept,omitempty"`
	Share         float64  `json:"share,omitempty"`  // value / (value + kept)
	Reason        string   `json:"reason,omitempty"` // share | threshold | fill
	Cause         string   `json:"cause,omitempty"`
	Causes        []Cause  `json:"causes,omitempty"`
	EvidenceQuery string   `json:"evidence_query,omitempty"`
}

// PR is the pull request a task opened.
type PR struct {
	Number          int    `json:"number"`
	URL             string `json:"url"`
	Title           string `json:"title"`
	State           string `json:"state"` // open | merged | closed
	CreatedAt       string `json:"created_at"`
	MergedAt        string `json:"merged_at,omitempty"`
	ClosedAt        string `json:"closed_at,omitempty"`
	Additions       int    `json:"additions,omitempty"`
	Deletions       int    `json:"deletions,omitempty"`
	UnreadableSince string `json:"unreadable_since,omitempty"` // first 404 from GitHub
	BodySHA         string `json:"body_sha,omitempty"`         // of the description arbetern last wrote
}

// Outcome is what a session reported through report_outcome.
type Outcome struct {
	Status     string `json:"status"`            // fixed | not_reproducible | cannot_fix | already_fixed | answered
	Summary    string `json:"summary"`           // ≤4000
	Testing    string `json:"testing,omitempty"` // ≤2000
	Title      string `json:"title,omitempty"`   // ≤120
	Branch     string `json:"branch,omitempty"`
	Question   string `json:"question,omitempty"` // ≤1000, a decision reviewers are asked to make
	Headline   string `json:"headline,omitempty"` // one line for the project's channel
	ReportedAt string `json:"reported_at"`
}

// Task is one dispatched attempt at one error group.
type Task struct {
	ID             string       `json:"id"`
	Fingerprint    string       `json:"fingerprint"`
	Group          Group        `json:"group"`
	Status         string       `json:"status"`
	Attempt        int          `json:"attempt"`
	SessionID      string       `json:"session_id,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	Ticket         string       `json:"ticket,omitempty"` // issue key filed with the pull request
	TicketURL      string       `json:"ticket_url,omitempty"`
	PR             *PR          `json:"pr,omitempty"`
	Outcome        *Outcome     `json:"outcome,omitempty"`
	ExitReason     string       `json:"exit_reason,omitempty"` // how the session process ended
	Recurrence     string       `json:"recurrence,omitempty"`  // "" | resolved | recurred (merged tasks only)
	Learnings      int          `json:"learnings,omitempty"`
	OpenAttempts   int          `json:"open_attempts,omitempty"`
	Opening        string       `json:"opening,omitempty"`        // guard while a PR is being opened; stale after 2m
	Bound          bool         `json:"bound,omitempty"`          // a session binding may still exist
	Review         *ReviewState `json:"review,omitempty"`         // follow-through of the pull request
	ClaudeSession  string       `json:"claude_session,omitempty"` // the Claude Code session id every round resumes
	Transcript     string       `json:"transcript,omitempty"`     // when the session transcript was last stored
	TranscriptSize int          `json:"transcript_size,omitempty"`
	Turns          int          `json:"turns,omitempty"`
	Error          string       `json:"error,omitempty"`
	CreatedAt      string       `json:"created_at"`
	DispatchedAt   string       `json:"dispatched_at,omitempty"`
	UpdatedAt      string       `json:"updated_at"`
	FinishedAt     string       `json:"finished_at,omitempty"`
}

// Candidate is an error group or metric series waiting for a session.
type Candidate struct {
	Group Group `json:"group"`
	Rank  int   `json:"rank,omitempty"` // metric series: admission order
}

// Track is the per-fingerprint history that outlives task truncation.
type Track struct {
	Attempts      int    `json:"attempts"`
	LastTask      string `json:"last_task,omitempty"`
	LastStatus    string `json:"last_status,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	MergedAt      string `json:"merged_at,omitempty"`
	Recurrence    string `json:"recurrence,omitempty"`
	LastSeen      string `json:"last_seen,omitempty"`
}

// Totals are the lifetime counters of a project.
type Totals struct {
	Sessions       int       `json:"sessions"`
	Rounds         int       `json:"rounds,omitempty"` // follow-up sessions, also counted in Sessions
	FailedSessions int       `json:"failed_sessions"`
	NoFix          int       `json:"no_fix"`
	PRsOpened      int       `json:"prs_opened"`
	PRsMerged      int       `json:"prs_merged"`
	PRsClosed      int       `json:"prs_closed"`
	LinesChanged   int       `json:"lines_changed"`
	Resolved       int       `json:"resolved"`
	Recurred       int       `json:"recurred"`
	MergeHours     []float64 `json:"merge_hours,omitempty"`     // ring of last 50
	SessionMinutes []float64 `json:"session_minutes,omitempty"` // ring of last 50
}

// Ledger is the durable work record of one project.
type Ledger struct {
	Cursor     string            `json:"cursor,omitempty"` // RFC3339Nano; next scan starts here
	Signal     string            `json:"signal,omitempty"` // Signal.key the candidates were scanned with
	Tasks      []Task            `json:"tasks"`            // newest first
	Candidates []Candidate       `json:"candidates,omitempty"`
	Tracks     map[string]*Track `json:"tracks,omitempty"`
	Totals     Totals            `json:"totals"`
	Daily      map[string]*Day   `json:"daily,omitempty"`
}

// Note is one entry of project memory.
type Note struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source"` // admin | session | review
	Task   string `json:"task,omitempty"`
	By     string `json:"by,omitempty"` // console email for admin notes
	At     string `json:"at"`
}

// Memory holds the notes that go into every work order.
type Memory struct {
	Notes []Note `json:"notes"` // newest first
}

type binding struct {
	Project       string `json:"project"`
	Task          string `json:"task"`
	Round         int    `json:"round,omitempty"`
	TokenHash     string `json:"token_hash"` // SHA-256 of the token arbetern issued; the token itself is never stored
	ClaudeSession string `json:"claude_session,omitempty"`
	Branch        string `json:"branch,omitempty"`
	CreatedAt     string `json:"created_at"`
}

func ledgerKey(id string) string { return StatePrefix + id + "/ledger.json" }

func memoryKey(id string) string { return StatePrefix + id + "/memory.json" }

func bindingKey(session string) string { return SessionPrefix + session + ".json" }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func earliest(a, b string) string {
	ta, tb := parseTime(a), parseTime(b)
	if ta.IsZero() || (!tb.IsZero() && tb.Before(ta)) {
		return b
	}
	return a
}

func latest(a, b string) string {
	ta, tb := parseTime(a), parseTime(b)
	if ta.IsZero() || tb.After(ta) {
		return b
	}
	return a
}

func isTerminal(status string) bool {
	switch status {
	case StatusMerged, StatusClosed, StatusNoFix, StatusFailed:
		return true
	}
	return false
}

func isActive(status string) bool { return status == StatusDispatching || status == StatusRunning }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func pushRing(ring []float64, v float64) []float64 {
	ring = append(ring, round(v, 2))
	if len(ring) > ringSize {
		ring = slices.Clone(ring[len(ring)-ringSize:])
	}
	return ring
}

func (l *Ledger) clone() *Ledger {
	out := &Ledger{}
	if body, err := json.Marshal(l); err == nil {
		_ = json.Unmarshal(body, out)
	}
	return out
}

func (l *Ledger) task(id string) *Task {
	for i := range l.Tasks {
		if l.Tasks[i].ID == id {
			return &l.Tasks[i]
		}
	}
	return nil
}

func (l *Ledger) openTask(fp string) *Task {
	for i := range l.Tasks {
		if l.Tasks[i].Fingerprint == fp && !isTerminal(l.Tasks[i].Status) {
			return &l.Tasks[i]
		}
	}
	return nil
}

func (l *Ledger) track(fp string) *Track {
	if l.Tracks == nil {
		l.Tracks = map[string]*Track{}
	}
	tr := l.Tracks[fp]
	if tr == nil {
		tr = &Track{}
		l.Tracks[fp] = tr
	}
	return tr
}

func (l *Ledger) day(now time.Time) *Day {
	date := now.UTC().Format(time.DateOnly)
	if l.Daily == nil {
		l.Daily = map[string]*Day{}
	}
	d := l.Daily[date]
	if d == nil {
		d = &Day{Date: date}
		l.Daily[date] = d
	}
	return d
}

func (l *Ledger) candidate(fp string) int {
	return slices.IndexFunc(l.Candidates, func(c Candidate) bool { return c.Group.Fingerprint == fp })
}

func (l *Ledger) counts() (open, active, rounds int) {
	for _, t := range l.Tasks {
		switch {
		case t.Status == StatusPROpen:
			open++
			if t.Review != nil && t.Review.active() != nil {
				rounds++
			}
		case isActive(t.Status):
			active++
		}
	}
	return open, active, rounds
}

func (l *Ledger) sessionsToday(now time.Time) int {
	if d := l.Daily[now.UTC().Format(time.DateOnly)]; d != nil {
		return d.Sessions
	}
	return 0
}

func (l *Ledger) roundCapacity(limits Limits) bool {
	_, active, rounds := l.counts()
	// The daily cap does not hold rounds back: new work spends it before review feedback arrives, and max_rounds bounds them.
	return active+rounds < limits.MaxActiveSessions
}

func (l *Ledger) workCapacity(limits Limits, now time.Time) bool {
	return l.roundCapacity(limits) && l.sessionsToday(now) < limits.MaxSessionsPerDay
}

func (l *Ledger) hasCapacity(limits Limits, now time.Time) bool {
	open, active, _ := l.counts()
	return open+active < limits.MaxOpenPRs && l.workCapacity(limits, now)
}

func (l *Ledger) fillCapacity(limits Limits, now time.Time) bool {
	open, active, _ := l.counts()
	// The last free slot stays for a series that breaches.
	return open+active+1 < limits.MaxOpenPRs && l.workCapacity(limits, now)
}

func (l *Ledger) eligible(fp string, limits Limits, now time.Time) bool {
	if l.openTask(fp) != nil {
		return false
	}
	tr := l.Tracks[fp]
	if tr == nil {
		return true
	}
	if tr.Attempts >= limits.MaxAttempts || tr.MergedAt != "" {
		return false
	}
	return !parseTime(tr.CooldownUntil).After(now)
}

func (l *Ledger) pick(signal string, limits Limits, now time.Time) (Candidate, bool) {
	if b := l.backlog(signal, limits, now, 1); len(b) > 0 {
		return b[0], true
	}
	return Candidate{}, false
}

// backlog stays empty while the candidates come from an earlier signal, until the next scan drops them.
func (l *Ledger) backlog(signal string, limits Limits, now time.Time, n int) []Candidate {
	out := []Candidate{}
	if l.Signal != signal {
		return out
	}
	for _, c := range l.Candidates {
		if n > 0 && len(out) == n {
			break
		}
		if l.eligible(c.Group.Fingerprint, limits, now) {
			out = append(out, c)
		}
	}
	return out
}

func (l *Ledger) recentOutcomes(exclude string, n int) []Task {
	var out []Task
	for _, t := range l.Tasks {
		if len(out) == n {
			break
		}
		if t.ID != exclude && isTerminal(t.Status) {
			out = append(out, t)
		}
	}
	return out
}

func (l *Ledger) openPRTasks(n int) []Task {
	var out []Task
	for i := len(l.Tasks) - 1; i >= 0 && len(out) < n; i-- {
		if t := l.Tasks[i]; t.Status == StatusPROpen && t.PR != nil {
			out = append(out, t)
		}
	}
	return out
}

func openingFresh(t *Task, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, t.Opening)
	return err == nil && now.Sub(at) < openingStaleAfter
}

func (l *Ledger) finish(t *Task, status string, now time.Time) {
	t.Status = status
	t.FinishedAt = stamp(now)
	t.UpdatedAt = t.FinishedAt
	t.Opening = ""
	t.Group.Samples = nil
	if t.Review != nil {
		for i := range t.Review.Rounds {
			rd := &t.Review.Rounds[i]
			if isActiveRound(rd.Status) {
				rd.Status, rd.Error, rd.FinishedAt = StatusFailed, "the pull request is "+status, t.FinishedAt
			}
			dropRoundBodies(rd)
		}
	}
	l.track(t.Fingerprint).LastStatus = status
}

func (l *Ledger) recordSessionTime(t *Task, now time.Time) {
	if at := parseTime(t.DispatchedAt); !at.IsZero() && !now.Before(at) {
		l.Totals.SessionMinutes = pushRing(l.Totals.SessionMinutes, now.Sub(at).Minutes())
	}
}

func (l *Ledger) addTask(t Task, limits Limits, now time.Time) bool {
	if l.task(t.ID) != nil || !l.hasCapacity(limits, now) || !l.eligible(t.Fingerprint, limits, now) {
		return false
	}
	i := l.candidate(t.Fingerprint)
	if i < 0 {
		return false
	}
	if t.Group.Fingerprint != t.Fingerprint {
		t.Group = l.Candidates[i].Group
	}
	t.Attempt = 1
	if tr := l.Tracks[t.Fingerprint]; tr != nil {
		t.Attempt = tr.Attempts + 1
	}
	t.Status = StatusDispatching
	t.CreatedAt = stamp(now)
	t.UpdatedAt = t.CreatedAt
	l.Candidates = slices.Delete(l.Candidates, i, i+1)
	l.Tasks = append([]Task{t}, l.Tasks...)
	l.compact(limits, now)
	return true
}

func (l *Ledger) markRunning(id string, st *started, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusDispatching {
		return false
	}
	t.Status = StatusRunning
	t.SessionID = st.SessionID
	t.ClaudeSession = st.ClaudeSession
	if t.Branch == "" {
		t.Branch = st.Branch
	}
	t.DispatchedAt = stamp(now)
	t.UpdatedAt = t.DispatchedAt
	t.Bound = true
	l.Totals.Sessions++
	l.day(now).Sessions++
	tr := l.track(t.Fingerprint)
	tr.Attempts++
	tr.LastTask = t.ID
	return true
}

func (l *Ledger) failTask(id string, from []string, reason string, now time.Time) bool {
	t := l.task(id)
	if t == nil || !slices.Contains(from, t.Status) {
		return false
	}
	t.Error = reason
	l.finish(t, StatusFailed, now)
	l.Totals.FailedSessions++
	l.track(t.Fingerprint).CooldownUntil = stamp(now.Add(failedCooldown))
	return true
}

func (l *Ledger) reportNoFix(id string, o Outcome, limits Limits, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusRunning || t.Outcome != nil || openingFresh(t, now) {
		return false
	}
	t.Outcome = &o
	l.recordSessionTime(t, now)
	l.finish(t, StatusNoFix, now)
	l.Totals.NoFix++
	l.track(t.Fingerprint).CooldownUntil = stamp(now.Add(time.Duration(limits.CooldownHours) * time.Hour))
	return true
}

func (l *Ledger) beginOpening(id, guard string, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusRunning || t.PR != nil || t.Outcome != nil || openingFresh(t, now) || t.OpenAttempts >= maxOpenAttempts {
		return false
	}
	t.Opening = guard
	t.OpenAttempts++
	return true
}

func (l *Ledger) setTicket(id, key, url string) bool {
	t := l.task(id)
	if t == nil || t.Ticket != "" {
		return false
	}
	t.Ticket, t.TicketURL = key, url
	return true
}

func (l *Ledger) clearOpening(id, guard string) bool {
	t := l.task(id)
	if t == nil || t.Opening == "" || t.Opening != guard {
		return false
	}
	t.Opening = ""
	return true
}

func (l *Ledger) openPR(id string, o Outcome, pr PR, follow, draft bool, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusRunning || t.PR != nil || t.Outcome != nil {
		return false
	}
	t.Outcome = &o
	t.Branch = o.Branch
	t.PR = &pr
	t.Opening = ""
	t.Status = StatusPROpen
	t.UpdatedAt = stamp(now)
	if follow {
		t.Review = &ReviewState{Waiting: o.Question, Draft: draft}
		t.Review.settle()
	}
	l.recordSessionTime(t, now)
	l.Totals.PRsOpened++
	l.day(now).Opened++
	return true
}

type prSnapshot struct {
	CreatedAt time.Time
	MergedAt  time.Time
	ClosedAt  time.Time
	Additions int
	Deletions int
}

func (l *Ledger) mergePR(id string, st prSnapshot, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusPROpen || t.PR == nil {
		return false
	}
	mergedAt := st.MergedAt
	if mergedAt.IsZero() {
		mergedAt = now
	}
	created := st.CreatedAt
	if created.IsZero() {
		created = parseTime(t.PR.CreatedAt)
	}
	t.PR.State = prStateMerged
	t.PR.MergedAt = stamp(mergedAt)
	t.PR.Additions, t.PR.Deletions = st.Additions, st.Deletions
	t.Recurrence = ""
	l.finish(t, StatusMerged, now)
	l.Totals.PRsMerged++
	l.Totals.LinesChanged += st.Additions + st.Deletions
	if !created.IsZero() && !mergedAt.Before(created) {
		l.Totals.MergeHours = pushRing(l.Totals.MergeHours, mergedAt.Sub(created).Hours())
	}
	l.day(now).Merged++
	tr := l.track(t.Fingerprint)
	tr.MergedAt = stamp(mergedAt)
	tr.Recurrence = ""
	tr.LastTask = t.ID
	return true
}

func (l *Ledger) closePR(id string, st prSnapshot, limits Limits, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusPROpen || t.PR == nil {
		return false
	}
	closedAt := st.ClosedAt
	if closedAt.IsZero() {
		closedAt = now
	}
	t.PR.State = prStateClosed
	t.PR.ClosedAt = stamp(closedAt)
	t.PR.Additions, t.PR.Deletions = st.Additions, st.Deletions
	l.finish(t, StatusClosed, now)
	l.Totals.PRsClosed++
	l.day(now).Closed++
	l.track(t.Fingerprint).CooldownUntil = stamp(now.Add(time.Duration(limits.CooldownHours) * time.Hour))
	return true
}

func (l *Ledger) markUnreadable(id string, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusPROpen || t.PR == nil || t.PR.UnreadableSince != "" {
		return false
	}
	t.PR.UnreadableSince = stamp(now)
	return true
}

func (l *Ledger) clearUnreadable(id string) bool {
	t := l.task(id)
	if t == nil || t.PR == nil || t.PR.UnreadableSince == "" {
		return false
	}
	t.PR.UnreadableSince = ""
	return true
}

func (l *Ledger) recur(fp string) bool {
	tr := l.Tracks[fp]
	if tr == nil || tr.MergedAt == "" || tr.Recurrence != "" {
		return false
	}
	tr.Recurrence = recurrenceRecurred
	tr.Attempts = 0
	tr.MergedAt = ""
	l.Totals.Recurred++
	if t := l.task(tr.LastTask); t != nil && t.Status == StatusMerged && t.Recurrence == "" {
		t.Recurrence = recurrenceRecurred
	}
	return true
}

func (l *Ledger) resolve(tr *Track) {
	tr.Recurrence = recurrenceResolved
	l.Totals.Resolved++
	if t := l.task(tr.LastTask); t != nil && t.Status == StatusMerged && t.Recurrence == "" {
		t.Recurrence = recurrenceResolved
	}
}

// resolveDue waits for the cursor rather than the clock, so logs from the window that are not scanned yet still count.
func (l *Ledger) resolveDue(limits Limits) bool {
	window := time.Duration(limits.RecurrenceWindowHours) * time.Hour
	scanned := parseTime(l.Cursor)
	changed := false
	for _, tr := range l.Tracks {
		merged := parseTime(tr.MergedAt)
		if merged.IsZero() || tr.Recurrence != "" || !scanned.After(merged.Add(window)) {
			continue
		}
		l.resolve(tr)
		changed = true
	}
	return changed
}

func (l *Ledger) reconcile(limits Limits, now time.Time) bool {
	timeout := time.Duration(limits.SessionTimeoutMinutes)*time.Minute + runningGrace
	var lost, ended, expired []string
	for _, t := range l.Tasks {
		switch {
		case t.Status == StatusDispatching && t.SessionID == "" && now.Sub(parseTime(t.CreatedAt)) > dispatchLostAfter:
			lost = append(lost, t.ID)
		case t.Status == StatusRunning && t.Outcome == nil && t.ExitReason != "" && !openingFresh(&t, now):
			ended = append(ended, t.ID)
		case t.Status == StatusRunning && t.Outcome == nil && now.Sub(parseTime(cmp.Or(t.DispatchedAt, t.CreatedAt))) > timeout:
			expired = append(expired, t.ID)
		}
	}
	for _, id := range lost {
		l.failTask(id, []string{StatusDispatching}, lostDispatch, now)
	}
	for _, id := range ended {
		l.failTask(id, []string{StatusRunning}, "session ended without a report (exit: "+l.task(id).ExitReason+")", now)
	}
	for _, id := range expired {
		l.failTask(id, []string{StatusRunning}, "session timed out", now)
	}
	rounds := l.reconcileRounds(timeout, now)
	return len(lost)+len(ended)+len(expired) > 0 || rounds
}

// reconcileJobs fails the running sessions whose Job finished, or has been gone for a while, without an end report.
func (l *Ledger) reconcileJobs(present, finished map[string]bool, now time.Time) bool {
	gone := func(id, since string) bool {
		return finished[id] || (!present[id] && now.Sub(parseTime(since)) > jobGoneAfter)
	}
	type ref struct {
		task string
		n    int
	}
	var tasks []string
	var rounds []ref
	for _, t := range l.Tasks {
		if t.Status == StatusRunning && t.Outcome == nil && t.ExitReason == "" && t.SessionID != "" && !openingFresh(&t, now) && gone(t.SessionID, cmp.Or(t.DispatchedAt, t.CreatedAt)) {
			tasks = append(tasks, t.ID)
		}
		if t.Status != StatusPROpen || t.Review == nil {
			continue
		}
		for _, rd := range t.Review.Rounds {
			if rd.Status == StatusRunning && rd.Outcome == nil && rd.ExitReason == "" && rd.SessionID != "" && !roundReportFresh(&rd, now) && gone(rd.SessionID, cmp.Or(rd.DispatchedAt, rd.CreatedAt)) {
				rounds = append(rounds, ref{t.ID, rd.N})
			}
		}
	}
	for _, id := range tasks {
		l.failTask(id, []string{StatusRunning}, jobFinished, now)
	}
	for _, r := range rounds {
		l.failRound(r.task, r.n, []string{StatusRunning}, jobFinished, now)
	}
	return len(tasks)+len(rounds) > 0
}

func (l *Ledger) unbind(ids []string) bool {
	changed := false
	for _, id := range ids {
		if t := l.task(id); t != nil && t.Bound {
			t.Bound = false
			changed = true
		}
	}
	return changed
}

// sessionFinished tells whether the session of a task (round 0) or of one of its follow-up rounds still works its work order, else when the work order ended.
func sessionFinished(t *Task, round int) (finishedAt string, running bool) {
	ended := cmp.Or(t.FinishedAt, t.UpdatedAt)
	if round > 0 {
		var rd *Round
		if t.Review != nil {
			rd = t.Review.round(round)
		}
		switch {
		case rd == nil || isTerminal(t.Status):
			return ended, false
		case isActiveRound(rd.Status):
			return "", true
		}
		return cmp.Or(rd.FinishedAt, ended), false
	}
	switch {
	case t.Status == StatusDispatching || t.Status == StatusRunning:
		return "", true
	case t.Outcome != nil && t.Outcome.ReportedAt != "":
		return t.Outcome.ReportedAt, false
	}
	return ended, false
}

// sessionDone reports whether a session's binding may go: its work order ended, and the pod reported its end or the wrap-up window passed.
func sessionDone(t *Task, round int, exitReason string, now time.Time) bool {
	finished, running := sessionFinished(t, round)
	if running {
		return false
	}
	return exitReason != "" || now.Sub(parseTime(finished)) > sessionWrapUp
}

func (l *Ledger) sessionEnded(id, reason, branch string, turns int, now time.Time) bool {
	t := l.task(id)
	if t == nil {
		return false
	}
	changed := false
	if t.ExitReason == "" && reason != "" {
		t.ExitReason = reason
		changed = true
	}
	if turns > 0 && t.Turns != turns {
		t.Turns = turns
		changed = true
	}
	if t.Status == StatusRunning && t.Outcome == nil && !openingFresh(t, now) {
		// The work a session left behind is on the branch it was on, which the gateway checked is its own or derived from it.
		if branch != "" {
			t.Branch = branch
		}
		l.failTask(id, []string{StatusRunning}, "session ended without a report (exit: "+cmp.Or(reason, "unknown")+")", now)
		changed = true
	}
	return changed
}

func (l *Ledger) setLearnings(id string, round, n int) bool {
	t := l.task(id)
	if t == nil {
		return false
	}
	count := &t.Learnings
	if round > 0 {
		if t.Review == nil || t.Review.round(round) == nil {
			return false
		}
		count = &t.Review.round(round).Learnings
	}
	if *count >= n {
		return false
	}
	*count = n
	return true
}

func (l *Ledger) setTranscript(id string, size int, now time.Time) bool {
	t := l.task(id)
	if t == nil {
		return false
	}
	t.Transcript, t.TranscriptSize = stamp(now), size
	return true
}

func (l *Ledger) inFlight(exclude string) []Task {
	var out []Task
	for _, t := range l.Tasks {
		if t.ID != exclude && t.Status == StatusPROpen {
			out = append(out, t)
		}
	}
	return out
}

// applyScan is a no-op unless the stored cursor is still from, the cursor the scan started at.
func (l *Ledger) applyScan(from, signal string, groups []Group, next time.Time, limits Limits, now time.Time) bool {
	if l.Cursor != from {
		return false
	}
	l.Cursor = next.UTC().Format(time.RFC3339Nano)
	if l.Signal != signal {
		l.Candidates, l.Signal = nil, signal
	}
	grace := time.Duration(limits.RecurrenceGraceHours) * time.Hour
	for _, g := range groups {
		l.sight(g, grace, limits)
		if t := l.openTask(g.Fingerprint); t != nil {
			t.Group.Count += g.Count
			t.Group.FirstSeen = earliest(t.Group.FirstSeen, g.FirstSeen)
			t.Group.LastSeen = latest(t.Group.LastSeen, g.LastSeen)
			continue
		}
		l.mergeCandidate(g)
	}
	l.compact(limits, now)
	return true
}

func (l *Ledger) applyMetricScan(signal string, groups []Group, window time.Duration, limits Limits, now time.Time) bool {
	l.Cursor = now.UTC().Format(time.RFC3339Nano)
	if l.Signal != signal {
		l.Candidates, l.Signal = nil, signal
	}
	firstSeen := map[string]string{}
	for _, c := range l.Candidates {
		firstSeen[c.Group.Fingerprint] = c.Group.FirstSeen
	}
	// A trailing sum keeps counting losses from before the fix for a whole window.
	grace := time.Duration(limits.RecurrenceGraceHours)*time.Hour + window
	cands := make([]Candidate, 0, len(groups))
	for i, g := range groups {
		// A fill series breaches nothing, so it says nothing about whether a merged fix held.
		if g.Reason != reasonFill {
			l.sight(g, grace, limits)
		}
		if t := l.openTask(g.Fingerprint); t != nil {
			t.Group.Value, t.Group.Kept, t.Group.Share, t.Group.Count = g.Value, g.Kept, g.Share, g.Count
			t.Group.LastSeen = latest(t.Group.LastSeen, g.LastSeen)
			continue
		}
		g.FirstSeen = earliest(g.FirstSeen, firstSeen[g.Fingerprint])
		cands = append(cands, Candidate{Group: g, Rank: i + 1})
	}
	l.Candidates = cands
	l.compact(limits, now)
	return true
}

func (l *Ledger) sight(g Group, grace time.Duration, limits Limits) {
	window := time.Duration(limits.RecurrenceWindowHours) * time.Hour
	first, last := parseTime(g.FirstSeen), parseTime(g.LastSeen)
	tr := l.track(g.Fingerprint)
	tr.LastSeen = latest(tr.LastSeen, g.LastSeen)
	merged := parseTime(tr.MergedAt)
	if tr.MergedAt != "" && tr.Recurrence == "" && first.After(merged.Add(window)) {
		// Seen only after the window, which the scans so far covered without a recurrence.
		l.resolve(tr)
	}
	switch {
	case tr.MergedAt != "" && tr.Recurrence == "" && last.After(merged.Add(grace)):
		l.recur(g.Fingerprint)
	case tr.MergedAt != "" && tr.Recurrence == recurrenceResolved && last.After(merged.Add(window)):
		// The fix held through its window; an error seen after that is new work with a fresh attempt budget.
		tr.Attempts, tr.MergedAt, tr.Recurrence = 0, "", ""
	}
}

func (l *Ledger) mergeCandidate(g Group) {
	g.Samples = slices.Clone(g.Samples)
	i := l.candidate(g.Fingerprint)
	if i < 0 {
		l.Candidates = append(l.Candidates, Candidate{Group: g})
		return
	}
	c := &l.Candidates[i].Group
	c.Count += g.Count
	c.FirstSeen = earliest(c.FirstSeen, g.FirstSeen)
	c.LastSeen = latest(c.LastSeen, g.LastSeen)
	c.Service, c.Kind, c.Pattern, c.Frame = g.Service, g.Kind, g.Pattern, g.Frame
	if len(g.Samples) > 0 {
		c.Samples = g.Samples
	}
}

func sortCandidates(cs []Candidate) {
	slices.SortStableFunc(cs, func(a, b Candidate) int {
		return cmp.Or(
			cmp.Compare(a.Rank, b.Rank),
			cmp.Compare(b.Group.Count, a.Group.Count),
			parseTime(b.Group.LastSeen).Compare(parseTime(a.Group.LastSeen)),
			strings.Compare(a.Group.Fingerprint, b.Group.Fingerprint),
		)
	})
}

// exhausted groups are kept out of the candidates, whose cap they would otherwise hold for good.
func (l *Ledger) exhausted(fp string, limits Limits) bool {
	tr := l.Tracks[fp]
	return tr != nil && tr.MergedAt == "" && tr.Attempts >= limits.MaxAttempts
}

func (l *Ledger) compact(limits Limits, now time.Time) {
	if l.Tasks == nil {
		l.Tasks = []Task{}
	}
	for i := range l.Tasks {
		if isTerminal(l.Tasks[i].Status) {
			l.Tasks[i].Group.Samples = nil
		}
	}
	for i := len(l.Tasks) - 1; i >= 0 && len(l.Tasks) > maxTasks; i-- {
		if isTerminal(l.Tasks[i].Status) {
			l.Tasks = slices.Delete(l.Tasks, i, i+1)
		}
	}
	cutoff := now.Add(-candidateMaxAge)
	l.Candidates = slices.DeleteFunc(l.Candidates, func(c Candidate) bool {
		return !parseTime(c.Group.LastSeen).After(cutoff) || l.exhausted(c.Group.Fingerprint, limits)
	})
	sortCandidates(l.Candidates)
	if len(l.Candidates) > maxCandidates {
		l.Candidates = l.Candidates[:maxCandidates]
	}
	l.trimTracks()
	oldest := now.UTC().AddDate(0, 0, -dailyKeepDays).Format(time.DateOnly)
	for date := range l.Daily {
		if date < oldest {
			delete(l.Daily, date)
		}
	}
}

// trimTracks never evicts a track that still steers an open task, a candidate or a merged fix awaiting its verdict.
func (l *Ledger) trimTracks() {
	if len(l.Tracks) <= maxTracks {
		return
	}
	busy := map[string]bool{}
	for _, t := range l.Tasks {
		if !isTerminal(t.Status) {
			busy[t.Fingerprint] = true
		}
	}
	for _, c := range l.Candidates {
		busy[c.Group.Fingerprint] = true
	}
	type entry struct {
		fp   string
		seen time.Time
		keep bool
	}
	entries := make([]entry, 0, len(l.Tracks))
	for fp, tr := range l.Tracks {
		entries = append(entries, entry{fp: fp, seen: parseTime(tr.LastSeen), keep: busy[fp] || tr.MergedAt != ""})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.keep != b.keep {
			if a.keep {
				return 1
			}
			return -1
		}
		return cmp.Or(a.seen.Compare(b.seen), strings.Compare(a.fp, b.fp))
	})
	for _, e := range entries[:len(entries)-maxTracks] {
		delete(l.Tracks, e.fp)
	}
}

func (m *Memory) has(id string) bool {
	return slices.ContainsFunc(m.Notes, func(n Note) bool { return n.ID == id })
}

func (m *Memory) add(n Note) bool {
	if m.has(n.ID) {
		return false
	}
	m.Notes = append([]Note{n}, m.Notes...)
	for len(m.Notes) > maxNotes {
		victim := len(m.Notes) - 1
		for i := len(m.Notes) - 1; i >= 0; i-- {
			if m.Notes[i].Source != noteAdmin {
				victim = i
				break
			}
		}
		m.Notes = slices.Delete(m.Notes, victim, victim+1)
	}
	return m.has(n.ID)
}

func (m *Memory) remove(id string) bool {
	n := len(m.Notes)
	m.Notes = slices.DeleteFunc(m.Notes, func(x Note) bool { return x.ID == id })
	return len(m.Notes) != n
}

func (m *Memory) count(source, task string) int {
	n := 0
	for _, x := range m.Notes {
		if x.Source == source && (task == "" || x.Task == task) {
			n++
		}
	}
	return n
}
