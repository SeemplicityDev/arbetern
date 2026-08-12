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
// tools. It ensures that multiple file changes targeting the same repository
// within a single handler execution (or thread session) are grouped into a
// single pull request.
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

// ReadBranch returns the branch to read files from — the active branch if one
// exists for this repo, otherwise the base branch.
func (bm *BranchManager) ReadBranch(ctx context.Context, owner, repo, baseBranch string) string {
	if active := bm.resolveActiveBranch(ctx, owner, repo); active != nil {
		return active.BranchName
	}
	return baseBranch
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
	defer bm.session.mu.Unlock()
	delete(bm.session.ActiveBranches, repoKey)
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
// "<agentID>: <description>" PR title. Both are only consulted when a new
// branch/PR is being created for this repo in the current run; subsequent
// write calls for the same repo group into the existing PR and the
// overrides are ignored.
func (bm *BranchManager) CommitAndPR(
	ctx context.Context,
	owner, repo, baseBranch, userID, description, prBody, branchOverride, prTitleOverride string,
	changedFiles []string,
	commitFn func(branch string) error,
) (*CommitResult, error) {
	repoKey := owner + "/" + repo

	if active := bm.resolveActiveBranch(ctx, owner, repo); active != nil {
		// Commit to existing branch.
		err := commitFn(active.BranchName)
		switch {
		case err == nil:
			log.Printf("[branch-manager] additional commit to branch %s for PR: %s", active.BranchName, active.PrURL)
			return &CommitResult{PrURL: active.PrURL, IsNew: false, Message: active.PrURL}, nil
		case github.IsMissingBranchError(err):
			// Deleted between the existence check and the commit — fall through
			// to the new branch/PR path rather than failing the whole request.
			log.Printf("[branch-manager] branch %s vanished mid-commit; falling back to a new branch", active.BranchName)
			bm.forget(repoKey)
		default:
			return nil, fmt.Errorf("committing to existing branch: %w", err)
		}
	}

	// Last line of defence before a PR is opened: with both the override and the
	// description empty the title degrades to a bare "<agentID>:" and the body
	// to a template with nothing filled in. Callers validate their own arguments
	// (see requireDescription); this makes the failure impossible at the choke
	// point rather than relying on every future caller to remember.
	prTitle := strings.TrimSpace(prTitleOverride)
	if prTitle == "" {
		if strings.TrimSpace(description) == "" {
			return nil, fmt.Errorf("refusing to open a pull request with no title: both pr_title and description are empty")
		}
		prTitle = fmt.Sprintf("%s: %s", bm.agentID, description)
	}
	if existing, err := bm.ghClient.FindSimilarOpenPullRequest(ctx, owner, repo, baseBranch, github.PRDuplicateCandidate{
		Title:        prTitle,
		Body:         prBody,
		ChangedFiles: changedFiles,
	}); err != nil {
		return nil, fmt.Errorf("checking for similar open pull requests: %w", err)
	} else if existing != nil {
		return nil, fmt.Errorf(
			"%w (%s, title %q). Reuse that PR instead of opening a new one",
			ErrDuplicateOpenPR,
			existing.URL,
			existing.Title,
		)
	}

	// Create a new branch, commit, and open a PR.
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

	info := &ActiveBranchInfo{
		BranchName: branchName,
		BaseBranch: baseBranch,
		PrURL:      prURL,
	}
	bm.activeBranches[repoKey] = info
	bm.syncToSession(repoKey, info)

	log.Printf("[branch-manager] PR created on branch %s: %s", branchName, prURL)
	return &CommitResult{PrURL: prURL, IsNew: true, Message: prURL}, nil
}

// syncToSession persists a branch/PR on the thread session so follow-up
// messages can reuse it.
func (bm *BranchManager) syncToSession(repoKey string, info *ActiveBranchInfo) {
	if bm.session == nil {
		return
	}
	bm.session.mu.Lock()
	defer bm.session.mu.Unlock()
	if bm.session.ActiveBranches == nil {
		bm.session.ActiveBranches = make(map[string]*ActiveBranchInfo)
	}
	bm.session.ActiveBranches[repoKey] = info
}
