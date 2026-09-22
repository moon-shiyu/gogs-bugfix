package database

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/gogs/git-module"
)

// CheckRollupState is the aggregated state of required checks for a pull
// request at a specific point in time.
type CheckRollupState string

const (
	CheckRollupStateUnprotected CheckRollupState = "unprotected"
	CheckRollupStatePending     CheckRollupState = "pending"
	CheckRollupStateSuccess     CheckRollupState = "success"
	CheckRollupStateFailure     CheckRollupState = "failure"
)

// PullRequestCheckStatus is what the UI renders. It is computed from live data
// every time, never cached from a previous green run.
type PullRequestCheckStatus struct {
	State CheckRollupState
	// Required is the full list of configured check names.
	Required []string
	// Missing are required names without a valid result on this commit, base
	// tip and rule version.
	Missing []string
	// Pending are required names whose valid result is still in progress.
	Pending []string
	// Failed are required names whose valid result is failure or error.
	Failed []string
	// Checks are all valid checks for the current commit, including optional
	// ones, so the UI can show full detail.
	Checks []*StatusCheck
	// HeadSHA and BaseSHA are the tips this status was computed against.
	HeadSHA string
	BaseSHA string
	// RuleVersion is the rule version this status was computed against.
	RuleVersion int64
	// StaleReason explains why an older result can not be used.
	StaleReason string
}

// PullRequestMergeReadiness computes the live merge readiness of a pull
// request against the current protected branch rules, head commit and target
// branch tip.
func PullRequestMergeReadiness(pr *PullRequest, baseGitRepo *git.Repository) (*PullRequestCheckStatus, error) {
	if err := pr.LoadAttributes(); err != nil {
		return nil, errors.Wrap(err, "load attributes")
	}

	// Merged pull requests, deleted head repositories and closed pull
	// requests do not need a live readiness computation.
	if pr.HasMerged || pr.Issue != nil && pr.Issue.IsClosed || pr.HeadRepo == nil {
		return &PullRequestCheckStatus{State: CheckRollupStateUnprotected}, nil
	}

	headRepoPath := RepoPath(pr.HeadUserName, pr.HeadRepo.Name)
	headGitRepo, err := git.Open(headRepoPath)
	if err != nil {
		// The head repository may have been deleted, the pull request view
		// already handles this as broken fork data. Report unprotected and let
		// the existing broken-state UI take over.
		return &PullRequestCheckStatus{State: CheckRollupStateUnprotected}, nil
	}
	headSHA, err := headGitRepo.BranchCommitID(pr.HeadBranch)
	if err != nil {
		return &PullRequestCheckStatus{State: CheckRollupStateUnprotected}, nil
	}
	baseSHA, err := baseGitRepo.BranchCommitID(pr.BaseBranch)
	if err != nil {
		return nil, errors.Wrap(err, "get base commit")
	}
	headSHA = strings.ToLower(headSHA)
	baseSHA = strings.ToLower(baseSHA)

	status := &PullRequestCheckStatus{
		State:   CheckRollupStateUnprotected,
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
	}

	protectBranch, err := GetProtectBranchOfRepoByName(pr.BaseRepoID, pr.BaseBranch)
	if err != nil && !IsErrBranchNotExist(err) {
		return nil, errors.Wrap(err, "get protected branch")
	}
	if protectBranch == nil || !protectBranch.Protected {
		return status, nil
	}

	status.RuleVersion = protectBranch.RuleVersion
	status.Required = protectBranch.RequiredStatusCheckNames()
	if len(status.Required) == 0 {
		// Protected without required checks, mergeability is still gated by the
		// conflict check status.
		status.State = CheckRollupStateSuccess
	}

	allChecks, err := GetStatusChecksByCommit(pr.BaseRepoID, headSHA)
	if err != nil {
		return nil, errors.Wrap(err, "get status checks")
	}

	validChecks, hasStale := filterCurrentChecks(allChecks, baseSHA, protectBranch.RuleVersion)
	applyCheckRollup(status, validChecks, hasStale, pr.Status)
	return status, nil
}

// filterCurrentChecks keeps checks reported against exactly the current base
// tip and rule version. Anything else signals that a re-check is in flight.
func filterCurrentChecks(checks []*StatusCheck, baseSHA string, ruleVersion int64) ([]*StatusCheck, bool) {
	validChecks := make([]*StatusCheck, 0, len(checks))
	hasStale := false
	for _, check := range checks {
		if check.BaseSHA != baseSHA || check.RuleVersion != ruleVersion {
			hasStale = true
			continue
		}
		validChecks = append(validChecks, check)
	}
	return validChecks, hasStale
}

// applyCheckRollup fills the aggregated UI state from live check results.
func applyCheckRollup(status *PullRequestCheckStatus, validChecks []*StatusCheck, hasStale bool, prStatus PullRequestStatus) {
	status.Checks = validChecks
	if hasStale {
		status.StaleReason = "Previous check results were recorded against an older target branch or protection rules and are being re-evaluated."
	}

	if len(status.Required) == 0 {
		status.State = CheckRollupStateSuccess
		return
	}

	evalErr := evaluateRequiredChecks(status.Required, validChecks)
	if evalErr == nil {
		status.State = CheckRollupStateSuccess
		return
	}
	missing, ok := evalErr.(ErrRequiredChecksMissing)
	if !ok {
		status.State = CheckRollupStatePending
		return
	}
	status.Missing = missing.Missing
	status.Pending = missing.Pending
	status.Failed = missing.Failed
	switch {
	case len(missing.Failed) > 0 && !hasStale:
		status.State = CheckRollupStateFailure
	default:
		// Missing or pending results, an in-progress conflict test, or stale
		// history all mean waiting, never reuse the previous green state.
		_ = prStatus
		status.State = CheckRollupStatePending
	}
}

// ErrStateNeedsResync is returned when persisted state disagrees with the
// repository on disk, typically after a database backup restore. The caller
// must surface this to the user instead of silently choosing one side.
type ErrStateNeedsResync struct {
	args map[string]any
}

func (err ErrStateNeedsResync) Error() string {
	return "stored state does not match the repository on disk, manual resynchronization is required: " + formatArgs(err.args)
}

// Conflict marks the error as an HTTP 409 style conflict for handlers.
func (ErrStateNeedsResync) Conflict() bool {
	return true
}

// IsErrStateNeedsResync returns true when the error is a resync conflict.
func IsErrStateNeedsResync(err error) bool {
	_, ok := err.(ErrStateNeedsResync)
	return ok
}

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return "unknown mismatch"
	}
	parts := make([]string, 0, len(args))
	for key, value := range args {
		parts = append(parts, key+"="+toString(value))
	}
	return strings.Join(parts, ", ")
}

func toString(value any) string {
	return fmt.Sprint(value)
}

// VerifyPullRequestConsistency cross-checks the persisted pull request state
// against the repository on disk. A mismatch after a backup restore is
// reported as ErrStateNeedsResync instead of being silently repaired.
func VerifyPullRequestConsistency(pr *PullRequest, baseGitRepo *git.Repository) error {
	if err := pr.LoadAttributes(); err != nil {
		return errors.Wrap(err, "load attributes")
	}

	if pr.HasMerged {
		// The recorded merge commit must exist in the target branch history.
		if pr.MergedCommitID == "" {
			return ErrStateNeedsResync{args: map[string]any{
				"reason":       "pull request is marked merged but has no merge commit",
				"pull_request": pr.ID,
			}}
		}
		if !baseGitRepo.HasBranch(pr.BaseBranch) {
			// The target branch was deleted after the merge, nothing to
			// reconcile against.
			return nil
		}
		baseCommit, err := baseGitRepo.BranchCommit(pr.BaseBranch)
		if err != nil {
			return errors.Wrap(err, "get base branch commit")
		}
		if _, err = baseGitRepo.CatFileCommit(pr.MergedCommitID); err != nil {
			return ErrStateNeedsResync{args: map[string]any{
				"reason":           "recorded merge commit is missing from the repository on disk",
				"pull_request":     pr.ID,
				"merged_commit_id": pr.MergedCommitID,
				"base_branch_tip":  baseCommit.ID.String(),
			}}
		}
	}

	// A pending merge request whose pull request is open and whose recorded
	// base tip no longer matches the branch is recoverable, but a terminal
	// merge request with contradictory HasMerged flag is not.
	if pr.HasMerged {
		return nil
	}
	pendingMerges, err := getTerminalMergeRequests(pr.ID)
	if err != nil {
		return errors.Wrap(err, "get merge requests")
	}
	for _, merge := range pendingMerges {
		if merge.State == MergeRequestStateMerged && merge.MergedCommitID != "" {
			return ErrStateNeedsResync{args: map[string]any{
				"reason":           "a completed merge request exists but the pull request is still open",
				"pull_request":     pr.ID,
				"merge_request":    merge.ID,
				"merged_commit_id": merge.MergedCommitID,
			}}
		}
	}
	return nil
}

func getTerminalMergeRequests(prID int64) ([]*MergeRequest, error) {
	merges := make([]*MergeRequest, 0)
	err := x.Where("pull_request_id = ?", prID).Find(&merges)
	if err != nil {
		return nil, errors.Wrap(err, "find merge requests")
	}
	return merges, nil
}

// CreateStatusCheckInput is the payload of a check report from CI.
type CreateStatusCheckInput struct {
	Name        string
	State       string
	TargetURL   string
	Description string
}

// RecordStatusCheck records a check result for a head commit against the live
// target branch tip and rule version. It is the background entry point shared
// by check systems so results always bind to the history they ran on.
func RecordStatusCheck(repoID int64, baseBranch, headCommitSHA string, input CreateStatusCheckInput) (*StatusCheck, error) {
	repo, err := GetRepositoryByID(repoID)
	if err != nil {
		return nil, errors.Wrap(err, "get repository")
	}
	gitRepo, err := git.Open(repo.RepoPath())
	if err != nil {
		return nil, errors.Wrap(err, "open repository")
	}
	baseSHA, err := gitRepo.BranchCommitID(baseBranch)
	if err != nil {
		return nil, errors.Wrap(err, "get base branch tip")
	}

	var ruleVersion int64
	protectBranch, err := GetProtectBranchOfRepoByName(repoID, baseBranch)
	if err != nil && !IsErrBranchNotExist(err) {
		return nil, errors.Wrap(err, "get protected branch")
	}
	if protectBranch != nil {
		ruleVersion = protectBranch.RuleVersion
	}

	check := &StatusCheck{
		RepoID:      repoID,
		CommitSHA:   headCommitSHA,
		BaseSHA:     baseSHA,
		RuleVersion: ruleVersion,
		Name:        input.Name,
		State:       StatusCheckState(input.State),
		TargetURL:   input.TargetURL,
		Description: input.Description,
	}
	if err = CreateOrUpdateStatusCheck(check); err != nil {
		return nil, errors.Wrap(err, "save status check")
	}
	return check, nil
}
