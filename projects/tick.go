package projects

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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
	nextDispatch   time.Time
	interrupted    bool
}

func (o *tickOutcome) fail(format string, args ...any) {
	o.errs = append(o.errs, fmt.Sprintf(format, args...))
}

func (o *tickOutcome) lastError() string {
	return oneLine(redact.Text(strings.Join(o.errs, "; ")), maxErrorChars)
}

func (r *Registry) tick(ctx context.Context, agent, id, trigger string) {
	k := key(agent, id)
	p, err := r.fetchProject(ctx, agent, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Printf("[projects] skip %s (%s): the project no longer exists", k, trigger)
		r.stopRunner(agent, id)
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
	out := r.runTick(ctx, p)
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

func (r *Registry) runTick(ctx context.Context, p *Project) *tickOutcome {
	out := &tickOutcome{}
	limits := p.Limits.effective()
	led, err := r.readLedger(ctx, p.Agent, p.ID)
	if err != nil {
		out.fail("reading the ledger: %v", err)
		return out
	}
	healthy := true
	if led, err = r.reconcileTasks(ctx, p, led, limits); err != nil {
		out.fail("reconciling tasks: %v", err)
		healthy = false
	}
	if led, err = r.syncPRs(ctx, p, led, limits); err != nil {
		out.fail("syncing pull requests: %v", err)
		healthy = false
	}
	if led, err = r.scan(ctx, p, led, limits, out); err != nil {
		out.fail("%v", err)
		healthy = false
	}
	if led, _, err = r.mergeLedger(ctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.resolveDue(limits) }); err != nil {
		out.fail("resolving merged fixes: %v", err)
		healthy = false
	}
	out.ledger = led
	if healthy {
		r.dispatch(ctx, p, limits, out)
	}
	return out
}

func (r *Registry) reconcileTasks(ctx context.Context, p *Project, led *Ledger, limits Limits) (*Ledger, error) {
	now := time.Now().UTC()
	led, _, err := r.mergeLedger(ctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.reconcile(limits, now) })
	if err != nil {
		return led, err
	}
	var unbound []string
	for _, t := range led.Tasks {
		if t.Bound && isTerminal(t.Status) && r.deleteBinding(ctx, t.SessionID) {
			unbound = append(unbound, t.ID)
		}
	}
	if len(unbound) == 0 {
		return led, nil
	}
	led, _, err = r.mergeLedger(ctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.unbind(unbound) })
	return led, err
}

func prCoordinates(p *Project, pr *PR) (owner, repo string, number int) {
	if o, n, num, err := github.ParsePRURL(pr.URL); err == nil {
		return o, n, num
	}
	return p.Repo.Owner, p.Repo.Name, pr.Number
}

func (r *Registry) syncPRs(ctx context.Context, p *Project, led *Ledger, limits Limits) (*Ledger, error) {
	open := led.openPRTasks(maxPRSync)
	if len(open) == 0 {
		return led, nil
	}
	if r.gh == nil {
		return led, errors.New("GitHub is not configured")
	}
	var errs []error
	for _, t := range open {
		owner, repo, number := prCoordinates(p, t.PR)
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
			if next, _, err := r.mergeLedger(ctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.clearUnreadable(t.ID) }); err == nil {
				led = next
			}
		}
		if st.State != "closed" {
			continue
		}
		snap := prSnapshot{CreatedAt: st.CreatedAt, MergedAt: st.MergedAt, ClosedAt: st.ClosedAt, Additions: st.Additions, Deletions: st.Deletions}
		now := time.Now().UTC()
		transition := func(l *Ledger) bool { return l.closePR(t.ID, snap, limits, now) }
		if st.Merged {
			transition = func(l *Ledger) bool { return l.mergePR(t.ID, snap, now) }
		}
		next, changed, err := r.mergeLedger(ctx, p.Agent, p.ID, led, transition)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		led = next
		switch {
		case !changed:
		case st.Merged:
			log.Printf("[projects] %s/%s task %s: pull request #%d merged", p.Agent, p.ID, t.ID, number)
		default:
			log.Printf("[projects] %s/%s task %s: pull request #%d closed without merging", p.Agent, p.ID, t.ID, number)
			r.noteReviewFeedback(ctx, p, t.ID, owner, repo, number)
		}
	}
	return led, errors.Join(errs...)
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
	next, changed, err := r.mergeLedger(ctx, p.Agent, p.ID, led, transition)
	if err != nil {
		log.Printf("[projects] %s/%s task %s: recording unreadable pull request #%d: %v", p.Agent, p.ID, t.ID, number, err)
		return led
	}
	if changed {
		log.Printf("[projects] %s/%s task %s: pull request #%d is not readable on GitHub", p.Agent, p.ID, t.ID, number)
	}
	return next
}

func (r *Registry) noteReviewFeedback(ctx context.Context, p *Project, taskID, owner, repo string, number int) {
	fb, err := r.gh.PullRequestFeedback(ctx, owner, repo, number, r.tokenLogin(ctx))
	if err != nil {
		log.Printf("[projects] %s/%s reading feedback on pull request #%d: %v", p.Agent, p.ID, number, err)
		return
	}
	body := feedbackNote(number, fb)
	if body == "" {
		return
	}
	n := Note{ID: "review-" + taskID, Text: body, Source: noteReview, Task: taskID, At: stamp(time.Now())}
	if _, _, err := r.mergeMemory(ctx, p.Agent, p.ID, func(m *Memory) bool { return m.add(n) }); err != nil {
		log.Printf("[projects] %s/%s saving review feedback: %v", p.Agent, p.ID, err)
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
	since, until := scanWindow(led.Cursor, p.Signal.lookback(), now)
	if !since.Before(until) {
		return led, nil
	}
	if r.deps.AgentAllowed != nil && !r.deps.AgentAllowed(p.Agent) {
		return led, fmt.Errorf("agent %s may not use Datadog", p.Agent)
	}
	client := r.datadogFor(p.Agent, p.Signal.Site)
	if client == nil {
		return led, fmt.Errorf("agent %s has no Datadog credentials for site %s", p.Agent, p.Signal.Site)
	}
	res, err := client.ErrorGroups(ctx, p.Signal.Query, since, until, scanMaxEvents, scanMaxSamples)
	if err != nil {
		return led, fmt.Errorf("scanning Datadog: %w", err)
	}
	kept, err := filterGroups(res.Groups, p.Signal.Pattern)
	if err != nil {
		return led, fmt.Errorf("signal.pattern: %w", err)
	}
	groups := make([]Group, 0, len(kept))
	for _, g := range kept {
		groups = append(groups, groupFromDatadog(g))
	}
	if res.Partial {
		log.Printf("[projects] %s/%s: Datadog reported a partial result for %s to %s", p.Agent, p.ID, since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	out.scanned, out.groups = res.Scanned, len(groups)
	next := res.Next
	if next.IsZero() || next.After(until) {
		next = until
	}
	if res.Truncated && !catchesUp(since, next, until, p.interval()) {
		log.Printf("[projects] %s/%s: the scan stopped after %d events, so the logs from %s to %s are skipped", p.Agent, p.ID, res.Scanned, next.Format(time.RFC3339), until.Format(time.RFC3339))
		next = until
	}
	from, signal := led.Cursor, p.Signal.key()
	updated, _, err := r.mergeLedger(ctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.applyScan(from, signal, groups, next, limits, now) })
	if err != nil {
		return led, fmt.Errorf("recording the scan: %w", err)
	}
	return updated, nil
}

func (r *Registry) dispatch(ctx context.Context, p *Project, limits Limits, out *tickOutcome) {
	if parseTime(p.NextDispatchAfter).After(time.Now()) {
		return
	}
	if r.gh == nil {
		out.fail("GitHub is not configured, so no sessions are dispatched")
		return
	}
	token := routineToken(p.Dispatch.Token)
	switch {
	case p.Dispatch.RoutineID == "":
		out.fail("no routine is configured")
		return
	case token == "":
		out.fail("the trigger token %q is not configured (set %s)", p.Dispatch.Token, TokenEnv(p.Dispatch.Token))
		return
	}
	led, signal := out.ledger, p.Signal.key()
	for {
		now := time.Now().UTC()
		if !led.hasCapacity(limits, now) {
			break
		}
		cand, ok := led.pick(signal, limits, now)
		if !ok {
			break
		}
		taskID, err := store.NewID()
		if err != nil {
			out.fail("creating a task id: %v", err)
			break
		}
		draft := Task{ID: taskID, Fingerprint: cand.Group.Fingerprint}
		next, _, err := r.mergeLedger(ctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.addTask(draft, limits, now) })
		if err != nil {
			out.fail("recording a task: %v", err)
			break
		}
		led = next
		if t := led.task(taskID); t == nil || t.Status != StatusDispatching {
			break
		}
		var fired bool
		led, fired = r.fireTask(ctx, p, led, taskID, token, out)
		if !fired {
			break
		}
	}
	out.ledger = led
}

func classifyFire(err error) (reason string, rateLimited bool, retryAfter time.Duration) {
	var fe *FireError
	if errors.As(err, &fe) {
		if fe.Status == http.StatusTooManyRequests {
			return "rate limited", true, fe.RetryAfter
		}
		return text.Truncate(fe.Error(), maxErrorChars), false, 0
	}
	return text.Truncate("routine fire failed: "+err.Error(), maxErrorChars), false, 0
}

// fireTask reports whether dispatching may continue.
func (r *Registry) fireTask(ctx context.Context, p *Project, led *Ledger, taskID, token string, out *tickOutcome) (*Ledger, bool) {
	fired, ferr := r.fire.Fire(ctx, p.Dispatch.RoutineID, token, fireText(p.Agent, p.ID, taskID))
	// The fire has happened either way; its result is recorded even when the tick is being cancelled.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	now := time.Now().UTC()
	fail := func(from, reason string) {
		next, _, err := r.mergeLedger(pctx, p.Agent, p.ID, led, func(l *Ledger) bool { return l.failTask(taskID, []string{from}, reason, now) })
		if err != nil {
			log.Printf("[projects] %s/%s recording the failure of task %s: %v", p.Agent, p.ID, taskID, err)
			return
		}
		led = next
	}
	if ferr != nil && ctx.Err() != nil {
		fail(StatusDispatching, "dispatch interrupted before the routine answered")
		return led, false
	}
	if ferr != nil {
		reason, rateLimited, retryAfter := classifyFire(ferr)
		fail(StatusDispatching, reason)
		if rateLimited {
			out.nextDispatch = now.Add(max(retryAfter, minRetryAfter))
			log.Printf("[projects] %s/%s routine fire rate limited; dispatching resumes after %s", p.Agent, p.ID, out.nextDispatch.Format(time.RFC3339))
			return led, false
		}
		out.dispatchFailed = true
		out.fail("%s", reason)
		return led, false
	}
	out.dispatched++
	next, applied, err := r.mergeLedger(pctx, p.Agent, p.ID, led, func(l *Ledger) bool {
		return l.markRunning(taskID, fired.SessionID, fired.SessionURL, now)
	})
	if err != nil {
		out.fail("recording the session of task %s: %v", taskID, err)
		return led, false
	}
	led = next
	if !applied {
		log.Printf("[projects] %s/%s task %s was no longer dispatching when its session started", p.Agent, p.ID, taskID)
		return led, true
	}
	b := binding{Agent: p.Agent, Project: p.ID, Task: taskID, CreatedAt: stamp(now)}
	if err := r.writeBinding(pctx, fired.SessionID, b); err != nil {
		fail(StatusRunning, "the session binding could not be stored")
		out.fail("storing the binding of task %s: %v", taskID, err)
		return led, false
	}
	log.Printf("[projects] %s/%s dispatched task %s (session %s)", p.Agent, p.ID, taskID, fired.SessionID)
	return led, true
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
	autoDisabled := false
	_, err := r.updateDescriptor(ctx, p.Agent, p.ID, func(d *Project) error {
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
		switch {
		case !out.nextDispatch.IsZero():
			d.NextDispatchAfter = stamp(out.nextDispatch)
		case d.NextDispatchAfter != "" && !parseTime(d.NextDispatchAfter).After(now):
			d.NextDispatchAfter = ""
		}
		autoDisabled = d.Enabled && d.ConsecutiveFailures >= MaxConsecutiveFailures
		if autoDisabled {
			d.Enabled = false
			d.DisabledReason = fmt.Sprintf("auto-disabled after %d consecutive failed dispatches. Last error: %s", d.ConsecutiveFailures, d.LastError)
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Deleted while this tick ran; its ledger writes may have recreated state nobody owns.
		r.deleteState(ctx, p.Agent, p.ID)
	case err != nil:
		log.Printf("[projects] persisting the tick of %s/%s: %v", p.Agent, p.ID, err)
	case autoDisabled:
		log.Printf("[projects] AUTO-DISABLED %s/%s after %d consecutive failed dispatches: %s", p.Agent, p.ID, MaxConsecutiveFailures, lastErr)
		r.stopRunner(p.Agent, p.ID)
	}
	r.touch(p.Agent, p.ID)
}
