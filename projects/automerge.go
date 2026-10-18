package projects

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
)

const (
	mergeStateClean    = "clean"
	mergeStateHasHooks = "has_hooks"
	mergeStateBehind   = "behind"

	maxMergeNoteChars = 300
	branchUpdateRetry = time.Hour
	heldNote          = "Auto-merge was turned off outside arbetern, so arbetern leaves it off."
)

// MergeState is arbetern's auto-merge progress on one pull request.
type MergeState struct {
	Snapshot      string   `json:"snapshot,omitempty"` // head, checks and discussion when the delay started
	ArmedAt       string   `json:"armed_at,omitempty"` // when the delay started
	On            bool     `json:"on,omitempty"`       // GitHub's auto-merge was on at the last read
	OnHead        string   `json:"on_head,omitempty"`  // the head and base auto-merge went on for
	OnBase        string   `json:"on_base,omitempty"`
	EnabledAt     string   `json:"enabled_at,omitempty"`      // when arbetern turned it on
	Withdrawing   bool     `json:"withdrawing,omitempty"`     // arbetern is turning it off
	MergedAt      string   `json:"merged_at,omitempty"`       // when arbetern merged the pull request itself
	Note          string   `json:"note,omitempty"`            // why auto-merge is not on
	Warned        string   `json:"warned,omitempty"`          // the note the Slack channel last heard; a note goes there once it repeats
	Held          bool     `json:"held,omitempty"`            // turned off outside arbetern, which leaves it off
	UpdateTriedAt string   `json:"update_tried_at,omitempty"` // when arbetern last asked GitHub to bring this head up to date
	UpdatedFrom   string   `json:"updated_from,omitempty"`    // the head GitHub accepted that request for
	Lineage       []string `json:"lineage,omitempty"`         // earlier heads that differ from the current one only by arbetern's branch updates
}

func (m *MergeState) headMoved(from string, updated bool) {
	if m == nil {
		return
	}
	if updated {
		m.Lineage = append(m.Lineage, from)
	} else {
		m.Lineage = nil
	}
	m.UpdatedFrom, m.UpdateTriedAt, m.Snapshot, m.ArmedAt = "", "", "", ""
	if !m.Held {
		m.Note = ""
	}
}

func (m *MergeState) disarm() {
	if m != nil {
		m.Snapshot, m.ArmedAt, m.UpdatedFrom, m.UpdateTriedAt = "", "", "", ""
	}
}

func contentHeads(m *MergeState, head string) []string {
	if m == nil {
		return []string{head}
	}
	return append(slices.Clone(m.Lineage), head)
}

func mergeSnapshot(pr *github.PRState, checks []github.Check, discussion []github.DiscussionItem, self, method string) string {
	head := []string{pr.HeadSHA, pr.BaseRef, strconv.FormatBool(pr.Draft), method, pr.Title, pr.Body}
	ci := make([]string, 0, len(checks))
	for _, c := range checks {
		ci = append(ci, strings.Join([]string{strconv.FormatInt(c.ID, 10), c.Name, c.Status, c.Conclusion}, "\x1f"))
	}
	var talk []string
	for _, it := range discussion {
		mine := strings.EqualFold(it.Author, self)
		// An approval is what auto-merge waits for, so it does not restart the delay.
		if mine && (it.Kind != github.DiscussionComment || ownComment(it.Body)) || it.Kind == github.DiscussionReview && it.State == "APPROVED" {
			continue
		}
		talk = append(talk, strings.Join([]string{it.Kind, strconv.FormatInt(it.ID, 10), it.State, laterOf(it.CreatedAt, it.UpdatedAt).Format(time.RFC3339Nano)}, "\x1f"))
	}
	slices.Sort(ci)
	slices.Sort(talk)
	return snapshotPart(head) + "." + snapshotPart(ci) + "." + snapshotPart(talk)
}

func snapshotPart(parts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

func snapshotChange(before, after string) string {
	a, b := strings.Split(before, "."), strings.Split(after, ".")
	if len(a) != 3 || len(b) != 3 {
		return "the pull request"
	}
	var parts []string
	for i, name := range []string{"head", "checks", "discussion"} {
		if a[i] != b[i] {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, ", ")
}

func approvers(reviews []github.DiscussionItem, heads []string) []string {
	latest := map[string]github.DiscussionItem{}
	var order []string
	for _, it := range reviews {
		if it.Bot || !it.Trusted() || it.State != "APPROVED" && it.State != "CHANGES_REQUESTED" && it.State != "DISMISSED" {
			continue
		}
		k := strings.ToLower(it.Author)
		if _, seen := latest[k]; !seen {
			order = append(order, k)
		}
		latest[k] = it
	}
	var out []string
	for _, k := range order {
		switch it := latest[k]; {
		case it.State == "CHANGES_REQUESTED":
			return nil
		case it.State == "APPROVED" && slices.Contains(heads, it.CommitID):
			out = append(out, it.Author)
		}
	}
	return out
}

func (r *Registry) approver(ctx context.Context, w prWatch, heads []string) string {
	for _, who := range approvers(w.reviews, heads) {
		ok, err := r.gh.CanPush(ctx, w.owner, w.repo, who)
		if err != nil {
			log.Printf("[projects] task %s: checking whether %s may approve pull request #%d: %v", w.taskID, who, w.number, err)
			continue
		}
		if ok {
			return who
		}
	}
	return ""
}

func mergeReady(rv *ReviewState, w prWatch) bool {
	return rv.Phase == PhaseDone && rv.DoneHead == w.pr.HeadSHA && w.ci == ciPassed && !rv.Conflict && !w.pr.Draft
}

func mergeHold(rv *ReviewState, w prWatch, need []string) bool {
	return len(need) > 0 || len(w.items) > 0 || rv.Waiting != "" || rv.Conflict || len(w.failed) > 0 && rv.ChecksHandled != w.pr.HeadSHA
}

func contentMoved(m *MergeState, w prWatch) bool {
	return m.OnHead != "" && (!slices.Contains(contentHeads(m, w.pr.HeadSHA), m.OnHead) || m.OnBase != w.pr.BaseRef)
}

func (l *Ledger) armMerge(id, snapshot string, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil {
		return false
	}
	m := cmp.Or(rv.AutoMerge, &MergeState{})
	if m.Held || m.On || m.Withdrawing || m.MergedAt != "" || m.Snapshot == snapshot && m.ArmedAt != "" {
		return false
	}
	m.Snapshot, m.ArmedAt = snapshot, stamp(now)
	rv.AutoMerge = m
	return true
}

func (l *Ledger) claimMerge(id, head, snapshot string) bool {
	_, rv := l.review(id)
	if rv == nil || rv.active() != nil || rv.Phase != PhaseDone || rv.Head != head || rv.DoneHead != head || rv.AutoMerge == nil {
		return false
	}
	m := rv.AutoMerge
	if m.Held || m.On || m.Withdrawing || m.MergedAt != "" || m.ArmedAt == "" || m.Snapshot != snapshot {
		return false
	}
	m.ArmedAt = ""
	return true
}

func (l *Ledger) mergeWaits(id, snapshot, note string, warn bool, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil || rv.AutoMerge == nil || rv.AutoMerge.Snapshot != snapshot {
		return false
	}
	rv.AutoMerge.ArmedAt = stamp(now)
	rv.AutoMerge.setNote(note, warn)
	return true
}

func (m *MergeState) setNote(note string, warn bool) bool {
	changed := false
	if warn && m.Note == note && m.Warned != note {
		m.Warned, changed = note, true
	}
	if m.Note != note {
		m.Note, changed = note, true
	}
	return changed
}

func (l *Ledger) mergeOn(id, head, base string, enabled bool, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil {
		return false
	}
	m := cmp.Or(rv.AutoMerge, &MergeState{})
	if m.On && !enabled {
		return false
	}
	m.On, m.OnHead, m.OnBase, m.Held, m.ArmedAt = true, head, base, false, ""
	if enabled {
		m.EnabledAt, m.Note = stamp(now), ""
	}
	rv.AutoMerge = m
	return true
}

func (l *Ledger) withdrawMerge(id string) bool {
	_, rv := l.review(id)
	if rv == nil {
		return false
	}
	m := cmp.Or(rv.AutoMerge, &MergeState{})
	if m.Withdrawing {
		return false
	}
	m.Withdrawing, m.Held = true, false
	rv.AutoMerge = m
	return true
}

func (l *Ledger) mergeOff(id string, held bool) bool {
	_, rv := l.review(id)
	if rv == nil || rv.AutoMerge == nil {
		return false
	}
	m := rv.AutoMerge
	changed := m.On || m.Withdrawing || m.EnabledAt != "" || m.Snapshot != "" || m.ArmedAt != "" || held && !m.Held
	m.On, m.OnHead, m.OnBase, m.EnabledAt, m.Withdrawing, m.Snapshot, m.ArmedAt = false, "", "", "", false, "", ""
	if held {
		m.Held, m.Note = true, heldNote
	} else if !m.Held {
		m.Note = ""
	}
	return changed
}

func (l *Ledger) claimUpdate(id, head string, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil || rv.active() != nil || rv.Head != head || rv.AutoMerge == nil {
		return false
	}
	m := rv.AutoMerge
	if m.Held || m.Withdrawing || now.Sub(parseTime(m.UpdateTriedAt)) < branchUpdateRetry {
		return false
	}
	m.UpdateTriedAt = stamp(now)
	return true
}

func (l *Ledger) branchUpdated(id, head string) bool {
	_, rv := l.review(id)
	if rv == nil || rv.AutoMerge == nil || rv.Head != head || rv.AutoMerge.UpdatedFrom == head {
		return false
	}
	rv.AutoMerge.UpdatedFrom = head
	return true
}

func (l *Ledger) noteMerge(id, note string, warn bool) bool {
	_, rv := l.review(id)
	if rv == nil || rv.AutoMerge == nil {
		return false
	}
	return rv.AutoMerge.setNote(note, warn)
}

func (l *Ledger) mergedByArbetern(id string, now time.Time) bool {
	_, rv := l.review(id)
	if rv == nil || rv.AutoMerge == nil || rv.AutoMerge.MergedAt != "" {
		return false
	}
	rv.AutoMerge.MergedAt, rv.AutoMerge.ArmedAt, rv.AutoMerge.Note = stamp(now), "", ""
	return true
}

// followAutoMerge reports false when auto-merge could not be turned off, so nothing else may act on the pull request this tick.
func (r *Registry) followAutoMerge(ctx context.Context, p *Project, led *Ledger, w prWatch, review Review, need []string, now time.Time) (*Ledger, bool) {
	k := p.ID
	_, rv := led.review(w.taskID)
	m := cmp.Or(rv.AutoMerge, &MergeState{})
	var change func(l *Ledger) bool
	switch {
	// A person's own auto-merge is theirs to keep, except while a round changes the pull request.
	case w.pr.AutoMerge && (m.Withdrawing || w.ourAutoMerge && (!review.AutoMerge || mergeHold(rv, w, need) || contentMoved(m, w)) || review.AutoMerge && len(need) > 0):
		return r.withdrawAutoMerge(ctx, p, led, w)
	case !w.pr.AutoMerge && m.Withdrawing:
		change = func(l *Ledger) bool { return l.mergeOff(w.taskID, false) }
	case m.On && !w.pr.AutoMerge:
		log.Printf("[projects] %s task %s: auto-merge on pull request #%d was turned off outside arbetern, which leaves it off", k, w.taskID, w.number)
		change = func(l *Ledger) bool { return l.mergeOff(w.taskID, true) }
	case review.AutoMerge && w.pr.AutoMerge && !m.On:
		change = func(l *Ledger) bool { return l.mergeOn(w.taskID, w.pr.HeadSHA, w.pr.BaseRef, false, now) }
	default:
		return led, true
	}
	next, _, err := r.mergeLedger(ctx, k, led, change)
	if err != nil {
		log.Printf("[projects] %s task %s: recording the auto-merge state of pull request #%d: %v", k, w.taskID, w.number, err)
		return led, true
	}
	return next, true
}

func (r *Registry) withdrawAutoMerge(ctx context.Context, p *Project, led *Ledger, w prWatch) (*Ledger, bool) {
	k := p.ID
	// Recorded first, so a failed call is retried and a lost record is not taken for someone else turning it off.
	next, _, err := r.mergeLedger(ctx, k, led, func(l *Ledger) bool { return l.withdrawMerge(w.taskID) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording that auto-merge on pull request #%d goes off: %v", k, w.taskID, w.number, err)
		return led, false
	}
	if err := r.gh.DisablePullRequestAutoMerge(ctx, w.pr.NodeID); err != nil {
		log.Printf("[projects] %s task %s: turning off auto-merge on pull request #%d: %v", k, w.taskID, w.number, err)
		return r.noteMerge(ctx, p, next, w, "arbetern could not turn off auto-merge, so nothing else happens on this pull request until it can: "+github.APIMessage(err), true), false
	}
	log.Printf("[projects] %s task %s: auto-merge off on pull request #%d", k, w.taskID, w.number)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	done, _, err := r.mergeLedger(pctx, k, next, func(l *Ledger) bool { return l.mergeOff(w.taskID, false) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording auto-merge off on pull request #%d: %v", k, w.taskID, w.number, err)
		return next, true
	}
	return done, true
}

func (r *Registry) autoMerge(ctx context.Context, p *Project, led *Ledger, w prWatch, review Review, now time.Time) *Ledger {
	k := p.ID
	_, rv := led.review(w.taskID)
	m := cmp.Or(rv.AutoMerge, &MergeState{})
	switch {
	case m.Held || m.Withdrawing || m.MergedAt != "":
		return led
	case w.pr.AutoMerge:
		return r.keepUpToDate(ctx, p, led, w, review, now)
	case m.Snapshot != w.snapshot || m.ArmedAt == "":
		next, armed, err := r.mergeLedger(ctx, k, led, func(l *Ledger) bool { return l.armMerge(w.taskID, w.snapshot, now) })
		if err != nil {
			log.Printf("[projects] %s task %s: starting the auto-merge delay of pull request #%d: %v", k, w.taskID, w.number, err)
			return led
		}
		switch {
		case !armed:
		case m.Snapshot != "" && m.Snapshot != w.snapshot:
			log.Printf("[projects] %s task %s: pull request #%d changed during the auto-merge delay (%s); the delay starts over", k, w.taskID, w.number, snapshotChange(m.Snapshot, w.snapshot))
		default:
			log.Printf("[projects] %s task %s: auto-merge for pull request #%d in %s unless it changes", k, w.taskID, w.number, plural(review.AutoMergeDelayMinutes, "minute"))
		}
		return next
	case now.Sub(parseTime(m.ArmedAt)) < time.Duration(review.AutoMergeDelayMinutes)*time.Minute:
		return led
	}
	// The stored ledger must still hold this armed state: a round or another replica may have moved on since the read.
	next, claimed, err := r.mergeLedger(ctx, k, led, func(l *Ledger) bool { return l.claimMerge(w.taskID, w.pr.HeadSHA, w.snapshot) })
	if err != nil || !claimed {
		if err != nil {
			log.Printf("[projects] %s task %s: claiming the auto-merge of pull request #%d: %v", k, w.taskID, w.number, err)
		}
		return next
	}
	led = next
	heads := contentHeads(m, w.pr.HeadSHA)
	if w.pr.MergeState == mergeStateClean || w.pr.MergeState == mergeStateHasHooks {
		return r.mergeNow(ctx, p, led, w, review, r.approver(ctx, w, heads), now)
	}
	decision, err := r.gh.ReviewDecision(ctx, w.pr.NodeID)
	switch {
	case err != nil:
		log.Printf("[projects] %s task %s: reading the review decision of pull request #%d: %v", k, w.taskID, w.number, err)
		return r.mergeWaits(ctx, p, led, w, "arbetern could not read the review decision: "+github.APIMessage(err), false, now)
	case decision == "" && r.approver(ctx, w, heads) == "":
		return r.mergeWaits(ctx, p, led, w, "No review is required here, so arbetern turns on auto-merge once a person with write access approves "+shortSHA(w.pr.HeadSHA)+" and no review asks for changes.", false, now)
	}
	err = r.gh.EnablePullRequestAutoMerge(ctx, w.pr.NodeID, review.MergeMethod, w.pr.HeadSHA)
	switch {
	case errors.Is(err, github.ErrMergeableNow):
		return r.mergeNow(ctx, p, led, w, review, r.approver(ctx, w, heads), now)
	case err != nil:
		log.Printf("[projects] %s task %s: turning on auto-merge for pull request #%d: %v", k, w.taskID, w.number, err)
		return r.mergeWaits(ctx, p, led, w, "GitHub did not turn on auto-merge: "+github.APIMessage(err), true, now)
	}
	log.Printf("[projects] %s task %s: auto-merge on for pull request #%d (head %s, %s)", k, w.taskID, w.number, shortSHA(w.pr.HeadSHA), review.MergeMethod)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	next, _, err = r.mergeLedger(pctx, k, led, func(l *Ledger) bool { return l.mergeOn(w.taskID, w.pr.HeadSHA, w.pr.BaseRef, true, now) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording auto-merge on pull request #%d: %v", k, w.taskID, w.number, err)
		next = led
	}
	if t := next.task(w.taskID); t != nil {
		r.notify(p, noteAutoMerge(t, review.MergeMethod))
	}
	return next
}

func (r *Registry) mergeNow(ctx context.Context, p *Project, led *Ledger, w prWatch, review Review, approver string, now time.Time) *Ledger {
	k := p.ID
	if approver == "" {
		return r.mergeWaits(ctx, p, led, w, "GitHub has nothing left to wait for, so auto-merge cannot be turned on. arbetern merges once a person with write access approves "+shortSHA(w.pr.HeadSHA)+" and no review asks for changes.", false, now)
	}
	if err := r.gh.MergePullRequest(ctx, w.owner, w.repo, w.number, w.pr.HeadSHA, review.MergeMethod); err != nil {
		log.Printf("[projects] %s task %s: merging pull request #%d: %v", k, w.taskID, w.number, err)
		return r.mergeWaits(ctx, p, led, w, "GitHub did not merge it: "+github.APIMessage(err), true, now)
	}
	log.Printf("[projects] %s task %s: merged pull request #%d (head %s, %s): nothing was left to wait for", k, w.taskID, w.number, shortSHA(w.pr.HeadSHA), review.MergeMethod)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if _, err := r.gh.CommentOnPullRequest(pctx, w.owner, w.repo, w.number, mergedComment(p, review, approver, w.pr.HeadSHA)); err != nil {
		log.Printf("[projects] %s task %s: posting the merge note on pull request #%d: %v", k, w.taskID, w.number, err)
	}
	next, _, err := r.mergeLedger(pctx, k, led, func(l *Ledger) bool { return l.mergedByArbetern(w.taskID, now) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording the merge of pull request #%d: %v", k, w.taskID, w.number, err)
		return led
	}
	return next
}

func (r *Registry) mergeWaits(ctx context.Context, p *Project, led *Ledger, w prWatch, note string, warn bool, now time.Time) *Ledger {
	k := p.ID
	note = oneLine(note, maxMergeNoteChars)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	next, _, err := r.mergeLedger(pctx, k, led, func(l *Ledger) bool { return l.mergeWaits(w.taskID, w.snapshot, note, warn, now) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording why auto-merge for pull request #%d waits: %v", k, w.taskID, w.number, err)
		return led
	}
	log.Printf("[projects] %s task %s: auto-merge for pull request #%d waits: %s", k, w.taskID, w.number, note)
	r.warnMerge(p, led, next, w, note)
	return next
}

func (r *Registry) noteMerge(ctx context.Context, p *Project, led *Ledger, w prWatch, note string, warn bool) *Ledger {
	k := p.ID
	note = oneLine(note, maxMergeNoteChars)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	next, changed, err := r.mergeLedger(pctx, k, led, func(l *Ledger) bool { return l.noteMerge(w.taskID, note, warn) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording an auto-merge note on pull request #%d: %v", k, w.taskID, w.number, err)
		return led
	}
	if changed {
		log.Printf("[projects] %s task %s: auto-merge for pull request #%d: %s", k, w.taskID, w.number, note)
	}
	r.warnMerge(p, led, next, w, note)
	return next
}

func (r *Registry) warnMerge(p *Project, before, after *Ledger, w prWatch, note string) {
	_, was := before.review(w.taskID)
	t, rv := after.review(w.taskID)
	if rv == nil || rv.AutoMerge == nil || rv.AutoMerge.Warned != note || was != nil && was.AutoMerge != nil && was.AutoMerge.Warned == note {
		return
	}
	r.notify(p, noteMergeWaits(t, note))
}

func (r *Registry) keepUpToDate(ctx context.Context, p *Project, led *Ledger, w prWatch, review Review, now time.Time) *Ledger {
	k := p.ID
	_, rv := led.review(w.taskID)
	m := rv.AutoMerge
	if w.pr.MergeState != mergeStateBehind || m == nil || now.Sub(parseTime(m.UpdateTriedAt)) < branchUpdateRetry {
		return led
	}
	switch {
	case review.MergeMethod == "rebase":
		return r.noteMerge(ctx, p, led, w, "The branch is behind its base. With rebase merges, a person brings it up to date.", true)
	case len(m.Lineage) >= maxBranchUpdates:
		return r.noteMerge(ctx, p, led, w, fmt.Sprintf("The branch fell behind its base again after %s by arbetern. Over to a person.", plural(maxBranchUpdates, "update")), true)
	case r.approver(ctx, w, contentHeads(m, w.pr.HeadSHA)) == "":
		return led
	}
	next, claimed, err := r.mergeLedger(ctx, k, led, func(l *Ledger) bool { return l.claimUpdate(w.taskID, w.pr.HeadSHA, now) })
	if err != nil || !claimed {
		if err != nil {
			log.Printf("[projects] %s task %s: claiming the update of pull request #%d: %v", k, w.taskID, w.number, err)
		}
		return next
	}
	if err := r.gh.UpdatePullRequestBranch(ctx, w.owner, w.repo, w.number, w.pr.HeadSHA); err != nil {
		log.Printf("[projects] %s task %s: bringing pull request #%d up to date with its base: %v", k, w.taskID, w.number, err)
		return r.noteMerge(ctx, p, next, w, "GitHub did not bring the branch up to date with its base: "+github.APIMessage(err), true)
	}
	log.Printf("[projects] %s task %s: asked GitHub to bring pull request #%d up to date with its base", k, w.taskID, w.number)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	done, _, err := r.mergeLedger(pctx, k, next, func(l *Ledger) bool { return l.branchUpdated(w.taskID, w.pr.HeadSHA) })
	if err != nil {
		log.Printf("[projects] %s task %s: recording the update of pull request #%d: %v", k, w.taskID, w.number, err)
		return next
	}
	return done
}

func mergedComment(p *Project, review Review, approver, head string) string {
	return fmt.Sprintf("**Merged.** Every check passed on `%s`, %s approved it, and nothing changed for %s. GitHub had nothing left to wait for, so arbetern merged it (%s) instead of turning on auto-merge.\n\n%s",
		shortSHA(head), approver, plural(review.AutoMergeDelayMinutes, "minute"), review.MergeMethod, projectFooter(p))
}

func (r *Registry) mergedBase(ctx context.Context, owner, repo string, pr *github.PRState, from string) (bool, error) {
	parents, byGitHub, err := r.gh.CommitParents(ctx, owner, repo, pr.HeadSHA)
	if err != nil || !byGitHub || len(parents) != 2 || parents[0] != from {
		return false, err
	}
	ahead, err := r.gh.CommitsAhead(ctx, owner, repo, pr.BaseRef, parents[1])
	return err == nil && ahead == 0, err
}
