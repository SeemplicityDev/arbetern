package projects

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/store"
	"github.com/justmike1/arbetern/internal/text"
)

type tickOutcome struct {
	errs           []string
	ledger         *Ledger
	scanned        int
	groups         int
	dispatched     int
	dispatchFailed bool
	interrupted    bool
	watches        []prWatch
	rounds         []roundWant
}

func (o *tickOutcome) fail(format string, args ...any) {
	o.errs = append(o.errs, fmt.Sprintf(format, args...))
}

func (o *tickOutcome) lastError() string {
	return oneLine(redact.Text(strings.Join(o.errs, "; ")), maxErrorChars)
}

func (r *Registry) tick(ctx context.Context, id, trigger string) {
	k := id
	p, err := r.fetchProject(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Printf("[projects] skip %s (%s): the project no longer exists", k, trigger)
		r.stopRunner(id)
		return
	case err != nil:
		log.Printf("[projects] skip %s (%s): reading the project: %v", k, trigger, err)
		return
	case !p.Enabled:
		log.Printf("[projects] skip %s (%s): the project is paused", k, trigger)
		return
	}
	if !r.tryBusy(k) {
		log.Printf("[projects] skip %s (%s): a tick is already running here", k, trigger)
		return
	}
	defer r.releaseBusy(k)
	lease := store.NewLease(r.b, "locks/projects/"+k, store.InstanceID(), leaseTTL)
	held, release, acquired, err := lease.Acquire(ctx)
	switch {
	case err != nil:
		log.Printf("[projects] %s tick lease unavailable, continuing without it: %v", k, err)
	case !acquired:
		log.Printf("[projects] skip %s (%s): ticking on another replica", k, trigger)
		return
	default:
		defer release()
		ctx = held
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()

	start := time.Now()
	out := r.runTick(ctx, p, trigger)
	// A shutdown or a lost lease cut the tick short; that says nothing about the project.
	out.interrupted = parent.Err() != nil
	r.recordTick(p, out)
	suffix := ""
	if e := out.lastError(); e != "" {
		suffix = ": " + e
	}
	log.Printf("[projects] tick %s (%s): %d events in %d groups, %d dispatched in %s%s",
		k, trigger, out.scanned, out.groups, out.dispatched, time.Since(start).Round(time.Millisecond), suffix)
}

func (r *Registry) runTick(ctx context.Context, p *Project, trigger string) *tickOutcome {
	out := &tickOutcome{}
	limits := p.Limits.effective()
	led, err := r.readLedger(ctx, p.ID)
	if err != nil {
		out.fail("reading the ledger: %v", err)
		return out
	}
	healthy := true
	if led, err = r.reconcileTasks(ctx, p, led, limits); err != nil {
		out.fail("reconciling tasks: %v", err)
		healthy = false
	} else if len(led.Tasks) >= maxTasks {
		r.pruneTranscripts(ctx, p.ID, led)
	}
	if led, err = r.syncPRs(ctx, p, led, limits, out); err != nil {
		out.fail("syncing pull requests: %v", err)
		healthy = false
	}
	led = r.closeMergedTickets(ctx, p, led)
	if r.deps.Datadog == nil {
		err = errors.New("nothing can be scanned: this deployment has no Datadog credentials")
	} else if p.Signal.metric() {
		led, err = r.scanMetrics(ctx, p, led, limits, out, trigger == "manual")
	} else {
		led, err = r.scan(ctx, p, led, limits, out)
	}
	if err != nil {
		out.fail("%v", err)
		healthy = false
	}
	if led, _, err = r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.resolveDue(limits) }); err != nil {
		out.fail("resolving merged fixes: %v", err)
		healthy = false
	}
	led = r.expireTasks(ctx, p, led, limits)
	if p.Review.FollowUp && len(out.watches) > 0 {
		led = r.advanceReviews(ctx, p, led, out)
	}
	out.ledger = led
	r.dispatch(ctx, p, limits, out, healthy)
	return out
}

func (r *Registry) reconcileTasks(ctx context.Context, p *Project, led *Ledger, limits Limits) (*Ledger, error) {
	now := time.Now().UTC()
	led, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.reconcile(limits, now) })
	if err != nil {
		return led, err
	}
	if present, finished, ok := r.cluster.sessionJobs(ctx, p.ID); ok {
		if led, _, err = r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.reconcileJobs(present, finished, now) }); err != nil {
			return led, err
		}
	}
	var unbound []string
	for i := range led.Tasks {
		t := &led.Tasks[i]
		if r.lostDispatch(t, now) {
			r.cluster.stopTask(ctx, t.ID)
		}
		if !t.Bound {
			continue
		}
		if _, running := sessionFinished(t, 0); running {
			continue
		}
		// The Job stops as soon as the work order failed; the binding stays until the pod has reported or the wrap-up passed.
		if t.Status == StatusFailed {
			r.cluster.stopSession(ctx, t.SessionID)
		}
		if !sessionDone(t, 0, t.ExitReason, now) {
			continue
		}
		if r.deleteBinding(ctx, t.SessionID) {
			unbound = append(unbound, t.ID)
		}
	}
	if len(unbound) > 0 {
		if led, _, err = r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.unbind(unbound) }); err != nil {
			return led, err
		}
	}
	rounds := r.deleteRoundBindings(ctx, led, led.boundRoundSessions(), now)
	if len(rounds) == 0 {
		return led, nil
	}
	led, _, err = r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.unbindRounds(rounds) })
	return led, err
}

// lostDispatch tells whether a task, or one of its rounds, was just failed for never recording its session; its Job, if any, is then still unknown to the ledger.
func (r *Registry) lostDispatch(t *Task, now time.Time) bool {
	recent := func(status, sessionID, finishedAt string) bool {
		return status == StatusFailed && sessionID == "" && now.Sub(parseTime(finishedAt)) < sessionWrapUp
	}
	if recent(t.Status, t.SessionID, t.FinishedAt) {
		return true
	}
	if t.Review == nil {
		return false
	}
	for _, rd := range t.Review.Rounds {
		if recent(rd.Status, rd.SessionID, rd.FinishedAt) {
			return true
		}
	}
	return false
}

func prCoordinates(p *Project, t *Task) (owner, repo string, number int) {
	if o, n, num, err := github.ParsePRURL(t.PR.URL); err == nil {
		return o, n, num
	}
	r := p.repoFor(t)
	return r.Owner, r.Name, t.PR.Number
}

func (r *Registry) syncPRs(ctx context.Context, p *Project, led *Ledger, limits Limits, out *tickOutcome) (*Ledger, error) {
	open := led.openPRTasks(maxPRSync)
	if len(open) == 0 {
		return led, nil
	}
	if r.gh == nil {
		return led, errors.New("GitHub is not configured")
	}
	var errs []error
	for _, t := range open {
		owner, repo, number := prCoordinates(p, &t)
		if repo == "" {
			continue
		}
		st, err := r.gh.PullRequestState(ctx, owner, repo, number)
		if github.IsNotFound(err) {
			led = r.unreadablePR(ctx, p, led, t, number)
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if t.PR.UnreadableSince != "" {
			if next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.clearUnreadable(t.ID) }); err == nil {
				led = next
			}
		}
		if st.State != "closed" {
			if p.Review.FollowUp {
				next, err := r.observePR(ctx, p, led, t, owner, repo, number, st, out)
				if err != nil {
					errs = append(errs, fmt.Errorf("following pull request #%d: %w", number, err))
				}
				led = next
			}
			continue
		}
		snap := prSnapshot{CreatedAt: st.CreatedAt, MergedAt: st.MergedAt, ClosedAt: st.ClosedAt, Additions: st.Additions, Deletions: st.Deletions}
		now := time.Now().UTC()
		transition := func(l *Ledger) bool { return l.closePR(t.ID, snap, limits, now) }
		if st.Merged {
			transition = func(l *Ledger) bool { return l.mergePR(t.ID, snap, now) }
		}
		next, changed, err := r.mergeLedger(ctx, p.ID, led, transition)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		led = next
		switch {
		case !changed:
		case st.Merged:
			log.Printf("[projects] %s task %s: pull request #%d merged", p.ID, t.ID, number)
			r.notify(p, noteClosed(&t, true))
		default:
			log.Printf("[projects] %s task %s: pull request #%d closed without merging", p.ID, t.ID, number)
			r.noteReviewFeedback(ctx, p, t.ID, owner, repo, number)
			r.notify(p, noteClosed(&t, false))
		}
	}
	return led, errors.Join(errs...)
}

func (r *Registry) expireTasks(ctx context.Context, p *Project, led *Ledger, limits Limits) *Ledger {
	keep := time.Duration(limits.TaskRetentionDays) * 24 * time.Hour
	now := time.Now().UTC()
	dropped := 0
	next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool {
		if dropped = l.expireTasks(keep, now); dropped > 0 {
			l.Swept = ""
		}
		return dropped > 0
	})
	if err != nil {
		log.Printf("[projects] %s dropping finished tasks: %v", p.ID, err)
		return led
	}
	if dropped > 0 {
		log.Printf("[projects] %s dropped %s finished more than %s ago", p.ID, plural(dropped, "task"), plural(limits.TaskRetentionDays, "day"))
	}
	// A daily sweep also catches transcripts left by a failed sweep or by the task cap.
	today := now.Format(time.DateOnly)
	if next.Swept == today || !r.pruneTranscripts(ctx, p.ID, next) {
		return next
	}
	swept, _, err := r.mergeLedger(ctx, p.ID, next, func(l *Ledger) bool {
		if l.Swept == today {
			return false
		}
		l.Swept = today
		return true
	})
	if err != nil {
		log.Printf("[projects] %s recording the transcript sweep: %v", p.ID, err)
		return next
	}
	return swept
}

func (r *Registry) closeMergedTickets(ctx context.Context, p *Project, led *Ledger) *Ledger {
	tc := p.Conventions.Ticket
	if tc == nil || !tc.DoneOnMerge || r.deps.Tickets == nil {
		return led
	}
	now := time.Now().UTC()
	for _, t := range led.mergedTickets(now, maxTicketCloses) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := r.deps.Tickets.ResolveTicket(cctx, t.Ticket)
		cancel()
		if err != nil {
			log.Printf("[projects] %s task %s: moving %s to Done: %v", p.ID, t.ID, t.Ticket, err)
			continue
		}
		next, changed, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.setTicketDone(t.ID, now) })
		if err != nil {
			log.Printf("[projects] %s task %s: recording that %s is done: %v", p.ID, t.ID, t.Ticket, err)
			continue
		}
		led = next
		if changed {
			log.Printf("[projects] %s task %s: %s is done", p.ID, t.ID, t.Ticket)
		}
	}
	return led
}

// A lapsed token authorization also answers 404, so a pull request is given up only after unreadablePRGrace.
func (r *Registry) unreadablePR(ctx context.Context, p *Project, led *Ledger, t Task, number int) *Ledger {
	now := time.Now().UTC()
	since := parseTime(t.PR.UnreadableSince)
	transition := func(l *Ledger) bool { return l.markUnreadable(t.ID, now) }
	if !since.IsZero() && now.Sub(since) > unreadablePRGrace {
		reason := fmt.Sprintf("pull request #%d has not been readable on GitHub since %s", number, t.PR.UnreadableSince)
		transition = func(l *Ledger) bool { return l.failTask(t.ID, []string{StatusPROpen}, reason, now) }
	}
	next, changed, err := r.mergeLedger(ctx, p.ID, led, transition)
	if err != nil {
		log.Printf("[projects] %s task %s: recording unreadable pull request #%d: %v", p.ID, t.ID, number, err)
		return led
	}
	if changed {
		log.Printf("[projects] %s task %s: pull request #%d is not readable on GitHub", p.ID, t.ID, number)
	}
	return next
}

func (r *Registry) noteReviewFeedback(ctx context.Context, p *Project, taskID, owner, repo string, number int) {
	fb, err := r.gh.PullRequestFeedback(ctx, owner, repo, number, r.tokenLogin(ctx))
	if err != nil {
		log.Printf("[projects] %s reading feedback on pull request #%d: %v", p.ID, number, err)
		return
	}
	body := feedbackNote(number, fb)
	if body == "" {
		return
	}
	n := Note{ID: "review-" + taskID, Text: body, Source: noteReview, Task: taskID, At: stamp(time.Now())}
	if _, _, err := r.mergeMemory(ctx, p.ID, func(m *Memory) bool { return m.add(n) }); err != nil {
		log.Printf("[projects] %s saving review feedback: %v", p.ID, err)
	}
}

func isBotLogin(login string) bool {
	return strings.HasSuffix(strings.ToLower(login), "[bot]") || strings.EqualFold(login, "copilot")
}

func feedbackNote(number int, fb *github.PRFeedback) string {
	if fb == nil {
		return ""
	}
	var parts []string
	add := func(c github.PRComment, label string) {
		body := oneLine(c.Body, 300)
		if body == "" || isBotLogin(c.Author) {
			return
		}
		who := cmp.Or(c.Author, "someone")
		if label != "" {
			who += " (" + label + ")"
		}
		parts = append(parts, who+": "+body)
	}
	for _, rv := range fb.Reviews {
		add(rv, strings.ToLower(strings.ReplaceAll(rv.State, "_", " ")))
	}
	for _, c := range fb.Comments {
		label := ""
		if c.Path != "" {
			label = "on " + c.Path
		}
		add(c, label)
	}
	if len(parts) == 0 {
		return ""
	}
	note := fmt.Sprintf("PR #%d was closed without merging. Feedback: %s", number, strings.Join(parts, " | "))
	return text.Truncate(redact.Text(note), maxNoteChars)
}

func (r *Registry) scan(ctx context.Context, p *Project, led *Ledger, limits Limits, out *tickOutcome) (*Ledger, error) {
	now := time.Now().UTC()
	var groupBy *regexp.Regexp
	if p.Signal.GroupPattern != "" {
		re, err := regexp.Compile(p.Signal.GroupPattern)
		if err != nil {
			return led, fmt.Errorf("signal.group_pattern: %w", err)
		}
		groupBy = re
	}
	var groups, sightings []Group
	seen := map[string]int{}
	next := map[string]time.Time{}
	var failed []error
	read := false
	// Each site keeps its own cursor, so a site that fails stays where it was while the others move on.
	for _, site := range p.Signal.sites() {
		since, until := scanWindow(led.siteCursor(site), p.Signal.lookback(), now)
		next[site] = since
		if !since.Before(until) {
			continue
		}
		client := r.datadogFor(site)
		if client == nil {
			failed = append(failed, fmt.Errorf("this deployment has no Datadog credentials for site %s", site))
			continue
		}
		res, err := client.ErrorGroupsBy(ctx, p.Signal.Query, since, until, scanMaxEvents, scanMaxSamples, groupBy)
		if err != nil {
			failed = append(failed, fmt.Errorf("scanning Datadog %s: %w", strings.ToUpper(site), err))
			continue
		}
		kept, err := filterGroups(res.Groups, p.Signal.Pattern)
		if err != nil {
			return led, fmt.Errorf("signal.pattern: %w", err)
		}
		read = true
		for _, g := range kept {
			ng := groupFromDatadog(g)
			ng.Site = site
			sightings = append(sightings, ng)
			if i, ok := seen[ng.Fingerprint]; ok {
				groups[i] = mergeSiteGroups(groups[i], ng)
				continue
			}
			seen[ng.Fingerprint] = len(groups)
			groups = append(groups, ng)
		}
		if res.Partial {
			log.Printf("[projects] %s: Datadog %s reported a partial result for %s to %s", p.ID, strings.ToUpper(site), since.Format(time.RFC3339), until.Format(time.RFC3339))
		}
		out.scanned += res.Scanned
		siteNext := res.Next
		if siteNext.IsZero() || siteNext.After(until) {
			siteNext = until
		}
		if res.Truncated && !catchesUp(since, siteNext, until, p.interval()) {
			log.Printf("[projects] %s: the %s scan stopped after %d events, so the logs from %s to %s are skipped", p.ID, strings.ToUpper(site), res.Scanned, siteNext.Format(time.RFC3339), until.Format(time.RFC3339))
			siteNext = until
		}
		next[site] = siteNext
	}
	if !read {
		return led, errors.Join(failed...)
	}
	out.groups = len(groups)
	from, signal := led.scanFrom(), p.Signal.key()
	updated, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.applyScan(from, signal, groups, sightings, next, limits, now) })
	if err != nil {
		return led, fmt.Errorf("recording the scan: %w", err)
	}
	return updated, errors.Join(failed...)
}

func (r *Registry) dispatch(ctx context.Context, p *Project, limits Limits, out *tickOutcome, newWork bool) {
	if !newWork && len(out.rounds) == 0 {
		return
	}
	if r.gh == nil {
		out.fail("GitHub is not configured, so no sessions are dispatched")
		return
	}
	// An open pull request is closer to done than new work, so its rounds go first.
	if len(out.rounds) > 0 {
		r.dispatchRounds(ctx, p, limits, out)
	}
	if !newWork || out.dispatchFailed {
		return
	}
	if p.Repo.org() {
		if repos, err := r.scopeRepos(ctx, p.Repo); err != nil || len(repos) == 0 {
			out.fail("no session is dispatched: %v", cmp.Or(err, errors.New("repo.include matches none of the repositories arbetern can see")))
			return
		}
	}
	led, signal := out.ledger, p.Signal.key()
	for {
		now := time.Now().UTC()
		if !led.hasCapacity(limits, now) {
			break
		}
		cand, ok := led.pick(signal, limits, now)
		if !ok || (cand.Group.Reason == reasonFill && !led.fillCapacity(limits, now)) {
			break
		}
		group, err := r.evidence(ctx, p, cand.Group, now)
		if err != nil {
			out.fail("fetching the evidence for %s: %v", groupLabel(cand.Group), err)
			break
		}
		taskID, err := store.NewID()
		if err != nil {
			out.fail("creating a task id: %v", err)
			break
		}
		draft := Task{ID: taskID, Fingerprint: cand.Group.Fingerprint, Group: group}
		next, _, err := r.mergeLedger(ctx, p.ID, led, func(l *Ledger) bool { return l.addTask(draft, limits, now) })
		if err != nil {
			out.fail("recording a task: %v", err)
			break
		}
		led = next
		if t := led.task(taskID); t == nil || t.Status != StatusDispatching {
			break
		}
		var fired bool
		led, fired = r.fireTask(ctx, p, led, taskID, out)
		if !fired {
			break
		}
	}
	out.ledger = led
}

// fireTask reports whether dispatching may continue.
func (r *Registry) fireTask(ctx context.Context, p *Project, led *Ledger, taskID string, out *tickOutcome) (*Ledger, bool) {
	var st *started
	err := errors.New("the task is no longer in the ledger")
	if t := led.task(taskID); t != nil {
		st, err = r.launch(ctx, p, t, 0)
	}
	// The Job may have started either way; its result is recorded even when the tick is being cancelled.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	now := time.Now().UTC()
	if err != nil {
		reason := text.Truncate(err.Error(), maxErrorChars)
		if next, _, merr := r.mergeLedger(pctx, p.ID, led, func(l *Ledger) bool { return l.failTask(taskID, []string{StatusDispatching}, reason, now) }); merr != nil {
			log.Printf("[projects] %s recording the failure of task %s: %v", p.ID, taskID, merr)
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
	next, applied, err := r.mergeLedger(pctx, p.ID, led, func(l *Ledger) bool { return l.markRunning(taskID, st, now) })
	if err == nil && !applied {
		// The pod checked in first and the gateway recorded the session.
		if t := next.task(taskID); t != nil && t.SessionID == st.SessionID {
			applied = true
		}
	}
	if err != nil || !applied {
		r.cluster.stopSession(pctx, st.SessionID)
		r.deleteBinding(pctx, st.SessionID)
		if err != nil {
			out.fail("recording the session of task %s: %v", taskID, err)
			return led, false
		}
		log.Printf("[projects] %s task %s was no longer dispatching when its session started", p.ID, taskID)
		return next, true
	}
	log.Printf("[projects] %s dispatched task %s (session %s)", p.ID, taskID, st.SessionID)
	return next, true
}

func (r *Registry) recordTick(p *Project, out *tickOutcome) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	now := time.Now().UTC()
	lastErr := out.lastError()
	var stats *Stats
	if out.ledger != nil {
		s := computeStats(out.ledger, p, now)
		stats = &s
	}
	autoDisabled, reason := false, ""
	_, err := r.updateDescriptor(ctx, p.ID, func(d *Project) error {
		d.LastTick = stamp(now)
		if !out.interrupted {
			d.LastError = lastErr
		}
		if stats != nil {
			d.Stats = *stats
		}
		switch {
		case out.dispatchFailed:
			d.ConsecutiveFailures++
		case out.dispatched > 0:
			d.ConsecutiveFailures = 0
		}
		autoDisabled = d.Enabled && d.ConsecutiveFailures >= MaxConsecutiveFailures
		if autoDisabled {
			d.Enabled = false
			d.DisabledReason = fmt.Sprintf("auto-disabled after %d consecutive failed dispatches. Last error: %s", d.ConsecutiveFailures, d.LastError)
			reason = d.DisabledReason
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Deleted while this tick ran; its ledger writes may have recreated state nobody owns.
		r.deleteState(ctx, p.ID)
	case err != nil:
		log.Printf("[projects] persisting the tick of %s: %v", p.ID, err)
	case autoDisabled:
		log.Printf("[projects] AUTO-DISABLED %s after %d consecutive failed dispatches: %s", p.ID, MaxConsecutiveFailures, lastErr)
		r.stopRunner(p.ID)
		r.notify(p, notePaused(p, reason))
	}
	r.touch(p.ID)
}
