package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/justmike1/arbetern/github"
)

// ErrDuplicateOpenPR is returned by CommitAndPR when the duplicate guard
// finds an equivalent open PR. No branch, commit, or PR was created, so
// callers should treat it as a recoverable no-op (reuse the existing PR)
// rather than a failed mutation.
var ErrDuplicateOpenPR = errors.New("duplicate guard: a similar open PR already exists")

// BranchManager handles the branch/commit/PR lifecycle for file modification
// tools. Multiple file changes targeting the same repository within a single
// handler execution (or thread session) are grouped into a single pull
// request, unless a write names its own branch — see groupingBranch.
type BranchManager struct {
	ghClient       *github.Client
	agentID        string
	activeBranches map[string]*ActiveBranchInfo
	session        *ThreadSession
}

// NewBranchManager creates a BranchManager. If a session is provided, it seeds
// the active branches from the session so that follow-up messages in a thread
// can reuse an existing PR.
func NewBranchManager(ghClient *github.Client, agentID string, session *ThreadSession) *BranchManager {
	bm := &BranchManager{
		ghClient:       ghClient,
		agentID:        agentID,
		activeBranches: make(map[string]*ActiveBranchInfo),
		session:        session,
	}
	if session != nil {
		session.mu.Lock()
		if session.ActiveBranches != nil {
			for k, v := range session.ActiveBranches {
				bm.activeBranches[k] = v
			}
		}
		session.mu.Unlock()
	}
	return bm
}

// ActiveBranch returns the existing branch info for a repo, or nil.
func (bm *BranchManager) ActiveBranch(owner, repo string) *ActiveBranchInfo {
	return bm.activeBranches[owner+"/"+repo]
}

// ReadBranch returns the branch to read files from — the branch this write
// will group into if there is one, otherwise the base branch. requestedBranch
// is the caller's branch_name argument, so a write that starts its own branch
// reads from the base rather than from an unrelated PR's branch.
func (bm *BranchManager) ReadBranch(ctx context.Context, owner, repo, baseBranch, requestedBranch string) string {
	if active := bm.groupingBranch(ctx, owner, repo, requestedBranch); active != nil {
		return active.BranchName
	}
	return baseBranch
}

// groupingBranch returns the active branch this write should be added to, or
// nil when it should open a new branch and PR. Writes that leave the branch
// name empty group into the repo's active PR, which is what a multi-file
// change wants. Naming a branch other than the active one is an explicit
// request for a separate PR, so it does not group: a caller fixing several
// unrelated issues in one repo gives each fix its own branch name and gets one
// reviewable PR per fix — unless that branch already exists on the remote, in
// which case the write belongs on it (see adoptRemoteBranch).
func (bm *BranchManager) groupingBranch(ctx context.Context, owner, repo, requestedBranch string) *ActiveBranchInfo {
	active := bm.resolveActiveBranch(ctx, owner, repo)
	requested := strings.TrimSpace(requestedBranch)
	if active != nil && (requested == "" || requested == active.BranchName) {
		return active
	}
	if requested == "" {
		return nil
	}
	return bm.adoptRemoteBranch(ctx, owner, repo, requested)
}

// adoptRemoteBranch makes a branch that already exists on the remote the repo's
// active branch, so the write commits onto it rather than trying to cut it
// again. This is what routes requested changes onto an existing pull request:
// in-memory grouping only covers PRs this process opened, and it is gone by the
// next chat turn, the next scheduled tick, and once a thread session expires —
// so asking for a change to a PR from any of those would otherwise cut a second
// branch and open a second PR for every round of review feedback.
//
// The open PR for that branch, when there is one, is adopted with it: later
// writes report its URL and add to it. A branch with no open PR (never had one,
// or its PR was merged or closed) is still adopted, and CommitAndPR opens a PR
// for the commit that lands on it.
func (bm *BranchManager) adoptRemoteBranch(ctx context.Context, owner, repo, branch string) *ActiveBranchInfo {
	exists, err := bm.ghClient.BranchExists(ctx, owner, repo, branch)
	if err != nil {
		log.Printf("[branch-manager] could not verify requested branch %s, treating it as new: %v", branch, err)
		return nil
	}
	if !exists {
		return nil
	}

	info := &ActiveBranchInfo{BranchName: branch}
	pr, err := bm.ghClient.FindOpenPullRequestByHead(ctx, owner, repo, branch)
	switch {
	case err != nil:
		log.Printf("[branch-manager] could not look up an open PR for existing branch %s: %v", branch, err)
	case pr != nil:
		info.BaseBranch = pr.BaseRef
		info.PrURL = pr.URL
		log.Printf("[branch-manager] committing to existing branch %s (open PR: %s)", branch, pr.URL)
	default:
		log.Printf("[branch-manager] committing to existing branch %s (no open PR; one will be opened)", branch)
	}

	repoKey := owner + "/" + repo
	bm.activeBranches[repoKey] = info
	bm.syncToSession(repoKey, info)
	return info
}

// resolveActiveBranch returns the active branch for a repo, first dropping it
// if it no longer exists upstream. A thread session outlives the PR it opened,
// and merging that PR deletes its head branch — without this check every
// later read and commit in the thread retries against a branch GitHub has
// already removed and fails identically each time. Forgetting it here lets the
// caller fall back to opening a fresh branch and PR.
func (bm *BranchManager) resolveActiveBranch(ctx context.Context, owner, repo string) *ActiveBranchInfo {
	active := bm.ActiveBranch(owner, repo)
	if active == nil {
		return nil
	}
	exists, err := bm.ghClient.BranchExists(ctx, owner, repo, active.BranchName)
	if err != nil {
		// Inconclusive: keep the branch. A commit against a branch that really
		// is gone still recovers via the not-found fallback in CommitAndPR.
		log.Printf("[branch-manager] could not verify branch %s, assuming it exists: %v", active.BranchName, err)
		return active
	}
	if exists {
		return active
	}
	log.Printf("[branch-manager] active branch %s no longer exists (PR likely merged); a new branch will be created", active.BranchName)
	bm.forget(owner + "/" + repo)
	return nil
}

// forget drops a repo's active branch from the manager and the thread session
// so the next write starts a new branch instead of reusing a dead one.
func (bm *BranchManager) forget(repoKey string) {
	delete(bm.activeBranches, repoKey)
	if bm.session == nil {
		return
	}
	bm.session.mu.Lock()
	delete(bm.session.ActiveBranches, repoKey)
	bm.session.mu.Unlock()
	bm.session.Save()
}

// CommitResult is returned by CommitAndPR with the outcome of the operation.
type CommitResult struct {
	PrURL   string
	IsNew   bool   // true if a new PR was created, false if committed to existing
	Message string // user-facing result message
}

// CommitAndPR commits a file change to an existing or new branch/PR.
// commitFn receives the target branch name and must perform the actual
// git commit (UpdateFile or CreateFile). prBody is used only when creating
// a new PR.
//
// branchOverride and prTitleOverride are optional. When non-empty, they are
// used in place of the auto-generated branch name and the default
// "<agentID>: <description>" PR title. They apply to the branch/PR this call
// opens; a later write that omits branchOverride, or repeats the active
// branch name, groups into that PR and ignores both overrides. A later write
// naming a different branch opens its own PR (see groupingBranch).
func (bm *BranchManager) CommitAndPR(
	ctx context.Context,
	owner, repo, baseBranch, userID, description, prBody, branchOverride, prTitleOverride string,
	changedFiles []string,
	commitFn func(branch string) error,
) (*CommitResult, error) {
	repoKey := owner + "/" + repo

	if active := bm.groupingBranch(ctx, owner, repo, branchOverride); active != nil {
		result, err := bm.commitToActive(ctx, active, owner, repo, baseBranch, description, prBody, prTitleOverride, changedFiles, commitFn)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
		// The branch was deleted between the existence check and the commit;
		// fall through to open a fresh branch and PR rather than failing.
		bm.forget(repoKey)
	}

	prTitle, err := bm.prTitle(description, prTitleOverride)
	if err != nil {
		return nil, err
	}
	if err := bm.ensureNoDuplicatePR(ctx, owner, repo, baseBranch, prTitle, prBody, changedFiles); err != nil {
		return nil, err
	}

	branchName := branchOverride
	if branchName == "" {
		branchName = github.GenerateBranchName(bm.agentID)
	}
	if err := bm.ghClient.CreateBranch(ctx, owner, repo, baseBranch, branchName); err != nil {
		return nil, fmt.Errorf("creating branch: %w", err)
	}
	if err := commitFn(branchName); err != nil {
		return nil, fmt.Errorf("committing file: %w", err)
	}
	prURL, err := bm.ghClient.CreatePullRequest(ctx, owner, repo, baseBranch, branchName, prTitle, prBody)
	if err != nil {
		return nil, fmt.Errorf("changes committed to branch %s but PR creation failed: %w", branchName, err)
	}

	bm.remember(repoKey, &ActiveBranchInfo{BranchName: branchName, BaseBranch: baseBranch, PrURL: prURL})
	log.Printf("[branch-manager] PR created on branch %s: %s", branchName, prURL)
	return &CommitResult{PrURL: prURL, IsNew: true, Message: prURL}, nil
}

// commitToActive commits onto a branch that already exists. A nil result with a
// nil error means the branch turned out to be gone and the caller should start
// a new one. An adopted branch with no open PR gets one opened for it here, so
// a change aimed at a stale branch still ends up reviewable.
func (bm *BranchManager) commitToActive(
	ctx context.Context,
	active *ActiveBranchInfo,
	owner, repo, baseBranch, description, prBody, prTitleOverride string,
	changedFiles []string,
	commitFn func(branch string) error,
) (*CommitResult, error) {
	base := active.BaseBranch
	if base == "" {
		base = baseBranch
	}

	// Resolved before the commit so a rejected PR cannot leave a stray commit
	// on a branch nothing will review.
	var prTitle string
	if active.PrURL == "" {
		var err error
		if prTitle, err = bm.prTitle(description, prTitleOverride); err != nil {
			return nil, err
		}
		if err := bm.ensureNoDuplicatePR(ctx, owner, repo, base, prTitle, prBody, changedFiles); err != nil {
			return nil, err
		}
	}

	switch err := commitFn(active.BranchName); {
	case err == nil:
	case github.IsMissingBranchError(err):
		log.Printf("[branch-manager] branch %s vanished mid-commit; falling back to a new branch", active.BranchName)
		return nil, nil
	default:
		return nil, fmt.Errorf("committing to existing branch: %w", err)
	}

	if active.PrURL != "" {
		log.Printf("[branch-manager] additional commit to branch %s for PR: %s", active.BranchName, active.PrURL)
		return &CommitResult{PrURL: active.PrURL, IsNew: false, Message: active.PrURL}, nil
	}

	prURL, err := bm.ghClient.CreatePullRequest(ctx, owner, repo, base, active.BranchName, prTitle, prBody)
	if err != nil {
		return nil, fmt.Errorf("changes committed to branch %s but PR creation failed: %w", active.BranchName, err)
	}
	bm.remember(owner+"/"+repo, &ActiveBranchInfo{BranchName: active.BranchName, BaseBranch: base, PrURL: prURL})
	log.Printf("[branch-manager] PR opened for existing branch %s: %s", active.BranchName, prURL)
	return &CommitResult{PrURL: prURL, IsNew: true, Message: prURL}, nil
}

// prTitle resolves the PR title. With both the override and the description
// empty the title would degrade to a bare "<agentID>:" and the body to a
// template with nothing filled in. Callers validate their own arguments (see
// requireDescription); this makes the failure impossible at the choke point
// rather than relying on every future caller to remember.
func (bm *BranchManager) prTitle(description, prTitleOverride string) (string, error) {
	if title := strings.TrimSpace(prTitleOverride); title != "" {
		return title, nil
	}
	if strings.TrimSpace(description) == "" {
		return "", fmt.Errorf("refusing to open a pull request with no title: both pr_title and description are empty")
	}
	return fmt.Sprintf("%s: %s", bm.agentID, description), nil
}

// ensureNoDuplicatePR fails with ErrDuplicateOpenPR when an equivalent PR is
// already open, naming the head branch so the caller can retry onto it.
func (bm *BranchManager) ensureNoDuplicatePR(ctx context.Context, owner, repo, baseBranch, prTitle, prBody string, changedFiles []string) error {
	existing, err := bm.ghClient.FindSimilarOpenPullRequest(ctx, owner, repo, baseBranch, github.PRDuplicateCandidate{
		Title:        prTitle,
		Body:         prBody,
		ChangedFiles: changedFiles,
	})
	if err != nil {
		return fmt.Errorf("checking for similar open pull requests: %w", err)
	}
	if existing == nil {
		return nil
	}
	return fmt.Errorf(
		"%w (%s, title %q). Do not open another one — retry this same write with pr_number=%d (or branch_name=%q) to commit the change onto that pull request",
		ErrDuplicateOpenPR,
		existing.URL,
		existing.Title,
		existing.Number,
		existing.HeadRef,
	)
}

// remember records a branch/PR as the repo's active one for this manager and
// the thread session behind it.
func (bm *BranchManager) remember(repoKey string, info *ActiveBranchInfo) {
	bm.activeBranches[repoKey] = info
	bm.syncToSession(repoKey, info)
}

// syncToSession persists a branch/PR on the thread session so follow-up
// messages can reuse it.
func (bm *BranchManager) syncToSession(repoKey string, info *ActiveBranchInfo) {
	if bm.session == nil {
		return
	}
	bm.session.mu.Lock()
	if bm.session.ActiveBranches == nil {
		bm.session.ActiveBranches = make(map[string]*ActiveBranchInfo)
	}
	bm.session.ActiveBranches[repoKey] = info
	bm.session.mu.Unlock()
	bm.session.Save()
}
