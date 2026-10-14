package projects

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
)

type plannedReply struct {
	item Feedback
	body string
}

func (a reportArgs) roundOutcome(rd *Round, base string, now time.Time) (Outcome, []plannedReply, string) {
	o, problem := a.common(now, 4000)
	if problem != "" {
		return o, nil, problem
	}
	switch o.Status {
	case OutcomeFixed, OutcomeAnswered, OutcomeCannotFix:
	default:
		return o, nil, "status must be fixed, answered or cannot_fix in a follow-up round."
	}
	if o.Status == OutcomeFixed {
		if o.Branch, problem = checkPushBranch(a.Branch, base); problem != "" {
			return o, nil, problem
		}
	}
	if a.RerunChecks && len(rd.Checks) == 0 {
		return o, nil, "rerun_checks needs failing checks in this round."
	}
	replies, problem := planReplies(rd.Items, a.Replies, o.Status != OutcomeCannotFix)
	return o, replies, problem
}

func planReplies(items []Feedback, args []replyArg, requireAll bool) ([]plannedReply, string) {
	if len(args) > maxReplies {
		return nil, fmt.Sprintf("replies holds at most %d entries.", maxReplies)
	}
	byID := make(map[string]Feedback, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	seen := map[string]bool{}
	out := make([]plannedReply, 0, len(args))
	for _, a := range args {
		id := strings.TrimSpace(a.ID)
		it, ok := byID[id]
		switch {
		case !ok:
			return nil, fmt.Sprintf("replies: %q is not one of this round's review comments.", oneLine(id, 64))
		case seen[id]:
			return nil, fmt.Sprintf("replies: %s is answered twice.", id)
		}
		body := strings.TrimSpace(stripControl(a.Body))
		if err := checkText("the reply to "+id, body, 1, maxReplyChars); err != nil {
			return nil, err.Error() + "."
		}
		seen[id] = true
		out = append(out, plannedReply{item: it, body: body})
	}
	if !requireAll {
		return out, ""
	}
	var missing []string
	for _, it := range items {
		if !seen[it.ID] {
			missing = append(missing, it.ID)
		}
	}
	if len(missing) > 0 {
		return nil, "Answer every review comment in replies; missing: " + strings.Join(missing, ", ") + "."
	}
	return out, ""
}

func (g *gateway) reportRound(ctx context.Context, s *session, a reportArgs) (string, bool, error) {
	rd := s.round()
	if rd == nil {
		return "This follow-up round is no longer in the ledger.", true, nil
	}
	if rd.Outcome != nil {
		return "An outcome (" + rd.Outcome.Status + ") was already recorded for this round. You can stop now.", false, nil
	}
	now := time.Now().UTC()
	o, replies, problem := a.roundOutcome(rd, s.project.Repo.BaseBranch, now)
	description := strings.TrimSpace(stripControl(a.Description))
	if problem == "" && description != "" {
		problem = checkRoundDescription(s.project, description)
	}
	if problem == "" && o.Status == OutcomeFixed && o.Branch != "" {
		if err := s.pushAllowed(o.Branch); err != nil {
			problem = "Report the pull request's branch or a name derived from it: " + err.Error() + "."
		}
	}
	switch {
	case problem != "":
		return problem, true, nil
	case rd.Status != StatusRunning:
		return "This follow-up round is not running (" + rd.Status + ").", true, nil
	case g.r.gh == nil:
		return "GitHub is not configured in this deployment, so nothing can be posted.", true, nil
	}
	taskID, n := s.task.ID, s.roundN
	guard := now.Format(time.RFC3339Nano)
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.beginRoundReport(taskID, n, guard, now) }); err != nil {
		return "", false, err
	}
	if rd = s.round(); rd == nil || rd.Reporting != guard {
		if rd != nil && rd.Outcome != nil {
			return "An outcome (" + rd.Outcome.Status + ") was already recorded for this round. You can stop now.", false, nil
		}
		return "This round's report is already being recorded; call report_outcome again in a minute if it does not finish.", true, nil
	}
	owner, repo, number := prCoordinates(s.project, s.task.PR)
	head := rd.HeadBefore
	var extra []string
	if o.Status == OutcomeFixed {
		var problem string
		var rewritten bool
		if head, problem, rewritten = g.landRound(ctx, rd, owner, repo, number, o.Branch); problem != "" {
			g.releaseRoundReport(s, guard)
			return problem, true, nil
		}
		if rewritten {
			log.Printf("[projects] %s task %s round %d: the pull request branch no longer contains %s", s.project.ID, taskID, n, shortSHA(rd.HeadBefore))
			extra = append(extra, "The pull request branch no longer contains `"+shortSHA(rd.HeadBefore)+"`, the head this round started from: its history was rewritten while the round ran.")
		}
	}
	plan := g.planDescription(ctx, s, owner, repo, number, description, o, rd)
	if description != "" && plan.edited {
		g.releaseRoundReport(s, guard)
		return "The pull request description was edited on GitHub since arbetern wrote it, so arbetern will not replace it: call report_outcome again without description, and say in your reply what the description should change.", true, nil
	}
	posted := g.postReplies(ctx, owner, repo, number, replies)
	rerun := a.RerunChecks && head == rd.HeadBefore && g.rerunChecks(ctx, s, rd, owner, repo)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	described, sha, fact := g.applyDescription(pctx, s, owner, repo, number, plan)
	if fact != "" {
		extra = append(extra, fact)
	}
	if _, err := g.r.gh.CommentOnPullRequest(ctx, owner, repo, number, roundComment(s.project, rd, o, owner, repo, head, posted, rerun, extra)); err != nil {
		log.Printf("[projects] %s task %s round %d: posting the round summary: %v", s.project.ID, taskID, n, err)
	}
	res := roundResult{outcome: o, head: head, replies: posted, rerun: rerun, described: described, bodySHA: sha}
	changed, err := g.apply(pctx, s, func(l *Ledger) bool { return l.finishRound(taskID, n, res, now) })
	if err != nil {
		log.Printf("[projects] %s task %s round %d: recording the report: %v", s.project.ID, taskID, n, err)
		return "Your replies and summary are posted, but arbetern could not record the round. Do not report again; you can stop now.", false, nil
	}
	if !changed {
		status := "ended"
		if rd = s.round(); rd != nil {
			status = rd.Status
		}
		log.Printf("[projects] %s task %s round %d was %s before its report landed; its replies stay posted", s.project.ID, taskID, n, status)
		if _, err := g.apply(pctx, s, func(l *Ledger) bool { return l.recordRoundReplies(taskID, n, res, now) }); err != nil {
			log.Printf("[projects] %s task %s round %d: recording the replies: %v", s.project.ID, taskID, n, err)
		}
		return "Your replies and summary are posted, but arbetern had already ended this round (" + status + "). Do not report again; you can stop now.", false, nil
	}
	log.Printf("[projects] %s task %s round %d reported %s (%d of %d replies posted)", s.project.ID, taskID, n, o.Status, posted, len(replies))
	g.r.refreshStats(s.project.ID, s.ledger)
	g.r.notify(s.project, noteRound(s.task, o))
	if description != "" && described == nil {
		return "Recorded, but the pull request description could not be updated; the round summary says why. You can stop now.", false, nil
	}
	return "Recorded. You can stop now.", false, nil
}

func checkRoundDescription(p *Project, description string) string {
	if p.Conventions.Description != descriptionSession {
		return "description is not used in this project: arbetern writes the pull request description from the first report."
	}
	if err := checkText("description", description, 1, maxSessionSummary); err != nil {
		return err.Error() + "."
	}
	return ""
}

type descriptionPlan struct {
	next   *Outcome // nil when the description stays
	body   string
	edited bool // the live description is not the one arbetern last wrote
	reason string
}

// planDescription works out whether the round changes what the description shows: the session's new description, or the decision callout.
func (g *gateway) planDescription(ctx context.Context, s *session, owner, repo string, number int, description string, o Outcome, rd *Round) descriptionPlan {
	t := s.task
	if t.Outcome == nil || t.PR == nil {
		return descriptionPlan{reason: "the task has no recorded description"}
	}
	next := *t.Outcome
	next.Summary = cmp.Or(description, next.Summary)
	switch {
	case o.Question != "":
		next.Question = o.Question
	case personSpoke(rd):
		next.Question = ""
	}
	if next.Summary == t.Outcome.Summary && next.Question == t.Outcome.Question {
		return descriptionPlan{}
	}
	pr, err := g.r.gh.PullRequestState(ctx, owner, repo, number)
	if err != nil {
		log.Printf("[projects] %s task %s round %d: reading the pull request description: %v", s.project.ID, t.ID, s.roundN, err)
		return descriptionPlan{reason: "GitHub could not be read"}
	}
	if !arbeternWrote(s.project, t, pr.Body) {
		return descriptionPlan{edited: true, reason: "someone edited it on GitHub since arbetern wrote it"}
	}
	return descriptionPlan{next: &next, body: prBody(s.project, t, next)}
}

// arbeternWrote reports whether body is still the description arbetern last wrote: by its hash, or, for a pull request opened before hashes were kept, by the rendered summary and the marker.
func arbeternWrote(p *Project, t *Task, body string) bool {
	if t.PR.BodySHA != "" {
		return bodySHA(body) == t.PR.BodySHA
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	return strings.Contains(body, summaryBlock(p, t, *t.Outcome)) && strings.Contains(body, github.ProjectPRMarker(p.ID, t.ID))
}

func (g *gateway) applyDescription(ctx context.Context, s *session, owner, repo string, number int, plan descriptionPlan) (described *Outcome, sha, fact string) {
	switch {
	case plan.next == nil && plan.reason != "":
		return nil, "", "The pull request description was left as is: " + plan.reason + "."
	case plan.next == nil:
		return nil, "", ""
	}
	if err := g.r.gh.EditPullRequestBody(ctx, owner, repo, number, plan.body); err != nil {
		// GitHub may have applied the edit and failed only to answer.
		if pr, rerr := g.r.gh.PullRequestState(ctx, owner, repo, number); rerr != nil || bodySHA(pr.Body) != bodySHA(plan.body) {
			log.Printf("[projects] %s task %s round %d: updating the pull request description: %v", s.project.ID, s.task.ID, s.roundN, err)
			return nil, "", "The pull request description could not be updated: GitHub refused the edit."
		}
	}
	return plan.next, bodySHA(plan.body), "Updated the pull request description."
}

func bodySHA(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))))
	return hex.EncodeToString(sum[:])
}

func (g *gateway) releaseRoundReport(s *session, guard string) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	taskID, n := s.task.ID, s.roundN
	if _, err := g.apply(ctx, s, func(l *Ledger) bool { return l.clearRoundReport(taskID, n, guard) }); err != nil {
		log.Printf("[projects] %s task %s round %d: clearing the report guard: %v", s.project.ID, taskID, n, err)
	}
}

// landRound reports the pull request head the round's commits are on, and whether its history no longer holds the head the round started from.
func (g *gateway) landRound(ctx context.Context, rd *Round, owner, repo string, number int, branch string) (head, problem string, rewritten bool) {
	gh := g.r.gh
	pr, err := gh.PullRequestState(ctx, owner, repo, number)
	switch {
	case err != nil:
		return "", "Could not read the pull request on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute.", false
	case pr.State != "open":
		return "", "The pull request is no longer open, so nothing can be added to it.", false
	case branch == pr.HeadRef && pr.HeadSHA == rd.HeadBefore:
		return "", "Nothing new is on " + branch + " yet: commit, push with `git push origin HEAD:" + branch + "`, then call report_outcome again.", false
	case branch == pr.HeadRef:
		_, behind, err := gh.Divergence(ctx, owner, repo, rd.HeadBefore, pr.HeadSHA)
		return pr.HeadSHA, "", err == nil && behind > 0
	}
	sha, err := gh.BranchHead(ctx, owner, repo, branch)
	switch {
	case github.IsNotFound(err):
		return "", "Branch " + branch + " is not on GitHub yet — push it and call report_outcome again.", false
	case err != nil:
		return "", "Could not read branch " + branch + " on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute.", false
	}
	ahead, behind, err := gh.Divergence(ctx, owner, repo, pr.HeadSHA, sha)
	switch {
	case err != nil:
		return "", "Could not compare " + branch + " with the pull request on GitHub (" + githubProblem(err) + "); call report_outcome again in a minute.", false
	case ahead == 0:
		return "", "Branch " + branch + " has no commits beyond the pull request head " + shortSHA(pr.HeadSHA) + ".", false
	case behind > 0:
		return "", "Branch " + branch + " does not contain the pull request head " + shortSHA(pr.HeadSHA) + ": start from origin/" + pr.HeadRef + ", reapply your commits and push again.", false
	}
	if err := gh.FastForwardBranch(ctx, owner, repo, pr.HeadRef, sha); err != nil {
		if github.IsRefused(err) {
			return "", "A branch rule of the repository refuses updates to " + pr.HeadRef + " by arbetern's token, which your pushes use too. Report cannot_fix and say so; an admin has to let the token's account update the project's branches, such as " + pr.HeadRef + ".", false
		}
		return "", "Could not move " + pr.HeadRef + " onto " + branch + " (" + githubProblem(err) + "); call report_outcome again in a minute.", false
	}
	if other, err := gh.FindOpenPullRequestByHead(ctx, owner, repo, branch); err == nil && other == nil {
		if err := gh.DeleteBranch(ctx, owner, repo, branch); err != nil {
			log.Printf("[projects] deleting the merged-in branch %s of %s/%s: %v", branch, owner, repo, err)
		}
	}
	return sha, "", false
}

func (g *gateway) postReplies(ctx context.Context, owner, repo string, number int, replies []plannedReply) int {
	posted := 0
	for _, rp := range replies {
		body := cleanPRText(rp.body, maxReplyChars)
		var err error
		if strings.HasPrefix(rp.item.ID, github.DiscussionInline+":") && rp.item.Thread != 0 {
			_, err = g.r.gh.ReplyToReviewComment(ctx, owner, repo, number, rp.item.Thread, body)
		} else {
			_, err = g.r.gh.CommentOnPullRequest(ctx, owner, repo, number, quotedReply(rp.item, body))
		}
		if err != nil {
			log.Printf("[projects] replying to %s on %s/%s#%d: %v", rp.item.ID, owner, repo, number, err)
			continue
		}
		posted++
	}
	return posted
}

func quotedReply(f Feedback, body string) string {
	quote := oneLine(cleanPRText(f.Body, 600), 200)
	who := "@" + f.Author
	if f.Bot {
		who = cleanPRText(f.Author, 100)
	}
	if f.URL != "" {
		return fmt.Sprintf("> %s [wrote](%s): %s\n\n%s", who, f.URL, quote, body)
	}
	return fmt.Sprintf("> %s wrote: %s\n\n%s", who, quote, body)
}

func (g *gateway) rerunChecks(ctx context.Context, s *session, rd *Round, owner, repo string) bool {
	if s.task.Review != nil && s.task.Review.RerunHead == rd.HeadBefore {
		return false
	}
	runs := map[int64]bool{}
	for _, c := range rd.Checks {
		o, r, id, err := github.ParseWorkflowRunURL(c.URL)
		if err == nil && strings.EqualFold(o, owner) && strings.EqualFold(r, repo) {
			runs[id] = true
		}
	}
	rerun := false
	for id := range runs {
		if err := g.r.gh.RerunFailedJobs(ctx, owner, repo, id); err != nil {
			log.Printf("[projects] re-running the failed jobs of run %d in %s/%s: %v", id, owner, repo, err)
			continue
		}
		rerun = true
	}
	return rerun
}

func roundComment(p *Project, rd *Round, o Outcome, owner, repo, head string, posted int, rerun bool, extra []string) string {
	label := map[string]string{
		OutcomeFixed:     "pushed a fix",
		OutcomeAnswered:  "answered without changing code",
		OutcomeCannotFix: "needs a person",
	}[o.Status]
	var b strings.Builder
	fmt.Fprintf(&b, "**Follow-up round %d: %s.**\n\n", rd.N, label)
	b.WriteString(cleanPRText(o.Summary, 4000))
	if t := cleanPRText(o.Testing, 2000); t != "" {
		b.WriteString("\n\n**Testing**\n\n" + t)
	}
	var facts []string
	if head != "" && rd.HeadBefore != "" && head != rd.HeadBefore {
		facts = append(facts, fmt.Sprintf("Changes: https://github.com/%s/%s/compare/%s...%s", owner, repo, rd.HeadBefore, head))
	}
	if len(rd.Items) > 0 {
		facts = append(facts, fmt.Sprintf("Answered %d of %d review comments in their threads.", posted, len(rd.Items)))
	}
	if rerun {
		facts = append(facts, "Re-ran the failed jobs on `"+shortSHA(head)+"`.")
	}
	facts = append(facts, extra...)
	if q := cleanPRText(o.Question, maxQuestionChars); q != "" {
		facts = append(facts, "**Decision needed:** "+q+" Reply in this pull request and a follow-up round applies the answer.")
	}
	if len(facts) > 0 {
		b.WriteString("\n\n- " + strings.Join(facts, "\n- "))
	}
	b.WriteString("\n\n" + projectFooter(p))
	return b.String()
}
