package projects

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/justmike1/arbetern/github"
)

var passedConclusions = []string{"success", "neutral", "skipped"}

func (r *Registry) mergePastCodeOwners(ctx context.Context, p *Project, led *Ledger, w prWatch, review Review, now time.Time) *Ledger {
	k := p.ID
	approval, ok := standInApproval(w, review.CodeOwnerStandIn)
	if !ok {
		return led
	}
	decision, err := r.gh.ReviewDecision(ctx, w.pr.NodeID)
	if err != nil {
		log.Printf("[projects] %s task %s: reading the review decision of pull request #%d: %v", k, w.taskID, w.number, err)
		return led
	}
	if decision != github.ReviewRequired {
		return led
	}
	rules, err := r.gh.BranchMergeRules(ctx, w.owner, w.repo, w.pr.BaseRef)
	if err != nil {
		log.Printf("[projects] %s task %s: reading the branch rules of pull request #%d: %v", k, w.taskID, w.number, err)
		return led
	}
	if !rules.CodeOwners {
		return led
	}
	// The bypass skips every rule on the branch, so each rule but code-owner review must already hold.
	hold, err := r.otherRulesHold(ctx, w, review, rules, approval)
	switch {
	case err != nil:
		log.Printf("[projects] %s task %s: checking the branch rules of pull request #%d: %v", k, w.taskID, w.number, err)
		return led
	case hold != "":
		return r.noteMerge(ctx, p, led, w, hold+" A code owner's review is still needed.", w.pr.HeadSHA, true)
	}
	if rules.Strict {
		behind, err := r.gh.CommitsBehind(ctx, w.owner, w.repo, w.pr.BaseRef, w.pr.HeadSHA)
		if err != nil {
			log.Printf("[projects] %s task %s: comparing pull request #%d with its base: %v", k, w.taskID, w.number, err)
			return led
		}
		if behind > 0 {
			return r.updateBranch(ctx, p, led, w, review, now, func(context.Context, prWatch) (string, error) { return approval.Author, nil })
		}
	}
	next, claimed, err := r.mergeLedger(ctx, k, led, func(l *Ledger) bool { return l.claimBypass(w.taskID, w.pr.HeadSHA, now) })
	if err != nil || !claimed {
		if err != nil {
			log.Printf("[projects] %s task %s: claiming the merge of pull request #%d: %v", k, w.taskID, w.number, err)
		}
		return next
	}
	why := "only a code owner's review was missing, and " + approval.Author + " approved"
	done, err := r.merge(ctx, p, next, w, review, why, standInComment(p, review, approval.Author, w.pr.HeadSHA), now)
	if err != nil {
		return r.noteMerge(ctx, p, next, w, "GitHub did not merge it past the missing code-owner review: "+github.APIMessage(err), w.pr.HeadSHA, true)
	}
	return done
}

func standInApproval(w prWatch, login string) (github.DiscussionItem, bool) {
	for _, a := range latestApprovals(w.reviews, func(it github.DiscussionItem) bool { return it.Bot || it.Trusted() }) {
		if a.Bot && strings.EqualFold(a.Author, login) && approvesCurrent(a, w) {
			return a, true
		}
	}
	return github.DiscussionItem{}, false
}

func (r *Registry) otherRulesHold(ctx context.Context, w prWatch, review Review, rules github.MergeRules, approval github.DiscussionItem) (string, error) {
	switch {
	case len(rules.Other) > 0:
		return fmt.Sprintf("The branch also has %s rules, which arbetern does not check itself.", strings.Join(rules.Other, ", ")), nil
	case rules.MergeMethods != nil && !slices.Contains(rules.MergeMethods, review.MergeMethod):
		return fmt.Sprintf("The branch rules do not allow %s merges.", review.MergeMethod), nil
	case review.MergeMethod == github.MergeCommit && (rules.LinearHistory || rules.SignedCommits):
		return "A merge commit would break the branch's linear history or signed commits rule.", nil
	case rules.LastPushApproval && approval.CommitID != w.pr.HeadSHA:
		return fmt.Sprintf("The branch rules want the latest push approved, and %s approved an earlier one.", approval.Author), nil
	}
	for _, c := range rules.Checks {
		if !requiredCheckPassed(w.checks, c) {
			return fmt.Sprintf("The required check %s has not passed on %s.", c.Context, shortSHA(w.pr.HeadSHA)), nil
		}
	}
	if rules.Approvals > 1 {
		n, err := r.writerApprovals(ctx, w)
		if err != nil {
			return "", err
		}
		if n+1 < rules.Approvals {
			return fmt.Sprintf("The branch rules ask for %d approvals, and these changes have %d.", rules.Approvals, n+1), nil
		}
	}
	if rules.ThreadResolution {
		open, err := r.gh.UnresolvedReviewThreads(ctx, w.pr.NodeID)
		if err != nil {
			return "", err
		}
		if open > 0 {
			verb := "are"
			if open == 1 {
				verb = "is"
			}
			return fmt.Sprintf("The branch rules want every review thread resolved, and %s %s still open.", plural(open, "thread"), verb), nil
		}
	}
	if len(rules.ReviewerFiles) > 0 {
		paths, err := r.gh.ChangedPaths(ctx, w.owner, w.repo, w.number)
		if err != nil {
			return "", err
		}
		for _, path := range paths {
			if slices.ContainsFunc(rules.ReviewerFiles, func(pattern string) bool { return globMatches(pattern, path) }) {
				return fmt.Sprintf("The branch rules ask a specific team to review %s.", path), nil
			}
		}
	}
	return "", nil
}

func requiredCheckPassed(checks []github.Check, rc github.RequiredCheck) bool {
	return slices.ContainsFunc(checks, func(c github.Check) bool {
		return c.Name == rc.Context && (rc.AppID == 0 || c.AppID == rc.AppID) && c.Status == github.CheckCompleted && slices.Contains(passedConclusions, c.Conclusion)
	})
}

func (r *Registry) writerApprovals(ctx context.Context, w prWatch) (int, error) {
	n := 0
	for _, a := range approvals(w.reviews) {
		if !approvesCurrent(a, w) {
			continue
		}
		ok, err := r.gh.CanPush(ctx, w.owner, w.repo, a.Author)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// globMatches errs toward a match: a wildcard crosses directories, any trailing part of the path may match, and a pattern it cannot read matches everything.
func globMatches(pattern, path string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	re, err := regexp.Compile("^" + globRegexp(pattern) + "$")
	if err != nil {
		return true
	}
	for i := range len(path) + 1 {
		if (i == 0 || path[i-1] == '/') && re.MatchString(path[i:]) {
			return true
		}
	}
	return false
}

func globRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if strings.HasPrefix(p[i:], "**/") {
				b.WriteString("(?:.*/)?")
				i += 2
				continue
			}
			b.WriteString(".*")
			for i+1 < len(p) && p[i+1] == '*' {
				i++
			}
		case '?':
			b.WriteString(".")
		case '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		case '{':
			end := strings.IndexByte(p[i+1:], '}')
			if end < 0 {
				b.WriteString(`\{`)
				continue
			}
			var alts []string
			for _, alt := range strings.Split(p[i+1:i+1+end], ",") {
				alts = append(alts, globRegexp(alt))
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

func standInComment(p *Project, review Review, approver, head string) string {
	return fmt.Sprintf("**Merged.** Every check passed on `%s` and %s approved these changes. A code owner's review was the only thing GitHub still asked for, and this project lets %s's approval stand in for it, so arbetern merged it (%s) with its bypass.\n\n%s",
		shortSHA(head), approver, approver, review.MergeMethod, projectFooter(p))
}
