package database

import (
	"context"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/gogs/git-module"
	log "unknwon.dev/clog/v2"
)

// MergePullRequestOptions carries an idempotent merge request.
type MergePullRequestOptions struct {
	PullRequest       *PullRequest
	Doer              *User
	BaseGitRepo       *git.Repository
	MergeStyle        MergeStyle
	CommitDescription string
}

// MergeResult is returned by the guarded merge entry point.
type MergeResult struct {
	Merged        bool
	AlreadyMerged bool
	InProgress    bool
	MergeRequest  *MergeRequest
}

// IsSuccessful reports whether the request observed a successful merge (this
// call merged or a previous identical call already did).
func (r *MergeResult) IsSuccessful() bool {
	return r.Merged || r.AlreadyMerged
}

// MergePullRequestIdempotent is the single guarded entry point shared by the
// web handler and background jobs. It enforces that:
//
//   - Protection rules and required checks are re-read at merge time and must
//     all pass against the exact current head commit.
//   - The target branch tip is the same one the checks were based on, a push
//     in between forces the pull request back to waiting.
//   - Only one attempt can write the target branch. Duplicate clicks, retried
//     API calls and notification retries converge to the same single result.
func MergePullRequestIdempotent(ctx context.Context, opts MergePullRequestOptions) (*MergeResult, error) {
	pr := opts.PullRequest
	if pr == nil {
		return nil, errors.New("pull request is required")
	}
	if err := pr.LoadIssue(); err != nil {
		return nil, errors.Wrap(err, "load issue")
	}
	if err := pr.LoadAttributes(); err != nil {
		return nil, errors.Wrap(err, "load attributes")
	}
	if pr.HasMerged {
		return &MergeResult{AlreadyMerged: true}, nil
	}

	guard, err := evaluateMergeGuard(ctx, pr, opts.BaseGitRepo)
	if err != nil {
		return nil, err
	}

	// Atomically claim the idempotency slot for (PR, head, base).
	claim, err := claimMergeRequest(ctx, pr, guard, opts)
	if err != nil {
		return nil, err
	}

	switch claim.State {
	case MergeRequestStateMerged:
		return &MergeResult{AlreadyMerged: true, MergeRequest: claim.MergeRequest}, nil
	case MergeRequestStatePending:
		// Either we just claimed it, or an identical request is still running.
		if !claim.held {
			return &MergeResult{InProgress: true, MergeRequest: claim.MergeRequest}, nil
		}
	case MergeRequestStateFailed:
		if !claim.retryable {
			// A deterministic prior failure (e.g. conflict, rules changed) is
			// returned as a conflict so the client refreshes instead of
			// silently replaying a second write.
			return nil, ErrMergeRequestChanged{args: map[string]any{
				"reason":        claim.FailureReason,
				"merge_request": claim.ID,
				"pull_request":  pr.ID,
			}}
		}
		// Interrupted merge on unchanged tips, reset the claim and retry once.
		if err = resetMergeRequestForRetry(claim.MergeRequest); err != nil {
			return nil, err
		}
	}

	// We hold the claim. Re-verify under no long-running lock but after
	// capturing both tips, then perform the git write.
	result := &MergeResult{MergeRequest: claim.MergeRequest}
	mergedCommitID, err := pr.executeMerge(opts.Doer, opts.BaseGitRepo, opts.MergeStyle, opts.CommitDescription, guard)
	if err != nil {
		_ = failMergeRequest(claim.MergeRequest, err)
		return nil, err
	}

	if err = completeMergeRequest(ctx, claim.MergeRequest, pr, opts.Doer, mergedCommitID); err != nil {
		log.Error("Failed to finalize merge request %d: %v", claim.MergeRequest.ID, err)
		return nil, err
	}
	result.Merged = true

	// Notifications are dispatched exactly once, guarded by the Notified flag.
	if err = dispatchMergeNotificationsOnce(ctx, claim.MergeRequest.ID, opts.Doer, pr, mergedCommitID, guard); err != nil {
		log.Error("Failed to dispatch merge notifications for pull request %d: %v", pr.ID, err)
	}
	return result, nil
}

type mergeGuard struct {
	protectBranch *ProtectBranch
	headSHA       string
	baseSHA       string
}

// evaluateMergeGuard re-reads rules and commits at merge time. It never uses
// stale check results from an older head commit.
func evaluateMergeGuard(ctx context.Context, pr *PullRequest, baseGitRepo *git.Repository) (*mergeGuard, error) {
	headRepoPath := RepoPath(pr.HeadUserName, pr.HeadRepo.Name)
	headGitRepo, err := git.Open(headRepoPath)
	if err != nil {
		return nil, errors.Newf("open head repository: %v", err)
	}
	headSHA, err := headGitRepo.BranchCommitID(pr.HeadBranch)
	if err != nil {
		return nil, errors.Newf("get head branch %q commit ID: %v", pr.HeadBranch, err)
	}
	headSHA = strings.ToLower(headSHA)

	baseSHA, err := baseGitRepo.BranchCommitID(pr.BaseBranch)
	if err != nil {
		return nil, errors.Newf("get base branch %q commit ID: %v", pr.BaseBranch, err)
	}
	baseSHA = strings.ToLower(baseSHA)

	guard := &mergeGuard{headSHA: headSHA, baseSHA: baseSHA}

	// The conflict test result applies to both protected and unprotected
	// branches, keep the historical CanAutoMerge gate for normal fast-forward
	// merges.
	if pr.Status == PullRequestStatusChecking {
		return nil, ErrMergeRequestChanged{args: map[string]any{
			"reason":       "pull request is still checking for conflicts",
			"pull_request": pr.ID,
		}}
	}
	if pr.Status == PullRequestStatusConflict {
		return nil, ErrMergeRequestChanged{args: map[string]any{
			"reason":       "pull request has merge conflicts",
			"pull_request": pr.ID,
		}}
	}

	protectBranch, err := GetProtectBranchOfRepoByName(pr.BaseRepoID, pr.BaseBranch)
	if err != nil && !IsErrBranchNotExist(err) {
		return nil, errors.Wrap(err, "get protected branch")
	}
	if protectBranch != nil && protectBranch.Protected {
		guard.protectBranch = protectBranch

		required := protectBranch.RequiredStatusCheckNames()
		if len(required) > 0 {
			checks, err := GetStatusChecksByCommit(pr.BaseRepoID, headSHA)
			if err != nil {
				return nil, errors.Wrap(err, "get status checks")
			}
			// Only results reported against the current base tip and the
			// current rule version can authorize the merge. Everything else is
			// treated as missing, the page shows waiting for a re-check.
			current := make([]*StatusCheck, 0, len(checks))
			for _, check := range checks {
				if check.BaseSHA == baseSHA && check.RuleVersion == protectBranch.RuleVersion {
					current = append(current, check)
				}
			}
			if err = evaluateRequiredChecks(required, current); err != nil {
				return nil, err
			}
		}
	}

	return guard, nil
}

// executeMerge performs the actual git merge and pushes to the target branch.
// The push is refused when the remote tip is not the base SHA captured by the
// guard, which prevents two merges from overwriting each other.
func (pr *PullRequest) executeMerge(doer *User, baseGitRepo *git.Repository, mergeStyle MergeStyle, commitDescription string, guard *mergeGuard) (string, error) {
	return pr.Merge(doer, baseGitRepo, mergeStyle, commitDescription, mergeExecutionOptions{
		expectedBaseSHA: guard.baseSHA,
	})
}
