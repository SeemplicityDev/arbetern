package projects

import (
	"cmp"
	"slices"
	"time"
)

const (
	triggerChecks   = "checks"
	triggerFeedback = "feedback"
	triggerConflict = "conflict"

	ciPending = "pending"
	ciPassed  = "passed"
	ciFailed  = "failed"
	ciNone    = "none"
)

// ReviewState follows one open pull request until its checks pass and every review comment has an answer.
type ReviewState struct {
	Phase           string   `json:"phase"`
	Head            string   `json:"head,omitempty"`
	HeadAt          string   `json:"head_at,omitempty"` // when this head was first seen
	CI              string   `json:"ci,omitempty"`      // pending | passed | failed | none
	FailedChecks    []string `json:"failed_checks,omitempty"`
	Conflict        bool     `json:"conflict,omitempty"`
	Pending         int      `json:"pending,omitempty"` // review comments waiting for an answer
	LastFeedbackAt  string   `json:"last_feedback_at,omitempty"`
	Waiting         string   `json:"waiting,omitempty"`          // the question or blocker a person has to answer
	ChecksHandled   string   `json:"checks_handled,omitempty"`   // head whose failing checks a round already took on
	ConflictHandled string   `json:"conflict_handled,omitempty"` // head whose conflict a round already took on
	RerunHead       string   `json:"rerun_head,omitempty"`       // head whose failed jobs were re-run
	Handled         []string `json:"handled,omitempty"`          // answered feedback ids, oldest first
	Rounds          []Round  `json:"rounds,omitempty"`
	NextRoundAfter  string   `json:"next_round_after,omitempty"`
	Draft           bool     `json:"draft,omitempty"`    // arbetern opened the pull request as a draft
	ReadyAt         string   `json:"ready_at,omitempty"` // when arbetern took the pull request out of draft
	DoneHead        string   `json:"done_head,omitempty"`
	DoneAt          string   `json:"done_at,omitempty"`

	Content     string      `json:"content,omitempty"`      // fingerprint of the head's own changes over the base branch
	DoneContent string      `json:"done_content,omitempty"` // the changes the last ready note announced
	AutoMerge   *MergeState `json:"auto_merge,omitempty"`
}

// Round is one follow-up session on an open pull request.
type Round struct {
	N            int            `json:"n"`
	Trigger      []string       `json:"trigger"` // checks | feedback | conflict
	Status       string         `json:"status"`  // dispatching | running | done | failed
	SessionID    string         `json:"session_id,omitempty"`
	Turns        int            `json:"turns,omitempty"`
	HeadBefore   string         `json:"head_before,omitempty"`
	HeadAfter    string         `json:"head_after,omitempty"`
	Items        []Feedback     `json:"items,omitempty"` // the comments to answer; bodies are dropped when the round ends
	Context      []Feedback     `json:"context,omitempty"`
	Checks       []CheckFailure `json:"checks,omitempty"`
	Outcome      *Outcome       `json:"outcome,omitempty"`
	Replies      int            `json:"replies,omitempty"`
	Learnings    int            `json:"learnings,omitempty"`
	Reporting    string         `json:"reporting,omitempty"` // guard while the report is posted; stale after 2m
	Bound        bool           `json:"bound,omitempty"`
	ExitReason   string         `json:"exit_reason,omitempty"`
	Error        string         `json:"error,omitempty"`
	CreatedAt    string         `json:"created_at"`
	DispatchedAt string         `json:"dispatched_at,omitempty"`
	FinishedAt   string         `json:"finished_at,omitempty"`
}

// Feedback is one review comment a round answers or reads for context.
type Feedback struct {
	ID     string `json:"id"` // review:<id> | inline:<id> | comment:<id>
	Author string `json:"author"`
	Bot    bool   `json:"bot,omitempty"`
	State  string `json:"state,omitempty"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Thread int64  `json:"thread,omitempty"` // first comment of an inline thread
	Body   string `json:"body,omitempty"`
	URL    string `json:"url,omitempty"`
	At     string `json:"at"`
}

// CheckFailure is a failing check a round has to fix or explain.
type CheckFailure struct {
	Name        string   `json:"name"`
	Conclusion  string   `json:"conclusion"`
	URL         string   `json:"url,omitempty"`
	Title       string   `json:"title,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
	Log         string   `json:"log,omitempty"`
}

type observation struct {
	head         string
	seenHead     string // the head the ledger held when the pull request was read
	ci           string
	failed       []string
	conflict     bool
	pending      int
	lastFeedback time.Time
	content      string // fingerprint of the head's own changes; empty when it could not be read
}

type roundResult struct {
	outcome   Outcome
	head      string
	replies   int
	rerun     bool
	described *Outcome
	bodySHA   string
}

func isActiveRound(status string) bool { return status == StatusDispatching || status == StatusRunning }

// personSpoke reports whether a person, not a bot, commented in the round: only a person can answer an open question.
func personSpoke(rd *Round) bool {
	return slices.ContainsFunc(rd.Items, func(f Feedback) bool { return !f.Bot })
}

func (r *ReviewState) round(n int) *Round {
	for i := range r.Rounds {
		if r.Rounds[i].N == n {
			return &r.Rounds[i]
		}
	}
	return nil
}

func (r *ReviewState) active() *Round {
	for i := range r.Rounds {
		if isActiveRound(r.Rounds[i].Status) {
			return &r.Rounds[i]
		}
	}
	return nil
}

func (r *ReviewState) used() int {
	n := 0
	for _, rd := range r.Rounds {
		if rd.SessionID != "" {
			n++
		}
	}
	return n
}

func (r *ReviewState) isHandled(id string) bool { return slices.Contains(r.Handled, id) }

func (r *ReviewState) markHandled(ids []string) {
	for _, id := range ids {
		if !r.isHandled(id) {
			r.Handled = append(r.Handled, id)
		}
	}
	if over := len(r.Handled) - maxHandled; over > 0 {
		r.Handled = slices.Clone(r.Handled[over:])
	}
}

func (r *ReviewState) quietSince() time.Time {
	at := parseTime(latest(latest(r.HeadAt, r.LastFeedbackAt), r.ReadyAt))
	for _, rd := range r.Rounds {
		if t := parseTime(rd.FinishedAt); t.After(at) {
			at = t
		}
	}
	return at
}

func (r *ReviewState) settle() {
	switch {
	case r.Phase == PhaseExhausted:
	case r.active() != nil:
		r.Phase = PhaseFixing
	case r.Waiting != "":
		r.Phase = PhaseWaiting
	case r.DoneHead != "" && r.DoneHead == r.Head && r.Pending == 0 && r.CI != ciFailed:
		r.Phase = PhaseDone
	default:
		r.Phase = PhaseChecking
	}
}

func dropRoundBodies(rd *Round) {
	for i := range rd.Items {
		rd.Items[i].Body = ""
	}
	rd.Context, rd.Checks = nil, nil
}

func (l *Ledger) review(id string) (*Task, *ReviewState) {
	t := l.task(id)
	if t == nil || t.Status != StatusPROpen || t.Review == nil {
		return nil, nil
	}
	return t, t.Review
}

func (l *Ledger) observe(id string, o observation, now time.Time) bool {
	t := l.task(id)
	if t == nil || t.Status != StatusPROpen {
		return false
	}
	started := t.Review == nil
	if started {
		t.Review = &ReviewState{}
	}
	rv := t.Review
	before := *rv
	// A round that reported after the read may have moved the head on; an older read must not move it back.
	if o.head != "" && o.head != rv.Head && (o.seenHead == "" || rv.Head == o.seenHead) {
		if rv.AutoMerge != nil {
			rv.AutoMerge.headMoved(o.head)
		}
		rv.Head, rv.HeadAt = o.head, stamp(now)
	}
	seen := false
	if o.content != "" && rv.Head == o.head && rv.Content != o.content {
		if rv.Content != "" && rv.AutoMerge != nil {
			rv.AutoMerge.changesChanged()
		}
		rv.Content, seen = o.content, true
	}
	// A pull request marked done before changes were recorded announced its done head.
	if o.content != "" && rv.DoneContent == "" && rv.DoneHead == o.head && rv.Head == o.head {
		rv.DoneContent, seen = o.content, true
	}
	rv.CI, rv.Conflict, rv.Pending = o.ci, o.conflict, o.pending
	rv.FailedChecks = slices.Clone(o.failed)
	if !o.lastFeedback.IsZero() {
		rv.LastFeedbackAt = latest(rv.LastFeedbackAt, stamp(o.lastFeedback))
	}
	rv.settle()
	return started || seen || rv.Head != before.Head || rv.CI != before.CI || rv.Conflict != before.Conflict || rv.Pending != before.Pending ||
		!slices.Equal(rv.FailedChecks, before.FailedChecks) || rv.LastFeedbackAt != before.LastFeedbackAt || rv.Phase != before.Phase
}

func (l *Ledger) addRound(id string, rd Round, maxRounds int, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil || rv.active() != nil || rv.used() >= maxRounds || rv.Phase == PhaseExhausted || parseTime(rv.NextRoundAfter).After(now) {
		return false
	}
	if slices.Contains(rd.Trigger, triggerFeedback) {
		rd.Items = slices.DeleteFunc(slices.Clone(rd.Items), func(f Feedback) bool { return rv.isHandled(f.ID) })
		if len(rd.Items) == 0 {
			rd.Trigger = slices.DeleteFunc(slices.Clone(rd.Trigger), func(s string) bool { return s == triggerFeedback })
			if len(rd.Trigger) == 0 {
				return false
			}
		}
	}
	rv.Rounds = slices.DeleteFunc(rv.Rounds, func(r Round) bool { return r.Status == StatusFailed && r.SessionID == "" })
	rd.N = len(rv.Rounds) + 1
	rd.Status = StatusDispatching
	rd.CreatedAt = stamp(now)
	rv.Rounds = append(rv.Rounds, rd)
	rv.AutoMerge.disarm()
	rv.settle()
	return true
}

func (l *Ledger) roundRunning(id string, n int, st *started, now time.Time) bool {
	t, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil || rd.Status != StatusDispatching {
		return false
	}
	rd.Status = StatusRunning
	rd.SessionID = st.SessionID
	if t.ClaudeSession != st.ClaudeSession {
		t.ClaudeSession, t.Transcript, t.TranscriptSize = st.ClaudeSession, "", 0
	}
	rd.DispatchedAt = stamp(now)
	rd.Bound = true
	t.UpdatedAt = rd.DispatchedAt
	l.Totals.Sessions++
	l.Totals.Rounds++
	l.day(now).Sessions++
	return true
}

func (l *Ledger) failRound(id string, n int, from []string, reason string, now time.Time) bool {
	t, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil || !slices.Contains(from, rd.Status) {
		return false
	}
	rd.Status = StatusFailed
	rd.Error = reason
	rd.Reporting = ""
	rd.FinishedAt = stamp(now)
	dropRoundBodies(rd)
	rv.NextRoundAfter = stamp(now.Add(roundFailedCooldown))
	rv.settle()
	t.UpdatedAt = rd.FinishedAt
	l.Totals.FailedSessions++
	return true
}

func roundReportFresh(rd *Round, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, rd.Reporting)
	return err == nil && now.Sub(at) < openingStaleAfter
}

func (l *Ledger) beginRoundReport(id string, n int, guard string, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil || rd.Status != StatusRunning || rd.Outcome != nil || roundReportFresh(rd, now) {
		return false
	}
	rd.Reporting = guard
	return true
}

func (l *Ledger) clearRoundReport(id string, n int, guard string) bool {
	_, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil || rd.Reporting == "" || rd.Reporting != guard {
		return false
	}
	rd.Reporting = ""
	return true
}

func (l *Ledger) finishRound(id string, n int, res roundResult, now time.Time) bool {
	t, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil || rd.Status != StatusRunning || rd.Outcome != nil {
		return false
	}
	o := res.outcome
	rd.Outcome = &o
	rd.Status = RoundDone
	rd.Reporting = ""
	rd.FinishedAt = stamp(now)
	recordRoundResult(t, rv, rd, res, now)
	if at := parseTime(rd.DispatchedAt); !at.IsZero() && !now.Before(at) {
		l.Totals.SessionMinutes = pushRing(l.Totals.SessionMinutes, now.Sub(at).Minutes())
	}
	return true
}

func (l *Ledger) roundEnded(id string, n int, reason string, turns int, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil {
		return false
	}
	changed := false
	if rd.ExitReason == "" && reason != "" {
		rd.ExitReason = reason
		changed = true
	}
	if turns > 0 && rd.Turns != turns {
		rd.Turns = turns
		changed = true
	}
	if rd.Status == StatusRunning && rd.Outcome == nil && !roundReportFresh(rd, now) {
		l.failRound(id, n, []string{StatusRunning}, "session ended without a report (exit: "+cmp.Or(reason, "unknown")+")", now)
		changed = true
	}
	return changed
}

func (l *Ledger) markReady(id string, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil || rv.ReadyAt != "" {
		return false
	}
	rv.ReadyAt = stamp(now)
	return true
}

func (l *Ledger) markDone(id, head, content string, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil || head == "" || rv.Head != head || rv.DoneHead == head {
		return false
	}
	rv.DoneHead, rv.DoneAt = head, stamp(now)
	if content != "" {
		rv.DoneContent = content
	}
	rv.settle()
	return true
}

func (l *Ledger) exhaust(id string) bool {
	_, rv := l.review(id)
	if rv == nil || rv.Phase == PhaseExhausted || rv.active() != nil {
		return false
	}
	rv.Phase = PhaseExhausted
	return true
}

func (l *Ledger) reconcileRounds(timeout time.Duration, now time.Time) bool {
	type ref struct {
		task string
		n    int
	}
	var lost, ended, expired []ref
	for _, t := range l.Tasks {
		if t.Status != StatusPROpen || t.Review == nil {
			continue
		}
		for _, rd := range t.Review.Rounds {
			switch {
			case rd.Status == StatusDispatching && rd.SessionID == "" && now.Sub(parseTime(rd.CreatedAt)) > dispatchLostAfter:
				lost = append(lost, ref{t.ID, rd.N})
			case rd.Status == StatusRunning && rd.Outcome == nil && rd.ExitReason != "" && !roundReportFresh(&rd, now):
				ended = append(ended, ref{t.ID, rd.N})
			case rd.Status == StatusRunning && rd.Outcome == nil && !roundReportFresh(&rd, now) && now.Sub(parseTime(cmp.Or(rd.DispatchedAt, rd.CreatedAt))) > timeout:
				expired = append(expired, ref{t.ID, rd.N})
			}
		}
	}
	for _, r := range lost {
		l.failRound(r.task, r.n, []string{StatusDispatching}, lostDispatch, now)
	}
	for _, r := range ended {
		_, rv := l.review(r.task)
		l.failRound(r.task, r.n, []string{StatusRunning}, "session ended without a report (exit: "+rv.round(r.n).ExitReason+")", now)
	}
	for _, r := range expired {
		l.failRound(r.task, r.n, []string{StatusRunning}, "session timed out", now)
	}
	return len(lost)+len(ended)+len(expired) > 0
}

func (l *Ledger) boundRoundSessions() map[string][]int {
	out := map[string][]int{}
	for _, t := range l.Tasks {
		if t.Review == nil {
			continue
		}
		for _, rd := range t.Review.Rounds {
			if rd.Bound {
				out[t.ID] = append(out[t.ID], rd.N)
			}
		}
	}
	return out
}

// recordRoundResult applies what a round posted: the comments answered, the head it left, and the marks that keep the same checks or conflict from starting another round.
func recordRoundResult(t *Task, rv *ReviewState, rd *Round, res roundResult, now time.Time) {
	o := res.outcome
	rd.Replies = res.replies
	rd.HeadAfter = cmp.Or(res.head, rd.HeadBefore)
	ids := make([]string, 0, len(rd.Items))
	for _, it := range rd.Items {
		ids = append(ids, it.ID)
	}
	rv.markHandled(ids)
	if rd.HeadAfter == rd.HeadBefore {
		if slices.Contains(rd.Trigger, triggerChecks) {
			rv.ChecksHandled = rd.HeadBefore
		}
		if slices.Contains(rd.Trigger, triggerConflict) {
			rv.ConflictHandled = rd.HeadBefore
		}
	}
	if res.rerun {
		rv.RerunHead = rd.HeadAfter
	}
	if d := res.described; d != nil && t.Outcome != nil && t.PR != nil {
		t.Outcome.Summary, t.Outcome.Question = d.Summary, d.Question
		t.PR.BodySHA = res.bodySHA
	}
	switch {
	case o.Question != "":
		rv.Waiting = o.Question
	case o.Status == OutcomeCannotFix:
		rv.Waiting = oneLine(o.Summary, maxQuestionChars)
	case personSpoke(rd):
		rv.Waiting = ""
	}
	if rd.HeadAfter != rv.Head {
		rv.Head, rv.HeadAt = rd.HeadAfter, stamp(now)
	}
	dropRoundBodies(rd)
	rv.settle()
	t.UpdatedAt = stamp(now)
}

// recordRoundReplies keeps what a round posted when arbetern had ended it before its report landed, so the next round does not answer the same comments again.
func (l *Ledger) recordRoundReplies(id string, n int, res roundResult, now time.Time) bool {
	t, rv := l.review(id)
	if rv == nil {
		return false
	}
	rd := rv.round(n)
	if rd == nil || rd.Outcome != nil {
		return false
	}
	recordRoundResult(t, rv, rd, res, now)
	return true
}

func (l *Ledger) unbindRounds(done map[string][]int) bool {
	changed := false
	for id, ns := range done {
		t := l.task(id)
		if t == nil || t.Review == nil {
			continue
		}
		for _, n := range ns {
			if rd := t.Review.round(n); rd != nil && rd.Bound {
				rd.Bound = false
				changed = true
			}
		}
	}
	return changed
}
