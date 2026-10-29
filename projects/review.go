package projects

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/text"
)

const (
	maxListedChecks     = 10
	maxCheckAnnotations = 10
)

var failedConclusions = []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale"}

type prWatch struct {
	taskID  string
	owner   string
	repo    string
	number  int
	pr      *github.PRState
	items   []Feedback
	context []Feedback
	ci      string
	failed  []github.Check

	content      string
	lineage      []string
	retargetedAt time.Time
	snapshot     string
	reviews      []github.DiscussionItem
	ourAutoMerge bool
}

type roundWant struct {
	watch   prWatch
	trigger []string
}

func ciState(checks []github.Check) (string, []github.Check) {
	pending := false
	var failed []github.Check
	for _, c := range checks {
		switch {
		case c.Status != "completed":
			pending = true
		case slices.Contains(failedConclusions, c.Conclusion):
			failed = append(failed, c)
		}
	}
	switch {
	case pending:
		return ciPending, failed
	case len(failed) > 0:
		return ciFailed, failed
	case len(checks) == 0:
		return ciNone, nil
	}
	return ciPassed, nil
}

func feedbackID(kind string, id int64) string { return kind + ":" + strconv.FormatInt(id, 10) }

func sortFeedback(items []github.DiscussionItem, self string, rv *ReviewState) (act, context []Feedback, last time.Time) {
	var reports []Feedback
	for _, it := range items {
		mine := strings.EqualFold(it.Author, self)
		// The marker also covers what arbetern posted under an earlier GitHub identity, such as a token's account before a GitHub App.
		if it.Body == "" || mine && (it.Kind != github.DiscussionComment || ownComment(it.Body)) || strings.Contains(it.Body, commentMarker) {
			continue
		}
		f := Feedback{
			ID:     feedbackID(it.Kind, it.ID),
			Author: it.Author,
			Bot:    it.Bot,
			State:  it.State,
			Path:   it.Path,
			Line:   it.Line,
			Body:   it.Body,
			URL:    it.URL,
			At:     stamp(it.CreatedAt),
		}
		if mine {
			// A CI that posts its reports with arbetern's own token: what a failing check found, never a comment to answer.
			f.Bot, f.At = true, stamp(laterOf(it.CreatedAt, it.UpdatedAt))
			reports = append(reports, f)
			continue
		}
		actionable := it.Trusted() && !it.Bot
		switch it.Kind {
		case github.DiscussionInline:
			f.Thread = cmp.Or(it.InReplyTo, it.ID)
			actionable = it.Bot || it.Trusted()
		case github.DiscussionReview:
			actionable = actionable && (it.State == "COMMENTED" || it.State == "CHANGES_REQUESTED")
		}
		switch {
		case actionable && !rv.isHandled(f.ID):
			act = append(act, f)
			if it.CreatedAt.After(last) {
				last = it.CreatedAt
			}
		case !actionable && it.Bot:
			f.At = stamp(laterOf(it.CreatedAt, it.UpdatedAt))
			context = append(context, f)
		}
	}
	slices.SortStableFunc(act, func(a, b Feedback) int { return parseTime(a.At).Compare(parseTime(b.At)) })
	newest := func(a, b Feedback) int { return parseTime(b.At).Compare(parseTime(a.At)) }
	slices.SortStableFunc(reports, newest)
	slices.SortStableFunc(context, newest)
	context = append(reports, context...)
	if len(context) > maxContextComments {
		context = context[:maxContextComments]
	}
	return act, context, last
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func ownComment(body string) bool {
	return strings.Contains(body, commentMarker) || strings.Contains(body, footerPrefix) || strings.HasPrefix(body, "> ")
}

func (r *Registry) observePR(ctx context.Context, p *Project, led *Ledger, t Task, owner, repo string, number int, pr *github.PRState, out *tickOutcome) (*Ledger, error) {
	self := r.tokenLogin(ctx)
	if self == "" {
		return led, errors.New("the GitHub token's login is unknown, so the project's own comments cannot be told apart")
	}
	discussion, err := r.gh.PullRequestDiscussion(ctx, owner, repo, number)
	if err != nil {
		return led, err
	}
	checks, err := r.gh.ChecksForRef(ctx, owner, repo, pr.HeadSHA)
	if err != nil {
		return led, err
	}
	rv := t.Review
	if rv == nil {
		rv = &ReviewState{}
	}
	items, context, last := sortFeedback(discussion, self, rv)
	ci, failed := ciState(checks)
	names := make([]string, 0, len(failed))
	for _, c := range failed {
		names = append(names, c.Name)
	}
	obs := observation{
		head:         pr.HeadSHA,
		seenHead:     rv.Head,
		ci:           ci,
		failed:       names,
		conflict:     pr.Mergeable != nil && !*pr.Mergeable,
		pending:      len(items),
		lastFeedback: last,
	}
	if obs.changes, err = r.readChanges(ctx, owner, repo, pr, rv); err != nil {
		log.Printf("[projects] %s task %s: reading the changes of pull request #%d: %v", p.ID, t.ID, number, err)
	}
	reviews := slices.DeleteFunc(slices.Clone(discussion), func(it github.DiscussionItem) bool {
		return it.Kind != github.DiscussionReview || strings.EqualFold(it.Author, self)
	})
	now := time.Now().UTC()
	next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.observe(t.ID, obs, now) })
	if err != nil {
		return led, err
	}
	w := prWatch{
		taskID: t.ID, owner: owner, repo: repo, number: number, pr: pr, items: items, context: context, ci: ci, failed: failed, reviews: reviews,
		ourAutoMerge: pr.AutoMerge && strings.EqualFold(pr.AutoMergeBy, self),
	}
	if _, nrv := next.review(t.ID); nrv != nil && nrv.ContentHead == pr.HeadSHA && nrv.ContentBase == pr.BaseRef {
		w.content, w.lineage, w.retargetedAt = nrv.Content, slices.Clone(nrv.Lineage), parseTime(nrv.RetargetedAt)
	}
	w.snapshot = mergeSnapshot(pr, w.content, checks, discussion, self, p.Review.effective().MergeMethod)
	out.watches = append(out.watches, w)
	return next, nil
}

func needsRound(rv *ReviewState, w prWatch, now time.Time) []string {
	if rv.Phase == PhaseExhausted {
		return nil
	}
	settled := w.ci != ciPending || now.Sub(parseTime(rv.HeadAt)) > ciWaitLimit
	var need []string
	if len(w.items) > 0 && settled {
		need = append(need, triggerFeedback)
	}
	if len(w.failed) > 0 && settled && rv.ChecksHandled != w.pr.HeadSHA {
		need = append(need, triggerChecks)
	}
	if rv.Conflict && rv.ConflictHandled != w.pr.HeadSHA {
		need = append(need, triggerConflict)
	}
	return need
}

func takeOutOfDraft(rv *ReviewState, w prWatch) bool {
	// A person who turns the pull request back into a draft keeps it that way.
	return w.pr.Draft && rv.Draft && rv.ReadyAt == ""
}

func readyFor(rv *ReviewState, w prWatch, review Review, now time.Time) bool {
	switch {
	case rv.Phase == PhaseExhausted || rv.Waiting != "" || rv.Conflict || len(w.items) > 0:
		return false
	case w.ci == ciPending || w.ci == ciFailed:
		return false
	case w.ci == ciNone && now.Sub(parseTime(rv.HeadAt)) < ciNoChecksGrace:
		return false
	case rv.DoneHead == w.pr.HeadSHA && !takeOutOfDraft(rv, w):
		return false
	}
	return now.Sub(rv.quietSince()) >= time.Duration(review.SettleMinutes)*time.Minute
}

func (r *Registry) advanceReviews(ctx context.Context, p *Project, led *Ledger, out *tickOutcome) *Ledger {
	review := p.Review.effective()
	now := time.Now().UTC()
	for _, w := range out.watches {
		_, rv := led.review(w.taskID)
		// The watch was read before rounds that reported during this tick: skip a moved head, drop comments since answered.
		if rv == nil || rv.active() != nil || rv.Head != w.pr.HeadSHA {
			continue
		}
		w.items = slices.DeleteFunc(slices.Clone(w.items), func(f Feedback) bool { return rv.isHandled(f.ID) })
		need := needsRound(rv, w, now)
		var ok bool
		if led, ok = r.followAutoMerge(ctx, p, led, w, review, need, now); !ok {
			continue
		}
		if _, rv = led.review(w.taskID); rv == nil {
			continue
		}
		switch {
		case len(need) > 0 && rv.used() >= review.MaxRounds:
			led = r.stopFollowing(ctx, p, led, w, rv, need)
		case len(need) > 0:
			if !parseTime(rv.NextRoundAfter).After(now) {
				out.rounds = append(out.rounds, roundWant{watch: w, trigger: need})
			}
		case readyFor(rv, w, review, now):
			led = r.finishReview(ctx, p, led, w, now)
		case review.AutoMerge && w.content != "" && mergeReady(rv, w):
			led = r.autoMerge(ctx, p, led, w, review, now)
		}
	}
	return led
}

func (r *Registry) finishReview(ctx context.Context, p *Project, led *Ledger, w prWatch, now time.Time) *Ledger {
	k := p.ID
	_, rv := led.review(w.taskID)
	if takeOutOfDraft(rv, w) {
		if err := r.gh.MarkReadyForReview(ctx, w.pr.NodeID); err != nil {
			log.Printf("[projects] %s task %s: marking pull request #%d ready for review: %v", k, w.taskID, w.number, err)
			return led
		}
		r.gh.RequestCopilotReview(ctx, w.owner, w.repo, w.number)
		next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.markReady(w.taskID, now) })
		if err != nil {
			log.Printf("[projects] %s task %s: recording pull request #%d as ready: %v", k, w.taskID, w.number, err)
			return led
		}
		log.Printf("[projects] %s task %s: pull request #%d marked ready for review", k, w.taskID, w.number)
		return next
	}
	if w.content == "" && rv.DoneContent != "" {
		return led
	}
	// The earlier ready note still holds while GitHub merges this head by itself, or when only the base branch was merged in.
	known := w.content != "" && rv.DoneContent != ""
	repeat := rv.DoneAt != "" && (known && rv.DoneContent == w.content || !known && w.pr.AutoMerge)
	if !repeat {
		if _, err := r.gh.CommentOnPullRequest(ctx, w.owner, w.repo, w.number, doneComment(p, rv, w.pr.HeadSHA)); err != nil {
			log.Printf("[projects] %s task %s: posting the ready note on pull request #%d: %v", k, w.taskID, w.number, err)
			return led
		}
	}
	next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.markDone(w.taskID, w.pr.HeadSHA, w.content, now) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording pull request #%d as done: %v", k, w.taskID, w.number, err)
		return led
	}
	log.Printf("[projects] %s task %s: pull request #%d is done (head %s)", k, w.taskID, w.number, shortSHA(w.pr.HeadSHA))
	if t := next.task(w.taskID); t != nil && !repeat {
		r.notify(p, noteReady(t))
	}
	return next
}

func (r *Registry) stopFollowing(ctx context.Context, p *Project, led *Ledger, w prWatch, rv *ReviewState, need []string) *Ledger {
	k := p.ID
	if _, err := r.gh.CommentOnPullRequest(ctx, w.owner, w.repo, w.number, exhaustedComment(p, rv, w, need)); err != nil {
		log.Printf("[projects] %s task %s: posting the stop note on pull request #%d: %v", k, w.taskID, w.number, err)
		return led
	}
	next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.exhaust(w.taskID) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording that pull request #%d ran out of rounds: %v", k, w.taskID, w.number, err)
		return led
	}
	log.Printf("[projects] %s task %s: pull request #%d used its %d follow-up rounds", k, w.taskID, w.number, rv.used())
	if t := next.task(w.taskID); t != nil {
		r.notify(p, noteExhausted(t, rv.used()))
	}
	return next
}

func (r *Registry) checkFailures(ctx context.Context, w prWatch) []CheckFailure {
	out := make([]CheckFailure, 0, min(len(w.failed), maxListedChecks))
	for i, c := range w.failed {
		if i == maxListedChecks {
			break
		}
		cf := CheckFailure{
			Name:       oneLine(c.Name, 200),
			Conclusion: c.Conclusion,
			URL:        c.URL,
			Title:      oneLine(redact.Text(c.Title), 300),
			Summary:    text.Truncate(redact.Text(c.Summary), 1000),
		}
		if c.ID != 0 && i < maxFailedChecks {
			if anns, err := r.gh.CheckAnnotations(ctx, w.owner, w.repo, c.ID); err == nil {
				for _, a := range anns[:min(len(anns), maxCheckAnnotations)] {
					cf.Annotations = append(cf.Annotations, oneLine(redact.Text(annotationLine(a)), 400))
				}
			}
			if c.App == "github-actions" {
				if logText, err := r.gh.JobLog(ctx, w.owner, w.repo, c.ID); err == nil {
					cf.Log = failureExcerpt(redact.Text(logText), checkLogChars)
				}
			}
		}
		out = append(out, cf)
	}
	return out
}

var (
	logStampRe = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z `)
	ansiRe     = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
)

func failureExcerpt(log string, max int) string {
	log = ansiRe.ReplaceAllString(logStampRe.ReplaceAllString(log, ""), "")
	if i := strings.Index(log, "##[error]"); i >= 0 {
		if end := strings.IndexByte(log[i:], '\n'); end >= 0 {
			log = log[:i+end]
		}
	}
	return text.TruncateStart(log, max)
}

func annotationLine(a github.Annotation) string {
	where := a.Path
	if a.Line > 0 {
		where += ":" + strconv.Itoa(a.Line)
	}
	head := strings.TrimSpace(strings.Join([]string{where, a.Level, a.Title}, " "))
	if head == "" {
		return a.Message
	}
	return head + " — " + a.Message
}

func (r *Registry) buildRound(ctx context.Context, want roundWant) Round {
	w := want.watch
	rd := Round{Trigger: want.trigger, HeadBefore: w.pr.HeadSHA, Context: w.context}
	if slices.Contains(want.trigger, triggerFeedback) {
		rd.Items = w.items[:min(len(w.items), maxRoundItems)]
	}
	if slices.Contains(want.trigger, triggerChecks) {
		rd.Checks = r.checkFailures(ctx, w)
	}
	return rd
}

func (r *Registry) dispatchRounds(ctx context.Context, p *Project, limits Limits, out *tickOutcome) {
	led := out.ledger
	maxRounds := p.Review.effective().MaxRounds
	for _, want := range out.rounds {
		if !led.roundCapacity(limits) {
			break
		}
		now := time.Now().UTC()
		rd := r.buildRound(ctx, want)
		next, added, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.addRound(want.watch.taskID, rd, maxRounds, now) })
		if err != nil {
			out.fail("recording a follow-up round: %v", err)
			break
		}
		led = next
		_, rv := led.review(want.watch.taskID)
		if !added || rv == nil || rv.active() == nil {
			continue
		}
		var fired bool
		led, fired = r.fireRound(ctx, p, led, want.watch.taskID, rv.active().N, out)
		if !fired {
			break
		}
	}
	out.ledger = led
}

// fireRound reports whether dispatching may continue.
func (r *Registry) fireRound(ctx context.Context, p *Project, led *Ledger, taskID string, n int, out *tickOutcome) (*Ledger, bool) {
	var st *started
	err := errors.New("the task is no longer in the ledger")
	if t := led.task(taskID); t != nil {
		st, err = r.launch(ctx, p, t, n)
	}
	// The Job may have started either way; its result is recorded even when the tick is being cancelled.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	now := time.Now().UTC()
	if err != nil {
		reason := text.Truncate(err.Error(), maxErrorChars)
		if next, _, merr := r.mergeLedger(pctx, p.ID, led, func(l *Ledger) bool { return l.failRound(taskID, n, []string{StatusDispatching}, reason, now) }); merr != nil {
			log.Printf("[projects] %s recording the failure of task %s round %d: %v", p.ID, taskID, n, merr)
		} else {
			led = next
		}
		if ctx.Err() == nil {
			out.dispatchFailed = true
			out.fail("%s", reason)
		}
		return led, false
	}
	out.dispatched++
	next, applied, err := r.mergeLedger(pctx, p.ID, led, func(l *Ledger) bool { return l.roundRunning(taskID, n, st, now) })
	if err == nil && !applied {
		// The pod checked in first and the gateway recorded the session.
		if t := next.task(taskID); t != nil && t.Review != nil {
			if rd := t.Review.round(n); rd != nil && rd.SessionID == st.SessionID {
				applied = true
			}
		}
	}
	if err != nil || !applied {
		r.cluster.stopSession(pctx, st.SessionID)
		r.deleteBinding(pctx, st.SessionID)
		if err != nil {
			out.fail("recording the session of task %s round %d: %v", taskID, n, err)
			return led, false
		}
		return next, true
	}
	log.Printf("[projects] %s dispatched follow-up round %d of task %s (session %s)", p.ID, n, taskID, st.SessionID)
	return next, true
}

func shortSHA(sha string) string { return sha[:min(len(sha), 7)] }

const (
	commentMarker = "<!-- arbetern project comment -->"
	footerPrefix  = "_Posted by the arbetern project "
)

func projectFooter(p *Project) string {
	return footerPrefix + "\"" + cleanPRText(oneLine(p.Name, 80), 80) + "\"._\n" + commentMarker
}

func doneComment(p *Project, rv *ReviewState, head string) string {
	rounds := 0
	answered := 0
	for _, rd := range rv.Rounds {
		if rd.Status == RoundDone {
			rounds++
			answered += rd.Replies
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Ready for review.** Every check passes on `%s`", shortSHA(head))
	if rounds > 0 {
		fmt.Fprintf(&b, " and every review comment has an answer (%s across %s)", plural(answered, "reply"), plural(rounds, "follow-up round"))
	} else {
		b.WriteString(" and there are no review comments to answer")
	}
	if review := p.Review.effective(); review.AutoMerge && (rv.AutoMerge == nil || !rv.AutoMerge.Held) {
		fmt.Fprintf(&b, ". If nothing changes for %s, arbetern turns on auto-merge (%s), and it merges once a person approves it.\n\n", plural(review.AutoMergeDelayMinutes, "minute"), review.MergeMethod)
	} else {
		b.WriteString(". This change still needs a person's review before it merges.\n\n")
	}
	b.WriteString(projectFooter(p))
	return b.String()
}

func exhaustedComment(p *Project, rv *ReviewState, w prWatch, need []string) string {
	var left []string
	if slices.Contains(need, triggerFeedback) {
		left = append(left, plural(len(w.items), "unanswered review comment"))
	}
	if slices.Contains(need, triggerChecks) {
		names := make([]string, 0, len(w.failed))
		for _, c := range w.failed {
			names = append(names, "`"+strings.ReplaceAll(oneLine(c.Name, 100), "`", "'")+"`")
		}
		left = append(left, "failing checks "+strings.Join(names, ", "))
	}
	if slices.Contains(need, triggerConflict) {
		left = append(left, "a conflict with the base branch")
	}
	return fmt.Sprintf("**Stopping here.** The %s are used up and %s remain. Over to a person.\n\n%s",
		plural(rv.used(), "follow-up round"), strings.Join(left, " and "), projectFooter(p))
}

func plural(n int, noun string) string {
	switch {
	case n == 1:
		return "1 " + noun
	case len(noun) > 1 && noun[len(noun)-1] == 'y' && !strings.ContainsRune("aeiou", rune(noun[len(noun)-2])):
		return strconv.Itoa(n) + " " + strings.TrimSuffix(noun, "y") + "ies"
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func (r *Registry) deleteRoundBindings(ctx context.Context, led *Ledger, bound map[string][]int, now time.Time) map[string][]int {
	unbound := map[string][]int{}
	for id, ns := range bound {
		t := led.task(id)
		if t == nil || t.Review == nil {
			continue
		}
		for _, n := range ns {
			rd := t.Review.round(n)
			if rd == nil {
				continue
			}
			if _, running := sessionFinished(t, n); running {
				continue
			}
			if rd.Status == StatusFailed || isTerminal(t.Status) {
				r.cluster.stopSession(ctx, rd.SessionID)
			}
			if !sessionDone(t, n, rd.ExitReason, now) {
				continue
			}
			if r.deleteBinding(ctx, rd.SessionID) {
				unbound[id] = append(unbound[id], n)
			}
		}
	}
	return unbound
}
